// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/jitmeta"
	"cmd/compile/publicinternal/noder"
	"cmd/compile/publicinternal/testimporter"
	"cmd/compile/publicinternal/types2"
	"cmd/publicinternal/goobj"
	"publicinternal/buildcfg"
	"publicinternal/exportdata"
	"publicinternal/pkgbits"
)

const jitImportPath = "jit"

type jitLinkRef struct {
	symbol jitmeta.Symbol
	value  string
}

type jitSourceFile struct {
	source noder.SourceFile
	ast    *ast.File
	jit    bool
}

// commandJITSourceFiles returns the command source files augmented with a
// compiler-owned registration file when the package imports jit.
func commandJITSourceFiles(target *ir.Package, filenames []string) ([]noder.SourceFile, bool, error) {
	if target.Flags.Cfg.PackageFile != nil && !configuredJITImport(target) {
		return nil, false, nil
	}
	fset := token.NewFileSet()
	inputs := make([]jitSourceFile, 0, len(filenames))
	jitPath, jitLiteral := "", ""
	for _, filename := range filenames {
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, false, err
		}
		file, err := parser.ParseFile(fset, filename, data, parser.AllErrors)
		if err != nil {
			// Leave source diagnostics to the compiler's normal frontend.
			return nil, false, nil
		}
		input := jitSourceFile{
			source: noder.SourceFile{Name: filename, Data: string(data)},
			ast:    file,
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			alias := ""
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "_" {
				continue
			}
			resolved := resolveJITImportPath(target, importPath)
			if isJITImportPath(importPath, resolved) && jitPath == "" {
				jitPath, jitLiteral = resolved, importPath
			}
			if isJITImportPath(importPath, resolved) {
				input.jit = true
			}
		}
		inputs = append(inputs, input)
	}
	if jitPath == "" {
		return nil, false, nil
	}
	if target.Ctxt.Pkgpath == "" || target.Ctxt.Pkgpath == "_" {
		return nil, false, fmt.Errorf("cannot embed jit metadata without a package import path")
	}
	if len(inputs) == 0 {
		return nil, false, fmt.Errorf("cannot embed jit metadata in an empty package")
	}
	target.Flags.Cfg.JITImportFiles = make(map[string]bool)
	for _, input := range inputs {
		if input.jit {
			target.Flags.Cfg.JITImportFiles[input.source.Name] = true
		}
	}

	canonicalImports := make(map[string]string)
	evalImports := make(map[string]string)
	linkerImports := make(map[string]string)
	bundleFiles := make([]jitmeta.File, 0, len(inputs))
	for _, input := range inputs {
		entry := jitmeta.File{Name: input.source.Name}
		for _, spec := range input.ast.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, false, fmt.Errorf("%s: invalid import path: %w", input.source.Name, err)
			}
			resolved := resolveJITImportPath(target, importPath)
			alias := ""
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			entry.Imports = append(entry.Imports, jitmeta.Import{Alias: alias, SourcePath: importPath, Path: resolved})
			if resolved != "C" && resolved != "unsafe" {
				if _, ok := canonicalImports[resolved]; !ok {
					canonicalImports[resolved] = importPath
				}
				// Package declarations and generic bodies can refer to imports
				// from their own source files even when Eval is called from a
				// different file. Retain those packages for late instantiation;
				// the per-file BuildMetadata still controls which aliases Eval
				// can name directly.
				if alias != "_" {
					if _, ok := linkerImports[resolved]; !ok {
						linkerImports[resolved] = importPath
					}
				}
				if input.jit && alias != "_" {
					if _, ok := evalImports[resolved]; !ok {
						evalImports[resolved] = importPath
					}
				}
			}
		}
		bundleFiles = append(bundleFiles, entry)
	}

	importPaths := make([]string, 0, len(canonicalImports))
	for importPath := range canonicalImports {
		importPaths = append(importPaths, importPath)
	}
	sort.Strings(importPaths)
	packageTypes := make(map[string]*types2.Package, len(importPaths))
	packageAliases := make(map[string]string, len(importPaths))
	packageExports := make(map[string]jitmeta.PackageExport)
	for _, importPath := range importPaths {
		// Capture the exact export closure for every package visible to source
		// files in this package. Eval can instantiate imported generic bodies,
		// whose type references may load transitive package exports.
		if err := collectPackageExports(target, importPath, packageExports, true); err != nil {
			return nil, false, err
		}
		pkg, err := readJITTypes(importPath, packageExports[importPath].Data)
		if err != nil {
			return nil, false, fmt.Errorf("read exported declarations for imported package %q: %w", importPath, err)
		}
		packageTypes[importPath] = pkg
	}
	if !isJITRuntimePackage(packageTypes[jitPath]) {
		return nil, false, nil
	}

	prefix := "__go_jit_meta"
	original := make([]noder.SourceFile, len(inputs))
	for i, input := range inputs {
		original[i] = input.source
	}
	for i := 0; sourceHasIdentifier(original, prefix); i++ {
		prefix = fmt.Sprintf("__go_jit_meta_%d", i+1)
	}
	for i, importPath := range importPaths {
		packageAliases[importPath] = fmt.Sprintf("%s_pkg%d", prefix, i)
	}
	runtimeAlias := packageAliases[jitPath]
	if runtimeAlias == "" {
		return nil, false, fmt.Errorf("jit package %q is not in the host import configuration", jitPath)
	}
	jitScopePaths := make([]string, 0, len(linkerImports))
	for importPath := range linkerImports {
		jitScopePaths = append(jitScopePaths, importPath)
	}
	sort.Strings(jitScopePaths)
	// Carry package roots separately from generated reflection references.
	// Eval can name imports from its own file; instantiated package declarations
	// can also use imports from their declaration files. The runtime metadata
	// keeps those scopes distinct even though the linker retention set is a
	// package-wide union.
	target.Ctxt.JITScope = make([]goobj.ImportedPkg, len(jitScopePaths))
	for i, importPath := range jitScopePaths {
		target.Ctxt.JITScope[i].Pkg = importPath
	}
	registrationImportPaths := append([]string(nil), jitScopePaths...)
	hasJITRegistrationImport := false
	for _, importPath := range registrationImportPaths {
		if importPath == jitPath {
			hasJITRegistrationImport = true
			break
		}
	}
	if !hasJITRegistrationImport {
		registrationImportPaths = append(registrationImportPaths, jitPath)
		sort.Strings(registrationImportPaths)
	}

	var refs []jitLinkRef
	var declarations []jitmeta.Declaration
	localConstraints := collectLocalJITConstraints(inputs, target, packageTypes)
	for _, input := range inputs {
		fileRefs, fileDecls := collectLocalJITSymbols(target.Ctxt.Pkgpath, fset, input.ast, input.source.Name, localConstraints)
		refs = append(refs, fileRefs...)
		declarations = append(declarations, fileDecls...)
	}
	for _, importPath := range jitScopePaths {
		if _, visibleToEval := evalImports[importPath]; !visibleToEval {
			continue
		}
		refs = append(refs, collectImportedJITSymbols(importPath, packageTypes[importPath], packageAliases[importPath])...)
	}
	if err := checkJITRefs(refs); err != nil {
		return nil, false, err
	}

	archFeatures, err := json.Marshal(target.ArchFeatures)
	if err != nil {
		return nil, false, fmt.Errorf("serialize architecture features: %w", err)
	}
	bundle := jitmeta.Bundle{Build: jitmeta.BuildMetadata{
		CompilerVersion: buildcfg.Version,
		BuildID:         target.Flags.BuildID,
		GOOS:            target.GOOS,
		GOARCH:          target.GOARCH,
		ArchFeatures:    string(archFeatures),
		GoVersion:       target.Flags.Lang,
		Race:            target.Flags.Race,
		MSan:            target.Flags.MSan,
		ASan:            target.Flags.ASan,
		Coverage:        target.Flags.Cfg.CoverageInfo != nil,
		Checkptr:        target.Debug.Checkptr,
		PackagePath:     target.Ctxt.Pkgpath,
		PackageName:     inputs[0].ast.Name.Name,
		Files:           bundleFiles,
		Declarations:    declarations,
		Packages:        sortedPackageExports(packageExports),
	}}
	for _, ref := range refs {
		bundle.Link.Symbols = append(bundle.Link.Symbols, ref.symbol)
	}
	encoded, err := jitmeta.Marshal(bundle)
	if err != nil {
		return nil, false, fmt.Errorf("serialize compiler metadata: %w", err)
	}

	generated, err := makeJITRegistrationSource(
		inputs[0].ast.Name.Name,
		jitLiteral,
		registrationImportPaths,
		canonicalImports,
		packageAliases,
		runtimeAlias,
		refs,
		base64.StdEncoding.EncodeToString(encoded),
		prefix,
	)
	if err != nil {
		return nil, false, err
	}
	result := make([]noder.SourceFile, 0, len(inputs)+1)
	result = append(result, generated)
	for _, input := range inputs {
		result = append(result, input.source)
	}
	return result, true, nil
}

// finalizeJITScope attaches the exact package fingerprints from the imports
// resolved by noder. This runs after frontend loading and prevents the linker
// from treating a path alone as a sufficient package identity.
func finalizeJITScope(target *ir.Package) error {
	for i := range target.Ctxt.JITScope {
		pkg := &target.Ctxt.JITScope[i]
		var found bool
		for _, imported := range target.Ctxt.Imports {
			if imported.Pkg != pkg.Pkg {
				continue
			}
			if found && pkg.Fingerprint != imported.Fingerprint {
				return fmt.Errorf("conflicting import fingerprints for JIT package %q", pkg.Pkg)
			}
			pkg.Fingerprint = imported.Fingerprint
			found = true
		}
		if !found || pkg.Fingerprint.IsZero() {
			return fmt.Errorf("missing linker fingerprint for JIT package %q", pkg.Pkg)
		}
	}
	return nil
}

func configuredJITImport(target *ir.Package) bool {
	for sourcePath, resolvedPath := range target.Flags.Cfg.ImportMap {
		if isJITImportPath(sourcePath, resolvedPath) {
			return true
		}
	}
	for importPath := range target.Flags.Cfg.PackageFile {
		if isJITImportPath(importPath, importPath) {
			return true
		}
	}
	return false
}

func isJITImportPath(sourcePath, resolvedPath string) bool {
	return sourcePath == jitImportPath || resolvedPath == jitImportPath || strings.HasSuffix(resolvedPath, "/jit")
}

func isJITRuntimePackage(pkg *types2.Package) bool {
	if pkg == nil || pkg.Name() != "jit" {
		return false
	}
	for _, name := range []string{
		"Eval", "EvalAt", "RegisterBuildMetadata", "TypeFor", "LinkReference", "FunctionLink", "VariableLink", "TypeLink",
	} {
		if pkg.Scope().Lookup(name) == nil {
			return false
		}
	}
	return true
}

func resolveJITImportPath(target *ir.Package, importPath string) string {
	if mapped := target.Flags.Cfg.ImportMap[importPath]; mapped != "" {
		importPath = mapped
	}
	if strings.HasPrefix(importPath, "./") || strings.HasPrefix(importPath, "../") || importPath == "." || importPath == ".." {
		base := target.Flags.D
		if base == "" {
			base = target.Ctxt.Pathname
		}
		return filepath.ToSlash(filepath.Clean(filepath.Join(base, filepath.FromSlash(importPath))))
	}
	return path.Clean(importPath)
}

func openCompilerPackage(target *ir.Package, importPath string) (io.ReadCloser, error) {
	if target.Flags.Cfg.PackageFile != nil {
		if filename := target.Flags.Cfg.PackageFile[importPath]; filename != "" {
			return os.Open(filename)
		}
		return nil, fmt.Errorf("build import configuration has no selected package file for %q", importPath)
	}
	for _, dir := range target.Flags.Cfg.ImportDirs {
		for _, ext := range []string{".a", ".o"} {
			if f, err := os.Open(filepath.Join(dir, filepath.FromSlash(importPath)+ext)); err == nil {
				return f, nil
			}
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
		for _, ext := range []string{".a", ".o"} {
			filename := filepath.Join(buildcfg.GOROOT, "pkg", target.GOOS+"_"+target.GOARCH+suffix, filepath.FromSlash(importPath)+ext)
			if f, err := os.Open(filename); err == nil {
				return f, nil
			}
		}
	}
	return nil, fmt.Errorf("no compiler package file for %q", importPath)
}

func collectPackageExports(target *ir.Package, root string, exports map[string]jitmeta.PackageExport, includeClosure bool) error {
	if root == "" || root == "builtin" || root == "unsafe" || exports[root].Path != "" {
		return nil
	}
	archive, err := openCompilerPackage(target, root)
	if err != nil {
		return fmt.Errorf("read export data for imported package %q: %w", root, err)
	}
	archiveData, readErr := io.ReadAll(archive)
	closeErr := archive.Close()
	if readErr != nil {
		return fmt.Errorf("read package archive %q: %w", root, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close package archive %q: %w", root, closeErr)
	}

	headerReader := bufio.NewReader(bytes.NewReader(archiveData))
	if _, err := exportdata.FindPackageDefinition(headerReader); err != nil {
		return fmt.Errorf("read package archive %q: %w", root, err)
	}
	objectHeader, headers, err := exportdata.ReadObjectHeaders(headerReader)
	if err != nil {
		return fmt.Errorf("read package headers %q: %w", root, err)
	}
	data, err := exportdata.ReadUnified(bufio.NewReader(bytes.NewReader(archiveData)), true)
	if err != nil {
		return fmt.Errorf("read unified export data for %q: %w", root, err)
	}
	decoder := pkgbits.NewPkgDecoder(root, string(data))
	name := packageNameFromExport(&decoder)
	if name == "" {
		return fmt.Errorf("package %q has no package name in export data", root)
	}
	dependencies, err := packageDependencies(&decoder, root)
	if err != nil {
		return err
	}
	allHeaders := append([]string{objectHeader}, headers...)
	publicRoot := decoder.NewDecoder(pkgbits.SectionMeta, pkgbits.PublicRootIdx, pkgbits.SyncPublic)
	exports[root] = jitmeta.PackageExport{
		Path: root, Name: name, ObjectHeader: strings.Join(allHeaders, ""),
		Dependencies:      dependencies,
		ExportDataVersion: uint32(publicRoot.Version()),
		Fingerprint:       decoder.Fingerprint(),
		Data:              data,
	}
	if len(exports) > 1<<16 {
		return fmt.Errorf("jit package set is too large (more than 65536 packages)")
	}
	if includeClosure {
		for _, dependency := range dependencies {
			if err := collectPackageExports(target, dependency.Path, exports, true); err != nil {
				return fmt.Errorf("read transitive package %q referenced by %q: %w", dependency.Path, root, err)
			}
		}
	}
	return nil
}

func packageDependencies(pr *pkgbits.PkgDecoder, currentPath string) ([]jitmeta.PackageDependency, error) {
	names := make(map[string]string)
	paths := make(map[string]bool)
	for i := 0; i < pr.NumElems(pkgbits.SectionPkg); i++ {
		r := pr.NewDecoder(pkgbits.SectionPkg, pkgbits.RelElemIdx(i), pkgbits.SyncPkgDef)
		importPath := r.String()
		if importPath == "builtin" || importPath == "unsafe" {
			continue
		}
		name := r.String()
		if importPath == "" {
			importPath = currentPath
		}
		if importPath != currentPath {
			paths[importPath] = true
			if name != "" {
				names[importPath] = name
			}
		}
		for n := r.Len(); n > 0; n-- {
			r.Sync(pkgbits.SyncPkg)
			dependency := pr.PeekPkgPath(r.Reloc(pkgbits.SectionPkg))
			if dependency != currentPath && dependency != "builtin" && dependency != "unsafe" {
				paths[dependency] = true
			}
		}
	}
	result := make([]jitmeta.PackageDependency, 0, len(paths))
	for importPath := range paths {
		name := names[importPath]
		if name == "" {
			return nil, fmt.Errorf("export data for %q references package %q without a name", currentPath, importPath)
		}
		result = append(result, jitmeta.PackageDependency{Path: importPath, Name: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func packageNameFromExport(pr *pkgbits.PkgDecoder) string {
	root := pr.NewDecoder(pkgbits.SectionMeta, pkgbits.PublicRootIdx, pkgbits.SyncPublic)
	root.Sync(pkgbits.SyncPkg)
	pkgIndex := root.Reloc(pkgbits.SectionPkg)
	pkg := pr.NewDecoder(pkgbits.SectionPkg, pkgIndex, pkgbits.SyncPkgDef)
	importPath := pkg.String()
	if importPath == "builtin" || importPath == "unsafe" {
		return importPath
	}
	return pkg.String()
}

func sortedPackageExports(exports map[string]jitmeta.PackageExport) []jitmeta.PackageExport {
	result := make([]jitmeta.PackageExport, 0, len(exports))
	for _, export := range exports {
		result = append(result, export)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func readJITTypes(importPath string, data []byte) (*types2.Package, error) {
	imports := make(map[string]*types2.Package)
	pkg := testimporter.ReadPackage(nil, imports, pkgbits.NewPkgDecoder(importPath, string(data)))
	if pkg == nil || !pkg.Complete() {
		return nil, fmt.Errorf("compiler export data did not provide a complete package scope")
	}
	return pkg, nil
}

func collectLocalJITSymbols(packagePath string, fset *token.FileSet, file *ast.File, filename string, constraints map[string]bool) ([]jitLinkRef, []jitmeta.Declaration) {
	var refs []jitLinkRef
	var declarations []jitmeta.Declaration
	genericTypes := make(map[string]bool)
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					genericTypes[typeSpec.Name.Name] = typeSpec.TypeParams != nil && len(typeSpec.TypeParams.List) > 0
				}
			}
		}
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			switch decl.Tok {
			case token.TYPE:
				for _, spec := range decl.Specs {
					spec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					generic := spec.TypeParams != nil && len(spec.TypeParams.List) > 0
					single := *decl
					single.Specs = []ast.Spec{spec}
					source, _ := formatNode(fset, &single)
					declarations = append(declarations, jitmeta.Declaration{
						File: filename, Names: []string{spec.Name.Name}, Kind: "type", Source: source, Generic: generic,
					})
					if !generic && !constraints[spec.Name.Name] {
						addJITRef(&refs, jitmeta.Symbol{PackagePath: packagePath, Name: spec.Name.Name, Kind: "type"},
							"__jit_runtime.TypeFor["+spec.Name.Name+"]()")
					}
				}
			case token.CONST:
				source, _ := formatNode(fset, decl)
				var names []string
				for _, spec := range decl.Specs {
					if spec, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range spec.Names {
							names = append(names, name.Name)
						}
					}
				}
				declarations = append(declarations, jitmeta.Declaration{
					File: filename, Names: names, Kind: "const", Source: source,
				})
			case token.VAR:
				for _, spec := range decl.Specs {
					spec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					single := *decl
					single.Specs = []ast.Spec{spec}
					source, _ := formatNode(fset, &single)
					names := make([]string, 0, len(spec.Names))
					for _, name := range spec.Names {
						names = append(names, name.Name)
						if name.Name != "_" {
							addJITRef(&refs, jitmeta.Symbol{PackagePath: packagePath, Name: name.Name, Kind: "variable"}, "&"+name.Name)
						}
					}
					declarations = append(declarations, jitmeta.Declaration{
						File: filename, Names: names, Kind: "var", Source: source,
					})
				}
			}
		case *ast.FuncDecl:
			copyDecl := *decl
			generic := decl.Type.TypeParams != nil && len(decl.Type.TypeParams.List) > 0
			if !generic {
				copyDecl.Body = nil
			}
			source, _ := formatNode(fset, &copyDecl)
			if decl.Recv == nil {
				declarations = append(declarations, jitmeta.Declaration{
					File: filename, Names: []string{decl.Name.Name}, Kind: "func", Source: source, Generic: generic,
				})
				if !generic && decl.Name.Name != "init" && decl.Name.Name != "_" {
					addJITRef(&refs, jitmeta.Symbol{PackagePath: packagePath, Name: decl.Name.Name, Kind: "function"}, decl.Name.Name)
				}
				continue
			}

			receiver, receiverGeneric := localReceiver(decl.Recv)
			methodName := receiver + "." + decl.Name.Name
			declarations = append(declarations, jitmeta.Declaration{
				File: filename, Names: []string{methodName}, Kind: "method", Source: source, Generic: receiverGeneric,
			})
			if receiver == "" {
				continue
			}
			baseType := strings.TrimPrefix(receiver, "*")
			if genericTypes[baseType] {
				receiverGeneric = true
			}
			if receiverGeneric || decl.Name.Name == "init" || decl.Name.Name == "_" {
				continue
			}
			receiverExpr, pointer := localReceiverExpr(decl.Recv)
			if receiverExpr == "" {
				continue
			}
			expr := receiverExpr + "." + decl.Name.Name
			if pointer {
				expr = "(*" + receiverExpr + ")." + decl.Name.Name
			}
			addJITRef(&refs, jitmeta.Symbol{PackagePath: packagePath, Name: methodName, Kind: "function"}, expr)
		}
	}
	return refs, declarations
}

// collectLocalJITConstraints identifies declared interfaces that are usable
// only as type parameter constraints. They have no runtime reflect.Type and
// cannot be passed to jit.TypeFor, so they must not become link references.
// The compiler metadata pass runs before noder type checking; this small AST
// walk resolves local constraint aliases and imported interface type sets from
// the export data already collected for the package.
func collectLocalJITConstraints(inputs []jitSourceFile, target *ir.Package, packages map[string]*types2.Package) map[string]bool {
	type declaration struct {
		file     *ast.File
		typeExpr ast.Expr
		name     string
	}
	var declarations []declaration
	localTypes := make(map[string]ast.Expr)
	for _, input := range inputs {
		for _, raw := range input.ast.Decls {
			gen, ok := raw.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, rawSpec := range gen.Specs {
				spec, ok := rawSpec.(*ast.TypeSpec)
				if ok && spec.Name.Name != "_" {
					declarations = append(declarations, declaration{file: input.ast, typeExpr: spec.Type, name: spec.Name.Name})
					localTypes[spec.Name.Name] = spec.Type
				}
			}
		}
	}

	constraints := make(map[string]bool)
	for changed := true; changed; {
		changed = false
		for _, declaration := range declarations {
			if constraints[declaration.name] {
				continue
			}
			aliases := jitImportAliases(target, declaration.file, packages)
			if isJITConstraintDeclaration(declaration.typeExpr, constraints, localTypes, aliases) {
				constraints[declaration.name] = true
				changed = true
			}
		}
	}
	return constraints
}

func jitImportAliases(target *ir.Package, file *ast.File, packages map[string]*types2.Package) map[string]*types2.Package {
	aliases := make(map[string]*types2.Package)
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		pkg := packages[resolveJITImportPath(target, importPath)]
		if pkg == nil {
			continue
		}
		alias := pkg.Name()
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias != "_" && alias != "." {
			aliases[alias] = pkg
		}
	}
	return aliases
}

func isJITConstraintDeclaration(expr ast.Expr, local map[string]bool, localTypes map[string]ast.Expr, imported map[string]*types2.Package) bool {
	switch expr := expr.(type) {
	case *ast.InterfaceType:
		if expr.Methods == nil {
			return false
		}
		for _, field := range expr.Methods.List {
			if len(field.Names) == 0 && isJITConstraintElement(field.Type, local, localTypes, imported) {
				return true
			}
		}
	case *ast.Ident:
		return expr.Name == "comparable" || local[expr.Name]
	case *ast.SelectorExpr:
		return importedJITTypeIsConstraint(expr, imported)
	case *ast.IndexExpr:
		return isJITConstraintDeclaration(expr.X, local, localTypes, imported)
	case *ast.IndexListExpr:
		return isJITConstraintDeclaration(expr.X, local, localTypes, imported)
	case *ast.ParenExpr:
		return isJITConstraintDeclaration(expr.X, local, localTypes, imported)
	}
	return false
}

func isJITConstraintElement(expr ast.Expr, local map[string]bool, localTypes map[string]ast.Expr, imported map[string]*types2.Package) bool {
	if isJITConstraintDeclaration(expr, local, localTypes, imported) {
		return true
	}
	return !isJITMethodSetInterface(expr, local, localTypes, imported, make(map[string]bool))
}

func isJITMethodSetInterface(expr ast.Expr, local map[string]bool, localTypes map[string]ast.Expr, imported map[string]*types2.Package, seen map[string]bool) bool {
	switch expr := expr.(type) {
	case *ast.InterfaceType:
		return !isJITConstraintDeclaration(expr, local, localTypes, imported)
	case *ast.Ident:
		if local[expr.Name] || seen[expr.Name] {
			return false
		}
		if localType, ok := localTypes[expr.Name]; ok {
			seen[expr.Name] = true
			return isJITMethodSetInterface(localType, local, localTypes, imported, seen)
		}
		if expr.Name == "any" || expr.Name == "error" {
			return true
		}
		for _, pkg := range imported {
			if typeName, ok := pkg.Scope().Lookup(expr.Name).(*types2.TypeName); ok && typeNameIsMethodSetInterface(typeName) {
				return true
			}
		}
	case *ast.SelectorExpr:
		alias, ok := expr.X.(*ast.Ident)
		if !ok || imported[alias.Name] == nil {
			return false
		}
		typeName, ok := imported[alias.Name].Scope().Lookup(expr.Sel.Name).(*types2.TypeName)
		return ok && typeNameIsMethodSetInterface(typeName)
	case *ast.IndexExpr:
		return isJITMethodSetInterface(expr.X, local, localTypes, imported, seen)
	case *ast.IndexListExpr:
		return isJITMethodSetInterface(expr.X, local, localTypes, imported, seen)
	case *ast.ParenExpr:
		return isJITMethodSetInterface(expr.X, local, localTypes, imported, seen)
	}
	return false
}

func importedJITTypeIsConstraint(expr *ast.SelectorExpr, imported map[string]*types2.Package) bool {
	alias, ok := expr.X.(*ast.Ident)
	if !ok || imported[alias.Name] == nil {
		return false
	}
	typeName, ok := imported[alias.Name].Scope().Lookup(expr.Sel.Name).(*types2.TypeName)
	return ok && !typeNameIsMethodSetInterface(typeName) && isInterfaceTypeName(typeName)
}

func isInterfaceTypeName(typeName *types2.TypeName) bool {
	_, ok := typeName.Type().Underlying().(*types2.Interface)
	return ok
}

func typeNameIsMethodSetInterface(typeName *types2.TypeName) bool {
	iface, ok := typeName.Type().Underlying().(*types2.Interface)
	return ok && iface.IsMethodSet()
}

func collectImportedJITSymbols(importPath string, pkg *types2.Package, alias string) []jitLinkRef {
	if pkg == nil || alias == "" || importPath == "unsafe" || importPath == "C" {
		return nil
	}
	var refs []jitLinkRef
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if obj == nil || !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types2.Func:
			if obj.Signature().TypeParams().Len() == 0 {
				addJITRef(&refs, jitmeta.Symbol{PackagePath: importPath, Name: name, Kind: "function"}, alias+"."+name)
			}
		case *types2.Var:
			addJITRef(&refs, jitmeta.Symbol{PackagePath: importPath, Name: name, Kind: "variable"}, "&"+alias+"."+name)
		case *types2.TypeName:
			if isJITReflectableType(obj.Type()) {
				addJITRef(&refs, jitmeta.Symbol{PackagePath: importPath, Name: name, Kind: "type"},
					"__jit_runtime.TypeFor["+alias+"."+name+"]()")
			}
			// A type alias contributes no linker identity of its own. Its
			// target package is part of the imported package closure and emits
			// the method references under the target type's real identity.
			if obj.IsAlias() {
				continue
			}
			named, ok := types2.Unalias(obj.Type()).(*types2.Named)
			if !ok || named.TypeParams().Len() != 0 {
				continue
			}
			// An exported type alias exposes the target type's methods, but
			// their linker symbols belong to the package that defined the
			// target type. Keep the generated Go expression in the importing
			// package's scope while recording the defining package identity.
			receiverObj := named.Obj()
			receiverPath := importPath
			if receiverObj.Pkg() != nil {
				receiverPath = receiverObj.Pkg().Path()
			}
			for i := 0; i < named.NumMethods(); i++ {
				method := named.Method(i)
				if !method.Exported() {
					continue
				}
				receiver := method.Signature().Recv().Type()
				pointer := false
				if ptr, ok := receiver.(*types2.Pointer); ok {
					pointer = true
					receiver = ptr.Elem()
				}
				receiverNamed, ok := types2.Unalias(receiver).(*types2.Named)
				if !ok || receiverNamed.Obj() != named.Obj() {
					continue
				}
				typeExpr := alias + "." + name
				if pointer {
					typeExpr = "(*" + typeExpr + ")"
				}
				addJITRef(&refs,
					jitmeta.Symbol{PackagePath: receiverPath, Name: receiverObj.Name() + "." + method.Name(), Kind: "function"},
					typeExpr+"."+method.Name())
			}
		}
	}
	return refs
}

func isJITReflectableType(typ types2.Type) bool {
	typ = types2.Unalias(typ)
	if named, ok := typ.(*types2.Named); ok && named.TypeParams().Len() != 0 {
		return false
	}
	if iface, ok := typ.Underlying().(*types2.Interface); ok && !iface.IsMethodSet() {
		return false
	}
	return true
}

func localReceiver(fields *ast.FieldList) (string, bool) {
	expr, _ := localReceiverExpr(fields)
	base := strings.TrimPrefix(expr, "*")
	return base, strings.Contains(base, "[") || expr == ""
}

func localReceiverExpr(fields *ast.FieldList) (string, bool) {
	if fields == nil || len(fields.List) != 1 {
		return "", false
	}
	expr := fields.List[0].Type
	pointer := false
	if star, ok := expr.(*ast.StarExpr); ok {
		pointer, expr = true, star.X
	}
	var out bytes.Buffer
	if err := format.Node(&out, token.NewFileSet(), expr); err != nil {
		return "", false
	}
	return out.String(), pointer
}

func addJITRef(refs *[]jitLinkRef, symbol jitmeta.Symbol, value string) {
	*refs = append(*refs, jitLinkRef{symbol: symbol, value: value})
}

func checkJITRefs(refs []jitLinkRef) error {
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		key := ref.symbol.PackagePath + "\x00" + ref.symbol.Name + "\x00" + ref.symbol.Kind
		if seen[key] {
			return fmt.Errorf("jit metadata has multiple link expressions for %s.%s", ref.symbol.PackagePath, ref.symbol.Name)
		}
		seen[key] = true
	}
	return nil
}

func makeJITRegistrationSource(
	packageName, jitLiteral string,
	importPaths []string,
	sourceImports, aliases map[string]string,
	runtimeAlias string,
	refs []jitLinkRef,
	encoded, prefix string,
) (noder.SourceFile, error) {
	if jitLiteral == "" || runtimeAlias == "" {
		return noder.SourceFile{}, fmt.Errorf("jit metadata has no runtime package import")
	}
	var source strings.Builder
	fmt.Fprintf(&source, "package %s\n\nimport (\n", packageName)
	for _, importPath := range importPaths {
		alias := aliases[importPath]
		literal := sourceImports[importPath]
		if alias == "" || literal == "" {
			continue
		}
		// Keep packages visible to eval in the binary even when they expose no
		// linkable nongeneric declarations. A blank import avoids making the
		// compiler-generated registration file fail on an unused alias.
		used := alias == runtimeAlias // RegisterBuildMetadata uses jit directly.
		for _, ref := range refs {
			if ref.symbol.PackagePath == importPath {
				used = true
				break
			}
		}
		if !used {
			alias = "_"
		}
		fmt.Fprintf(&source, "%s %s\n", alias, strconv.Quote(literal))
	}
	source.WriteString(")\n\n")

	const refsPerFunction = 128
	chunks := (len(refs) + refsPerFunction - 1) / refsPerFunction
	for chunk := 0; chunk < chunks; chunk++ {
		start := chunk * refsPerFunction
		end := start + refsPerFunction
		if end > len(refs) {
			end = len(refs)
		}
		fmt.Fprintf(&source, "func %s_refs_%d() []%s.LinkReference {\n", prefix, chunk, runtimeAlias)
		fmt.Fprintf(&source, "return []%s.LinkReference{\n", runtimeAlias)
		for _, ref := range refs[start:end] {
			value := strings.ReplaceAll(ref.value, "__jit_runtime", runtimeAlias)
			kind := ""
			switch ref.symbol.Kind {
			case "function":
				kind = "FunctionLink"
			case "variable":
				kind = "VariableLink"
			case "type":
				kind = "TypeLink"
			default:
				return noder.SourceFile{}, fmt.Errorf("unknown jit link kind %q", ref.symbol.Kind)
			}
			fmt.Fprintf(&source, "{PackagePath:%s, Name:%s, Kind:%s.%s, Value:%s},\n",
				strconv.Quote(ref.symbol.PackagePath),
				strconv.Quote(ref.symbol.Name),
				runtimeAlias,
				kind,
				value,
			)
		}
		source.WriteString("}\n}\n\n")
	}
	fmt.Fprintf(&source, "func init() {\nrefs := make([]%s.LinkReference, 0, %d)\n", runtimeAlias, len(refs))
	for chunk := 0; chunk < chunks; chunk++ {
		fmt.Fprintf(&source, "refs = append(refs, %s_refs_%d()...)\n", prefix, chunk)
	}
	fmt.Fprintf(&source, "%s.RegisterBuildMetadata(%s, refs)\n}\n",
		runtimeAlias, strconv.Quote(encoded))

	name := prefix + ".go"
	return noder.SourceFile{Name: name, Data: source.String()}, nil
}

func formatNode(fset *token.FileSet, node any) (string, error) {
	var out bytes.Buffer
	err := format.Node(&out, fset, node)
	return out.String(), err
}

func sourceHasIdentifier(files []noder.SourceFile, identifier string) bool {
	for _, file := range files {
		if strings.Contains(file.Data, identifier) {
			return true
		}
	}
	return false
}
