// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package jitbundle builds and appends the assets used by JIT-enabled Go
// executables.
package jitbundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Artifact identifies a prepared, compressed GOROOT payload.
type Artifact struct {
	Path    string
	ID      string
	GOOS    string
	GOARCH  string
	Version string
}

// ToolchainPackages returns the buildable library packages in GOROOT/src for
// ctxt. It includes internal standard library and command packages, while
// omitting command executables, vendor trees, and testdata.
func ToolchainPackages(ctxt build.Context, goroot string) ([]string, error) {
	src := filepath.Join(goroot, "src")
	var paths []string
	err := filepath.WalkDir(src, func(dir string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, dir)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skipToolchainTree(rel) {
			return filepath.SkipDir
		}
		if rel == "cmd" {
			return nil
		}
		pkg, err := ctxt.ImportDir(dir, 0)
		if err != nil {
			var noGo *build.NoGoError
			if errors.As(err, &noGo) {
				return nil
			}
			return fmt.Errorf("reading standard library package %s: %w", filepath.ToSlash(rel), err)
		}
		if pkg.Name == "main" || pkg.Name == "" || filepath.ToSlash(rel) == "unsafe" || filepath.ToSlash(rel) == "builtin" {
			return nil
		}
		if len(pkg.GoFiles)+len(pkg.CgoFiles) == 0 {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func skipToolchainTree(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, elem := range parts {
		if elem == "vendor" || elem == "testdata" {
			return true
		}
		if elem == "cmd" && i != 0 {
			return true
		}
	}
	return false
}

// PrepareGOROOT creates a compressed, self-contained copy of goroot. JIT
// bundles currently require the executable's target to match the host on
// which the Go tools in GOROOT were built.
func PrepareGOROOT(goroot, goos, goarch string) (Artifact, error) {
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		return Artifact{}, fmt.Errorf("-JIT requires a native build (target %s/%s, host %s/%s)", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	toolDir := filepath.Join(goroot, "pkg", "tool", runtime.GOOS+"_"+runtime.GOARCH)
	toolSuffix := ""
	if runtime.GOOS == "windows" {
		toolSuffix = ".exe"
	}
	for _, tool := range []string{"asm", "compile", "link"} {
		if _, err := os.Stat(filepath.Join(toolDir, tool+toolSuffix)); err != nil {
			return Artifact{}, fmt.Errorf("-JIT requires the installed Go toolchain at %s: %w", toolDir, err)
		}
	}

	f, err := os.CreateTemp("", "go-jit-goroot-*.tar.gz")
	if err != nil {
		return Artifact{}, err
	}
	path := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()

	h := sha256.New()
	compressed := io.MultiWriter(f, h)
	gz, err := gzip.NewWriterLevel(compressed, gzip.DefaultCompression)
	if err != nil {
		return Artifact{}, err
	}
	gz.Header.ModTime = time.Time{}
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	manifest, err := json.MarshalIndent(struct {
		Format  int    `json:"format"`
		Version string `json:"goVersion"`
		GOOS    string `json:"goos"`
		GOARCH  string `json:"goarch"`
	}{Format: 1, Version: runtime.Version(), GOOS: goos, GOARCH: goarch}, "", "  ")
	if err == nil {
		err = tw.WriteHeader(&tar.Header{Name: "gojit/manifest.json", Mode: 0644, Size: int64(len(manifest)), ModTime: time.Time{}, Typeflag: tar.TypeReg, Format: tar.FormatPAX})
	}
	if err == nil {
		_, err = tw.Write(manifest)
	}
	if err == nil {
		err = filepath.WalkDir(goroot, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(goroot, path)
			if err != nil {
				return err
			}
			if rel == ".git" || strings.HasPrefix(filepath.ToSlash(rel), ".git/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(filepath.Join("GOROOT", rel))
			var link string
			if info.Mode()&os.ModeSymlink != 0 {
				link, err = os.Readlink(path)
				if err != nil {
					return err
				}
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = name
			hdr.ModTime = time.Time{}
			hdr.AccessTime = time.Time{}
			hdr.ChangeTime = time.Time{}
			hdr.Format = tar.FormatPAX
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, file)
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		})
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return Artifact{}, err
	}
	ok = true
	return Artifact{Path: path, ID: hex.EncodeToString(h.Sum(nil)), GOOS: goos, GOARCH: goarch, Version: runtime.Version()}, nil
}

var footerMagic = [16]byte{'G', 'O', '-', 'J', 'I', 'T', '-', 'B', 'U', 'N', 'D', 'L', 'E', '-', 'V', '1'}

// PackageFile is a compiled package archive to make available to a future JIT
// compiler. ImportPath is stored as a path below GOROOT/pkg/jit/stdlib.
type PackageFile struct {
	ImportPath string
	Path       string
}

// Append combines the GOROOT source/tool payload with compiled package
// archives, appends it to target, and writes a footer that lets a future
// runtime locate and validate the payload without an external GOROOT.
func Append(target string, artifact Artifact, packages []PackageFile) error {
	bundle, err := os.CreateTemp("", "go-jit-bundle-*.tar.gz")
	if err != nil {
		return err
	}
	bundlePath := bundle.Name()
	defer os.Remove(bundlePath)
	defer bundle.Close()

	h := sha256.New()
	gz, err := gzip.NewWriterLevel(io.MultiWriter(bundle, h), gzip.DefaultCompression)
	if err != nil {
		return err
	}
	gz.Header.ModTime = time.Time{}
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)

	manifest, err := json.MarshalIndent(struct {
		Format       int      `json:"format"`
		Version      string   `json:"goVersion"`
		GOOS         string   `json:"goos"`
		GOARCH       string   `json:"goarch"`
		GOROOTSHA256 string   `json:"gorootSHA256"`
		Packages     []string `json:"compiledPackages"`
	}{Format: 1, Version: artifact.Version, GOOS: artifact.GOOS, GOARCH: artifact.GOARCH, GOROOTSHA256: artifact.ID, Packages: packageNames(packages)}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeTarBytes(tw, "gojit/manifest.json", manifest, 0644); err != nil {
		return err
	}
	if err := writeTarFile(tw, "GOROOT.tar.gz", artifact.Path); err != nil {
		return err
	}
	for _, pkg := range sortedPackageFiles(packages) {
		name := "GOROOT/pkg/jit/stdlib/" + filepath.ToSlash(pkg.ImportPath) + ".a"
		if err := writeTarFile(tw, name, pkg.Path); err != nil {
			return fmt.Errorf("bundling compiled package %s: %w", pkg.ImportPath, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := bundle.Sync(); err != nil {
		return err
	}
	if err := bundle.Close(); err != nil {
		return err
	}
	digest := h.Sum(nil)

	payload, err := os.Open(bundlePath)
	if err != nil {
		return err
	}
	defer payload.Close()
	info, err := payload.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, payload); err != nil {
		output.Close()
		return err
	}
	var footer [56]byte
	copy(footer[:16], footerMagic[:])
	for i := 0; i < 8; i++ {
		footer[16+i] = byte(uint64(info.Size()) >> (8 * i))
	}
	copy(footer[24:], digest)
	if _, err := output.Write(footer[:]); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func packageNames(packages []PackageFile) []string {
	packages = sortedPackageFiles(packages)
	names := make([]string, 0, len(packages))
	for i, pkg := range packages {
		if i == 0 || pkg.ImportPath != packages[i-1].ImportPath {
			names = append(names, pkg.ImportPath)
		}
	}
	return names
}

func sortedPackageFiles(packages []PackageFile) []PackageFile {
	packages = append([]PackageFile(nil), packages...)
	// Keep the first archive for duplicate import paths. Link actions order
	// rebuilt-for-test packages before the corresponding standard packages.
	sort.SliceStable(packages, func(i, j int) bool {
		return packages[i].ImportPath < packages[j].ImportPath
	})
	out := packages[:0]
	for _, pkg := range packages {
		if len(out) == 0 || pkg.ImportPath != out[len(out)-1].ImportPath {
			out = append(out, pkg)
		}
	}
	return out
}

func writeTarBytes(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: time.Time{}, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeTarFile(tw *tar.Writer, name, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: time.Time{}, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}
