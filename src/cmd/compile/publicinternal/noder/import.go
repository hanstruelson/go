// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"errors"
	"fmt"
	"publicinternal/buildcfg"
	"publicinternal/exportdata"
	"publicinternal/pkgbits"
	"os"
	pathpkg "path"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/testimporter"
	"cmd/compile/publicinternal/types"
	"cmd/compile/publicinternal/types2"
	"cmd/publicinternal/bio"
	"cmd/publicinternal/goobj"
	"cmd/publicinternal/objabi"
)

type gcimports struct {
	compileCtx *Context
	ctxt       *types2.Context
	packages   map[string]*types2.Package
}

func (m *gcimports) Import(path string) (*types2.Package, error) {
	return m.ImportFrom(path, "" /* no vendoring */, 0)
}

func (m *gcimports) ImportFrom(path, srcDir string, mode types2.ImportMode) (*types2.Package, error) {
	if mode != 0 {
		panic("mode must be 0")
	}

	_, pkg, err := readImportFile(m.compileCtx, path, m.compileCtx.Target, m.ctxt, m.packages)
	return pkg, err
}

func isDriveLetter(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// is this path a local name? begins with ./ or ../ or /
func islocalname(name string) bool {
	return strings.HasPrefix(name, "/") ||
		runtime.GOOS == "windows" && len(name) >= 3 && isDriveLetter(name[0]) && name[1] == ':' && name[2] == '/' ||
		strings.HasPrefix(name, "./") || name == "." ||
		strings.HasPrefix(name, "../") || name == ".."
}

func openPackage(target *ir.Package, path string) (*os.File, error) {
	if islocalname(path) {
		if target.Flags.NoLocalImports {
			return nil, errors.New("local imports disallowed")
		}

		if target.Flags.Cfg.PackageFile != nil {
			return os.Open(target.Flags.Cfg.PackageFile[path])
		}

		// try .a before .o.  important for building libraries:
		// if there is an array.o in the array.a library,
		// want to find all of array.a, not just array.o.
		if file, err := os.Open(fmt.Sprintf("%s.a", path)); err == nil {
			return file, nil
		}
		if file, err := os.Open(fmt.Sprintf("%s.o", path)); err == nil {
			return file, nil
		}
		return nil, errors.New("file not found")
	}

	// local imports should be canonicalized already.
	// don't want to see "encoding/../encoding/base64"
	// as different from "encoding/base64".
	if q := pathpkg.Clean(path); q != path {
		return nil, fmt.Errorf("non-canonical import path %q (should be %q)", path, q)
	}

	if target.Flags.Cfg.PackageFile != nil {
		return os.Open(target.Flags.Cfg.PackageFile[path])
	}

	for _, dir := range target.Flags.Cfg.ImportDirs {
		if file, err := os.Open(fmt.Sprintf("%s/%s.a", dir, path)); err == nil {
			return file, nil
		}
		if file, err := os.Open(fmt.Sprintf("%s/%s.o", dir, path)); err == nil {
			return file, nil
		}
	}

	if buildcfg.GOROOT != "" {
		suffix := ""
		if target.Flags.InstallSuffix != "" {
			suffix = "_" + target.Flags.InstallSuffix
		} else if target.Flags.Race {
			suffix = "_race"
		} else if target.Flags.MSan {
			suffix = "_msan"
		} else if target.Flags.ASan {
			suffix = "_asan"
		}

		if file, err := os.Open(fmt.Sprintf("%s/pkg/%s_%s%s/%s.a", buildcfg.GOROOT, buildcfg.GOOS, buildcfg.GOARCH, suffix, path)); err == nil {
			return file, nil
		}
		if file, err := os.Open(fmt.Sprintf("%s/pkg/%s_%s%s/%s.o", buildcfg.GOROOT, buildcfg.GOOS, buildcfg.GOARCH, suffix, path)); err == nil {
			return file, nil
		}
	}
	return nil, errors.New("file not found")
}

// resolveImportPath resolves an import path as it appears in a Go
// source file to the package's full path.
func resolveImportPath(target *ir.Package, path string) (string, error) {
	// The package name main is no longer reserved,
	// but we reserve the import path "main" to identify
	// the main package, just as we reserve the import
	// path "math" to identify the standard math package.
	if path == "main" {
		return "", errors.New("cannot import \"main\"")
	}

	if target.Ctxt.Pkgpath == "" {
		panic("missing pkgpath")
	}
	if path == target.Ctxt.Pkgpath {
		return "", fmt.Errorf("import %q while compiling that package (import cycle)", path)
	}

	if mapped, ok := target.Flags.Cfg.ImportMap[path]; ok {
		path = mapped
	}

	if islocalname(path) {
		if path[0] == '/' {
			return "", errors.New("import path cannot be absolute path")
		}

		prefix := target.Flags.D
		if prefix == "" {
			// Questionable, but when -D isn't specified, historically we
			// resolve local import paths relative to the directory the
			// compiler's current directory, not the respective source
			// file's directory.
			prefix = target.Ctxt.Pathname
		}
		path = pathpkg.Join(prefix, path)

		if err := checkImportPath(path, true); err != nil {
			return "", err
		}
	}

	return path, nil
}

// readImportFile reads the import file for the given package path and
// returns its types.Pkg representation. If packages is non-nil, the
// types2.Package representation is also returned.
func readImportFile(compileCtx *Context, path string, target *ir.Package, env *types2.Context, packages map[string]*types2.Package) (pkg1 *types.Pkg, pkg2 *types2.Package, err error) {
	path, err = resolveImportPath(target, path)
	if err != nil {
		return
	}

	if path == "unsafe" {
		pkg1, pkg2 = target.UnsafePkg, types2.Unsafe

		// TODO(mdempsky): Investigate if this actually matters. Why would
		// the linker or runtime care whether a package imported unsafe?
		imported := false
		for _, pkg := range target.Imports {
			if pkg == pkg1 {
				imported = true
				break
			}
		}
		if !imported {
			target.Imports = append(target.Imports, pkg1)
		}
		pkg1.Direct = true

		return
	}

	pkg1 = target.Pkg(path, "")
	if packages != nil {
		pkg2 = packages[path]
		if !(pkg1.Direct == (pkg2 != nil && pkg2.Complete())) {

			base.Fatalf("pkg1.Direct == (pkg2 != nil && pkg2.Complete()), path=%s\npackages=%v", path, packages)
		}
	}

	if pkg1.Direct {
		return
	}
	pkg1.Direct = true
	target.Imports = append(target.Imports, pkg1)

	f, err := openPackage(target, path)
	if err != nil {
		return
	}
	defer f.Close()

	data, err := readExportData(f)
	if err != nil {
		return
	}

	if compileCtx.Target.Debug.Export != 0 {
		fmt.Printf("importing %s (%s)\n", path, f.Name())
	}

	pr := pkgbits.NewPkgDecoder(pkg1.Path, data)

	// Read package descriptors for both types2 and compiler backend.
	readPackage(newPkgReader(compileCtx, pr), pkg1, false)
	pkg2 = testimporter.ReadPackage(env, packages, pr)

	err = addFingerprint(target, path, data)
	return
}

// readExportData returns the contents of GC-created unified export data.
func readExportData(f *os.File) (data string, err error) {
	r := bio.NewReader(f)

	sz, err := exportdata.FindPackageDefinition(r.Reader)
	if err != nil {
		return
	}
	end := r.Offset() + int64(sz)

	abihdr, _, err := exportdata.ReadObjectHeaders(r.Reader)
	if err != nil {
		return
	}

	if expect := objabi.HeaderString(); abihdr != expect {
		err = fmt.Errorf("object is [%s] expected [%s]", abihdr, expect)
		return
	}

	_, err = exportdata.ReadExportDataHeader(r.Reader, true)
	if err != nil {
		return
	}

	pos := r.Offset()

	// Map export data section (+ end-of-section marker) into memory
	// as a single large string. This reduces heap fragmentation and
	// allows returning individual substrings very efficiently.
	var mapped string
	mapped, err = base.MapFile(r.File(), pos, end-pos)
	if err != nil {
		return
	}

	// check for end-of-section marker "\n$$\n" and remove it
	const marker = "\n$$\n"

	var ok bool
	data, ok = strings.CutSuffix(mapped, marker)
	if !ok {
		cutoff := data // include last 10 bytes in error message
		if len(cutoff) >= 10 {
			cutoff = cutoff[len(cutoff)-10:]
		}
		err = fmt.Errorf("expected $$ marker, but found %q (recompile package)", cutoff)
		return
	}

	return
}

// addFingerprint reads the linker fingerprint included at the end of
// the exportdata.
func addFingerprint(target *ir.Package, path string, data string) error {
	var fingerprint goobj.FingerprintType

	pos := len(data) - len(fingerprint)
	if pos < 0 {
		return fmt.Errorf("missing linker fingerprint in exportdata, but found %q", data)
	}
	buf := []byte(data[pos:])

	copy(fingerprint[:], buf)
	target.Ctxt.AddImport(path, fingerprint)

	return nil
}

func checkImportPath(path string, allowSpace bool) error {
	if path == "" {
		return errors.New("import path is empty")
	}

	if strings.Contains(path, "\x00") {
		return errors.New("import path contains NUL")
	}

	for ri := range base.ReservedImports {
		if path == ri {
			return fmt.Errorf("import path %q is reserved and cannot be used", path)
		}
	}

	for _, r := range path {
		switch {
		case r == utf8.RuneError:
			return fmt.Errorf("import path contains invalid UTF-8 sequence: %q", path)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("import path contains control character: %q", path)
		case r == '\\':
			return fmt.Errorf("import path contains backslash; use slash: %q", path)
		case !allowSpace && unicode.IsSpace(r):
			return fmt.Errorf("import path contains space character: %q", path)
		case strings.ContainsRune("!\"#$%&'()*,:;<=>?[]^`{|}", r):
			return fmt.Errorf("import path contains invalid character '%c': %q", r, path)
		}
	}

	return nil
}
