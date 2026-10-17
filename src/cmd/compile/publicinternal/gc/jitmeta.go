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
		inputs = append(inputs, jitSourceFile{
			source: noder.SourceFile{Name: filename, Data: string(data)},
			ast:    file,
		})
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			resolved := resolveJITImportPath(target, importPath)
			if isJITImportPath(importPath, resolved) && jitPath == "" {
				jitPath, jitLiteral = resolved, importPath
			}
		}
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

	canonicalImports := make(map[string]string)
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
		// The jit package carries the compiler implementation itself. Its
		// dependency closure is not needed just to type-check an eval that does
		// not import those packages; avoid duplicating that closure in every
		// importing executable. Other imports retain their transitive archive
		// closure so runtime compilation can use the exact build-selected files.
		includeClosure := importPath != jitPath
		if err := collectPackageExports(target, importPath, packageExports, includeClosure); err != nil {
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

	var refs []jitLinkRef
	var declarations []jitmeta.Declaration
	for _, input := range inputs {
		fileRefs, fileDecls := collectLocalJITSymbols(target.Ctxt.Pkgpath, fset, input.ast, input.source.Name)
		refs = append(refs, fileRefs...)
		declarations = append(declarations, fileDecls...)
	}
	for _, importPath := range importPaths {
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
		importPaths,
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
		"RegisterBuildMetadata", "TypeFor", "LinkReference", "FunctionLink", "VariableLink", "TypeLink",
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
	if filename := target.Flags.Cfg.PackageFile[importPath]; filename != "" {
		return os.Open(filename)
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
		Archive:           archiveData,
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

func collectLocalJITSymbols(packagePath string, fset *token.FileSet, file *ast.File, filename string) ([]jitLinkRef, []jitmeta.Declaration) {
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
					if !generic {
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
			copyDecl.Body = nil
			source, _ := formatNode(fset, &copyDecl)
			generic := decl.Type.TypeParams != nil && len(decl.Type.TypeParams.List) > 0
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
			if !isGenericGoType(obj.Type()) {
				addJITRef(&refs, jitmeta.Symbol{PackagePath: importPath, Name: name, Kind: "type"},
					"__jit_runtime.TypeFor["+alias+"."+name+"]()")
			}
			named, ok := types2.Unalias(obj.Type()).(*types2.Named)
			if !ok || named.TypeParams().Len() != 0 {
				continue
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
					jitmeta.Symbol{PackagePath: importPath, Name: name + "." + method.Name(), Kind: "function"},
					typeExpr+"."+method.Name())
			}
		}
	}
	return refs
}

func isGenericGoType(typ types2.Type) bool {
	named, ok := types2.Unalias(typ).(*types2.Named)
	return ok && named.TypeParams().Len() != 0
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
