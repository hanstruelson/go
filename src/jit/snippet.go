// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"bytes"
	"cmd/publicinternal/archive"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"sync"
)

// Code is a mapped in-memory snippet registered with the Go runtime's PC and
// GC metadata tables for as long as it remains loaded.
type Code struct {
	mu      sync.Mutex
	cond    *sync.Cond
	image   *mappedImage
	active  int
	closing bool
	closed  bool
}

// LoadSnippet links a compiler object against the archives recorded in m and
// loads the resulting Go module into the process.
func (m *Metadata) LoadSnippet(compiled CompiledPackage) (*Code, error) {
	image, err := loadLinkedSnippet(m, compiled)
	if err != nil {
		return nil, err
	}
	code := &Code{image: image}
	code.cond = sync.NewCond(&code.mu)
	return code, nil
}

// Run calls the linked snippet through the host architecture's trampoline.
func (c *Code) Run() error {
	entry, done, err := c.beginRun()
	if err != nil {
		return err
	}
	defer done()
	callEntry(entry)
	return nil
}

// RunInt calls a func() int entry and returns its result.
func (c *Code) RunInt() (int, error) {
	entry, done, err := c.beginRun()
	if err != nil {
		return 0, err
	}
	defer done()
	return int(callEntryInt(entry)), nil
}

func (c *Code) beginRun() (uintptr, func(), error) {
	if c == nil {
		return 0, nil, fmt.Errorf("jit: code is not loaded")
	}
	c.mu.Lock()
	if c.closed || c.closing || c.image == nil || c.image.entry == 0 {
		c.mu.Unlock()
		return 0, nil, fmt.Errorf("jit: code is not loaded")
	}
	entry := c.image.entry
	c.active++
	c.mu.Unlock()
	return entry, func() {
		c.mu.Lock()
		c.active--
		if c.active == 0 && c.cond != nil {
			c.cond.Broadcast()
		}
		c.mu.Unlock()
	}, nil
}

// Close unregisters this generated module from Go runtime PC and GC tables,
// then releases its executable and data mappings. It waits for active Run
// calls to return before changing either mapping.
func (c *Code) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.cond == nil {
		c.cond = sync.NewCond(&c.mu)
	}
	for c.closing && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	for c.active != 0 {
		c.cond.Wait()
	}
	image := c.image
	c.mu.Unlock()

	err := closeMappedImage(image)
	c.mu.Lock()
	if err == nil {
		c.image = nil
		c.closed = true
	}
	c.closing = false
	c.cond.Broadcast()
	c.mu.Unlock()
	return err
}

// CompileSnippet parses, type-checks against the selected file's imports, and
// compiles statements as an exported Eval function. It gives the new package
// the host package name and file import scope, but the snippet is a separate
// package: host package declarations and locals are not available. Imported
// package symbols resolve to the objects already linked into the host process.
func (m *Metadata) CompileSnippet(fileName, source string) (CompiledPackage, error) {
	ctx, err := m.ContextForFile(fileName)
	if err != nil {
		return CompiledPackage{}, err
	}
	if _, err := ParseStatementsInContext(ctx, source); err != nil {
		return CompiledPackage{}, err
	}

	usedImports, err := snippetImports(source, ctx, m.Build.Packages)
	if err != nil {
		return CompiledPackage{}, err
	}
	var file strings.Builder
	fmt.Fprintf(&file, "package %s\n", ctx.PackageName)
	if len(usedImports) != 0 {
		file.WriteString("import (\n")
		for _, imp := range ctx.Imports {
			if !usedImports[imp.Path] {
				continue
			}
			if imp.Alias != "" {
				file.WriteString(imp.Alias)
				file.WriteByte(' ')
			}
			file.WriteString(strconv.Quote(imp.SourcePath))
			file.WriteByte('\n')
		}
		file.WriteString(")\n")
	}
	file.WriteString("func Eval() {\n")
	file.WriteString(source)
	file.WriteString("\n}\n")

	packageArchives := make(map[string][]byte, len(m.Build.Packages))
	for _, pkg := range m.Build.Packages {
		archiveData, err := compilerPackageArchive(pkg)
		if err != nil {
			continue
		}
		packageArchives[pkg.Path] = archiveData
	}
	for path, used := range usedImports {
		if used && len(packageArchives[path]) == 0 {
			return CompiledPackage{}, fmt.Errorf("jit: no embedded compiler archive for imported package %q", path)
		}
	}
	importMap := make(map[string]string, len(usedImports))
	for _, imp := range ctx.Imports {
		if usedImports[imp.Path] {
			importMap[imp.SourcePath] = imp.Path
		}
	}
	return Compile(CompileRequest{
		// Give each compiled module a stable, unique package path within the
		// host package's runtime namespace.
		PackagePath:     m.Build.PackagePath + "/jit/1",
		Files:           []SourceFile{{Name: "jit_eval.go", Data: file.String()}},
		PackageArchives: packageArchives,
		ImportMap:       importMap,
		Options:         []string{"-l=1"},
		// The custom in-memory linker consumes ordinary compiler object files;
		// it does not ask cmd/link to produce a plugin module.
	})
}

func snippetImports(source string, ctx PackageContext, packages []PackageExport) (map[string]bool, error) {
	fset := token.NewFileSet()
	wrapped := "package jit_snippet\nfunc _jit_snippet() {\n" + source + "\n}\n"
	file, err := parser.ParseFile(fset, "jit_snippet.go", wrapped, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("jit: parse snippet for import selection: %w", err)
	}
	packageNames := make(map[string]string, len(packages))
	for _, pkg := range packages {
		packageNames[pkg.Path] = pkg.Name
	}
	aliasPaths := make(map[string]string, len(ctx.Imports))
	used := make(map[string]bool)
	for _, imp := range ctx.Imports {
		if imp.Alias == "_" {
			continue
		}
		if imp.Alias == "." {
			used[imp.Path] = true
			continue
		}
		alias := imp.Alias
		if alias == "" {
			alias = packageNames[imp.Path]
		}
		if alias != "" {
			aliasPaths[alias] = imp.Path
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if ok {
			if path := aliasPaths[ident.Name]; path != "" {
				used[path] = true
			}
		}
		return true
	})
	return used, nil
}

// compilerPackageArchive returns a compiler-facing archive for the in-process
// compiler importer and the in-memory linker input.
func compilerPackageArchive(pkg PackageExport) ([]byte, error) {
	if len(pkg.Archive) != 0 {
		return pkg.Archive, nil
	}
	if pkg.ObjectHeader == "" || len(pkg.Data) == 0 {
		return nil, fmt.Errorf("jit: package %q has no compiler export payload", pkg.Path)
	}
	section := make([]byte, 0, len(pkg.ObjectHeader)+len(pkg.Data)+16)
	section = append(section, pkg.ObjectHeader...)
	section = append(section, "$$B\nu"...)
	section = append(section, pkg.Data...)
	section = append(section, "\n$$\n"...)
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	var header [archive.HeaderSize]byte
	archive.FormatHeader(header[:], "__.PKGDEF", int64(len(section)))
	out.Write(header[:])
	out.Write(section)
	if len(section)&1 != 0 {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
