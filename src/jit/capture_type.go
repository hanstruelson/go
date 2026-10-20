// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// captureTypeRenderer turns a concrete runtime type into a Go type expression
// usable by a snippet compiled in the host package. It deliberately refuses
// types whose source identity cannot be recovered from reflect.Type alone.
type captureTypeRenderer struct {
	metadata   *Metadata
	ctx        PackageContext
	imports    map[string]string // package path -> import alias ("." means dot import)
	needed     map[string]bool
	usedNames  map[string]bool
	packageIDs map[string]bool
	nextAlias  int
}

func newCaptureTypeRenderer(metadata *Metadata, ctx PackageContext, captures []Capture) *captureTypeRenderer {
	r := &captureTypeRenderer{
		metadata:   metadata,
		ctx:        ctx,
		imports:    make(map[string]string),
		needed:     make(map[string]bool),
		usedNames:  make(map[string]bool),
		packageIDs: make(map[string]bool),
	}
	for _, declaration := range metadata.Build.Declarations {
		for _, name := range declaration.Names {
			r.usedNames[name] = true
			if declaration.Kind == "type" {
				r.packageIDs[name] = true
			}
		}
	}
	for _, capture := range captures {
		r.usedNames[capture.name] = true
		for _, imp := range capture.imports {
			if imp.alias != "" && imp.alias != "." && imp.alias != "_" {
				r.usedNames[imp.alias] = true
			}
			if imp.alias != "_" {
				if _, exists := r.imports[imp.path]; !exists {
					r.imports[imp.path] = imp.alias
				}
			}
		}
	}
	for _, imp := range ctx.Imports {
		alias := imp.Alias
		if alias == "" {
			for _, pkg := range metadata.Build.Packages {
				if pkg.Path == imp.Path {
					alias = pkg.Name
					break
				}
			}
		}
		if alias != "" && alias != "." && alias != "_" {
			r.usedNames[alias] = true
		}
		if _, exists := r.imports[imp.Path]; !exists {
			r.imports[imp.Path] = alias
		}
	}
	return r
}

func (r *captureTypeRenderer) render(typ reflect.Type) (string, error) {
	if typ == nil {
		return "", fmt.Errorf("missing runtime type")
	}
	if typ.Name() != "" {
		return r.renderNamed(typ)
	}
	switch typ.Kind() {
	case reflect.Pointer:
		elem, err := r.render(typ.Elem())
		return "*" + elem, err
	case reflect.Slice:
		elem, err := r.render(typ.Elem())
		return "[]" + elem, err
	case reflect.Array:
		elem, err := r.render(typ.Elem())
		return fmt.Sprintf("[%d]%s", typ.Len(), elem), err
	case reflect.Map:
		key, err := r.render(typ.Key())
		if err != nil {
			return "", err
		}
		value, err := r.render(typ.Elem())
		if err != nil {
			return "", err
		}
		return "map[" + key + "]" + value, nil
	case reflect.Chan:
		elem, err := r.render(typ.Elem())
		if err != nil {
			return "", err
		}
		switch typ.ChanDir() {
		case reflect.RecvDir:
			return "<-chan " + elem, nil
		case reflect.SendDir:
			return "chan<- " + elem, nil
		default:
			return "chan " + elem, nil
		}
	case reflect.Func:
		return r.renderFunc(typ)
	case reflect.Struct:
		return r.renderStruct(typ)
	case reflect.Interface:
		return r.renderInterface(typ)
	default:
		return "", fmt.Errorf("runtime type %s has no representable Go type expression", typ)
	}
}

func (r *captureTypeRenderer) renderNamed(typ reflect.Type) (string, error) {
	name := typ.Name()
	if strings.ContainsAny(name, "[]") {
		return r.renderInstantiatedNamed(typ, name)
	}
	if typ.PkgPath() == "" {
		return name, nil
	}
	if typ.PkgPath() == r.metadata.Build.PackagePath {
		if !r.packageIDs[name] {
			return "", fmt.Errorf("named type %s is local to a function or is absent from host package declarations", typ)
		}
		return name, nil
	}
	if !token.IsExported(name) {
		return "", fmt.Errorf("named type %s is unexported and cannot be named from the JIT function", typ)
	}
	if !r.hasPackageExport(typ.PkgPath()) {
		return "", fmt.Errorf("named type %s belongs to package %q, which has no embedded compiler export data", typ, typ.PkgPath())
	}
	alias, err := r.importAlias(typ.PkgPath())
	if err != nil {
		return "", err
	}
	if alias == "." {
		return name, nil
	}
	return alias + "." + name, nil
}

func (r *captureTypeRenderer) renderInstantiatedNamed(typ reflect.Type, name string) (string, error) {
	expr, err := parser.ParseExpr(name)
	if err != nil {
		return "", fmt.Errorf("instantiated named type %s has an unparseable runtime name: %w", typ, err)
	}
	var base ast.Expr
	var args []ast.Expr
	switch expr := expr.(type) {
	case *ast.IndexExpr:
		base, args = expr.X, []ast.Expr{expr.Index}
	case *ast.IndexListExpr:
		base, args = expr.X, expr.Indices
	default:
		return "", fmt.Errorf("instantiated named type %s has no source type arguments in its runtime name", typ)
	}
	baseName, ok := base.(*ast.Ident)
	if !ok || !token.IsIdentifier(baseName.Name) || token.Lookup(baseName.Name).IsKeyword() {
		return "", fmt.Errorf("instantiated named type %s has an unsupported base name", typ)
	}
	baseSource, err := r.namedTypeSource(typ.PkgPath(), baseName.Name)
	if err != nil {
		return "", err
	}
	arguments := make([]string, len(args))
	for i, arg := range args {
		arguments[i], err = r.renderTypeExpr(arg)
		if err != nil {
			return "", fmt.Errorf("instantiated named type %s argument %d: %w", typ, i, err)
		}
	}
	return baseSource + "[" + strings.Join(arguments, ", ") + "]", nil
}

func (r *captureTypeRenderer) namedTypeSource(path, name string) (string, error) {
	if path == r.metadata.Build.PackagePath {
		if !r.packageIDs[name] {
			return "", fmt.Errorf("type %s is local to a function or absent from host package declarations", name)
		}
		return name, nil
	}
	if !token.IsExported(name) {
		return "", fmt.Errorf("type %s from package %q is unexported and cannot be named from the JIT function", name, path)
	}
	if !r.hasPackageExport(path) {
		return "", fmt.Errorf("type %s belongs to package %q, which has no embedded compiler export data", name, path)
	}
	alias, err := r.importAlias(path)
	if err != nil {
		return "", err
	}
	if alias == "." {
		return name, nil
	}
	return alias + "." + name, nil
}

func (r *captureTypeRenderer) renderTypeExpr(expr ast.Expr) (string, error) {
	switch expr := expr.(type) {
	case *ast.Ident:
		if isBuiltinTypeName(expr.Name) {
			return expr.Name, nil
		}
		if r.packageIDs[expr.Name] {
			return expr.Name, nil
		}
		return "", fmt.Errorf("type name %q is not a predeclared or host package type", expr.Name)
	case *ast.SelectorExpr:
		pkgName, ok := expr.X.(*ast.Ident)
		if !ok {
			return "", fmt.Errorf("unsupported package-qualified type expression")
		}
		path := r.packagePathForName(pkgName.Name)
		if path == "" {
			return "", fmt.Errorf("package qualifier %q is absent or ambiguous in build metadata", pkgName.Name)
		}
		return r.namedTypeSource(path, expr.Sel.Name)
	case *ast.StarExpr:
		elem, err := r.renderTypeExpr(expr.X)
		return "*" + elem, err
	case *ast.ArrayType:
		elem, err := r.renderTypeExpr(expr.Elt)
		if err != nil {
			return "", err
		}
		if expr.Len == nil {
			return "[]" + elem, nil
		}
		length, ok := expr.Len.(*ast.BasicLit)
		if !ok || length.Kind != token.INT {
			return "", fmt.Errorf("array length in runtime type name is not an integer literal")
		}
		return "[" + length.Value + "]" + elem, nil
	case *ast.MapType:
		key, err := r.renderTypeExpr(expr.Key)
		if err != nil {
			return "", err
		}
		value, err := r.renderTypeExpr(expr.Value)
		if err != nil {
			return "", err
		}
		return "map[" + key + "]" + value, nil
	case *ast.ChanType:
		elem, err := r.renderTypeExpr(expr.Value)
		if err != nil {
			return "", err
		}
		switch expr.Dir {
		case ast.RECV:
			return "<-chan " + elem, nil
		case ast.SEND:
			return "chan<- " + elem, nil
		default:
			return "chan " + elem, nil
		}
	case *ast.FuncType:
		return r.renderFuncExpr(expr)
	case *ast.StructType:
		return r.renderStructExpr(expr)
	case *ast.InterfaceType:
		return r.renderInterfaceExpr(expr)
	case *ast.ParenExpr:
		inner, err := r.renderTypeExpr(expr.X)
		return "(" + inner + ")", err
	case *ast.IndexExpr:
		return r.renderIndexedTypeExpr(expr.X, []ast.Expr{expr.Index})
	case *ast.IndexListExpr:
		return r.renderIndexedTypeExpr(expr.X, expr.Indices)
	default:
		return "", fmt.Errorf("unsupported type expression %T in runtime generic type name", expr)
	}
}

func (r *captureTypeRenderer) renderIndexedTypeExpr(base ast.Expr, args []ast.Expr) (string, error) {
	var source string
	switch base := base.(type) {
	case *ast.Ident:
		if !r.packageIDs[base.Name] {
			return "", fmt.Errorf("generic type %q is not a host package declaration", base.Name)
		}
		source = base.Name
	case *ast.SelectorExpr:
		pkgName, ok := base.X.(*ast.Ident)
		if !ok {
			return "", fmt.Errorf("unsupported generic package qualifier")
		}
		path := r.packagePathForName(pkgName.Name)
		if path == "" {
			return "", fmt.Errorf("generic package qualifier %q is absent or ambiguous", pkgName.Name)
		}
		var err error
		source, err = r.namedTypeSource(path, base.Sel.Name)
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported instantiated type base %T", base)
	}
	rendered := make([]string, len(args))
	for i, arg := range args {
		value, err := r.renderTypeExpr(arg)
		if err != nil {
			return "", err
		}
		rendered[i] = value
	}
	return source + "[" + strings.Join(rendered, ", ") + "]", nil
}

func (r *captureTypeRenderer) packagePathForName(name string) string {
	var path string
	add := func(candidate string) bool {
		if path != "" && path != candidate {
			return false
		}
		path = candidate
		return true
	}
	if name == r.metadata.Build.PackageName && !add(r.metadata.Build.PackagePath) {
		return ""
	}
	for _, pkg := range r.metadata.Build.Packages {
		if pkg.Name == name && !add(pkg.Path) {
			return ""
		}
	}
	for _, imp := range r.ctx.Imports {
		alias := imp.Alias
		if alias == "" {
			for _, pkg := range r.metadata.Build.Packages {
				if pkg.Path == imp.Path {
					alias = pkg.Name
					break
				}
			}
		}
		if alias == name && !add(imp.Path) {
			return ""
		}
	}
	return path
}

func isBuiltinTypeName(name string) bool {
	switch name {
	case "any", "bool", "byte", "complex64", "complex128", "error", "float32", "float64",
		"int", "int8", "int16", "int32", "int64", "rune", "string", "uint", "uint8",
		"uint16", "uint32", "uint64", "uintptr":
		return true
	default:
		return false
	}
}

func (r *captureTypeRenderer) renderFuncExpr(expr *ast.FuncType) (string, error) {
	params, err := r.renderFieldTypes(expr.Params, true)
	if err != nil {
		return "", err
	}
	results, err := r.renderFieldTypes(expr.Results, false)
	if err != nil {
		return "", err
	}
	return "func(" + strings.Join(params, ", ") + ")" + renderResults(results), nil
}

func (r *captureTypeRenderer) renderFieldTypes(fields *ast.FieldList, variadic bool) ([]string, error) {
	if fields == nil {
		return nil, nil
	}
	var out []string
	for i, field := range fields.List {
		var fieldType string
		var err error
		if ellipsis, ok := field.Type.(*ast.Ellipsis); ok {
			if !variadic || i != len(fields.List)-1 {
				return nil, fmt.Errorf("ellipsis is only valid on the final function parameter")
			}
			fieldType, err = r.renderTypeExpr(ellipsis.Elt)
			fieldType = "..." + fieldType
		} else {
			fieldType, err = r.renderTypeExpr(field.Type)
		}
		if err != nil {
			return nil, err
		}
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			out = append(out, fieldType)
		}
	}
	return out, nil
}

func (r *captureTypeRenderer) renderStructExpr(expr *ast.StructType) (string, error) {
	fields := make([]string, len(expr.Fields.List))
	for i, field := range expr.Fields.List {
		fieldType, err := r.renderTypeExpr(field.Type)
		if err != nil {
			return "", err
		}
		if len(field.Names) == 0 {
			fields[i] = fieldType
		} else {
			names := make([]string, len(field.Names))
			for j, name := range field.Names {
				names[j] = name.Name
			}
			fields[i] = strings.Join(names, ", ") + " " + fieldType
		}
		if field.Tag != nil {
			fields[i] += " " + field.Tag.Value
		}
	}
	return "struct { " + strings.Join(fields, "; ") + " }", nil
}

func (r *captureTypeRenderer) renderInterfaceExpr(expr *ast.InterfaceType) (string, error) {
	var methods []string
	for _, field := range expr.Methods.List {
		methodType, ok := field.Type.(*ast.FuncType)
		if !ok {
			embedded, err := r.renderTypeExpr(field.Type)
			if err != nil {
				return "", err
			}
			methods = append(methods, embedded)
			continue
		}
		if len(field.Names) == 0 {
			return "", fmt.Errorf("interface method has no name in runtime type name")
		}
		signature, err := r.renderFuncExpr(methodType)
		if err != nil {
			return "", err
		}
		for _, name := range field.Names {
			methods = append(methods, name.Name+strings.TrimPrefix(signature, "func"))
		}
	}
	return "interface { " + strings.Join(methods, "; ") + " }", nil
}

func (r *captureTypeRenderer) hasPackageExport(path string) bool {
	for _, pkg := range r.metadata.Build.Packages {
		if pkg.Path == path && len(pkg.Data) != 0 {
			return true
		}
	}
	return false
}

func (r *captureTypeRenderer) importAlias(path string) (string, error) {
	r.needed[path] = true
	if alias, ok := r.imports[path]; ok {
		if alias == "_" {
			alias = r.allocateAlias()
			r.imports[path] = alias
		}
		if alias == "" {
			for _, pkg := range r.metadata.Build.Packages {
				if pkg.Path == path {
					alias = pkg.Name
					break
				}
			}
			if alias == "" {
				return "", fmt.Errorf("jit: no declared package name for import %q", path)
			}
			r.imports[path] = alias
		}
		return alias, nil
	}
	if !r.hasPackageExport(path) {
		return "", fmt.Errorf("jit: no embedded compiler export data for runtime type package %q", path)
	}
	alias := r.allocateAlias()
	r.imports[path] = alias
	return alias, nil
}

func (r *captureTypeRenderer) allocateAlias() string {
	for {
		alias := fmt.Sprintf("_jit_capture_type_%d", r.nextAlias)
		r.nextAlias++
		if !r.usedNames[alias] {
			r.usedNames[alias] = true
			return alias
		}
	}
}

func (r *captureTypeRenderer) renderFunc(typ reflect.Type) (string, error) {
	params := make([]string, typ.NumIn())
	for i := range params {
		param, err := r.render(typ.In(i))
		if err != nil {
			return "", err
		}
		if typ.IsVariadic() && i == len(params)-1 {
			param, err = r.render(typ.In(i).Elem())
			if err != nil {
				return "", err
			}
			param = "..." + param
		}
		params[i] = param
	}
	results := make([]string, typ.NumOut())
	for i := range results {
		result, err := r.render(typ.Out(i))
		if err != nil {
			return "", err
		}
		results[i] = result
	}
	return "func(" + strings.Join(params, ", ") + ")" + renderResults(results), nil
}

func renderResults(results []string) string {
	switch len(results) {
	case 0:
		return ""
	case 1:
		return " " + results[0]
	default:
		return " (" + strings.Join(results, ", ") + ")"
	}
}

func (r *captureTypeRenderer) renderStruct(typ reflect.Type) (string, error) {
	fields := make([]string, typ.NumField())
	for i := range fields {
		field := typ.Field(i)
		if field.PkgPath != "" && field.PkgPath != r.metadata.Build.PackagePath {
			return "", fmt.Errorf("anonymous struct type %s contains inaccessible field %s", typ, field.Name)
		}
		fieldType, err := r.render(field.Type)
		if err != nil {
			return "", err
		}
		if field.Anonymous {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Name() == "" {
				return "", fmt.Errorf("anonymous struct type %s has an unrepresentable embedded field %s", typ, field.Name)
			}
			fields[i] = fieldType
		} else {
			fields[i] = field.Name + " " + fieldType
		}
		if field.Tag != "" {
			fields[i] += " " + quoteStructTag(field.Tag)
		}
	}
	return "struct { " + strings.Join(fields, "; ") + " }", nil
}

func (r *captureTypeRenderer) renderInterface(typ reflect.Type) (string, error) {
	if typ.NumMethod() == 0 {
		return "any", nil
	}
	methods := make([]string, typ.NumMethod())
	for i := range methods {
		method := typ.Method(i)
		if method.PkgPath != "" && method.PkgPath != r.metadata.Build.PackagePath {
			return "", fmt.Errorf("anonymous interface type %s contains inaccessible method %s", typ, method.Name)
		}
		methodType := method.Type
		params := make([]string, methodType.NumIn())
		for j := range params {
			param, err := r.render(methodType.In(j))
			if err != nil {
				return "", err
			}
			if methodType.IsVariadic() && j == len(params)-1 {
				param, err = r.render(methodType.In(j).Elem())
				if err != nil {
					return "", err
				}
				param = "..." + param
			}
			params[j] = param
		}
		results := make([]string, methodType.NumOut())
		for j := range results {
			result, err := r.render(methodType.Out(j))
			if err != nil {
				return "", err
			}
			results[j] = result
		}
		methods[i] = method.Name + "(" + strings.Join(params, ", ") + ")" + renderResults(results)
	}
	return "interface { " + strings.Join(methods, "; ") + " }", nil
}

func (r *captureTypeRenderer) importsFor(source []captureImport) []captureImport {
	byPath := make(map[string]string, len(source)+len(r.imports))
	for _, imp := range source {
		alias := imp.alias
		if selected, ok := r.imports[imp.path]; ok && (alias == "_" || alias == "") {
			alias = selected
		}
		byPath[imp.path] = alias
	}
	for path := range r.needed {
		if _, exists := byPath[path]; !exists {
			byPath[path] = r.imports[path]
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	imports := make([]captureImport, 0, len(paths))
	for _, path := range paths {
		imports = append(imports, captureImport{alias: byPath[path], path: path})
	}
	return imports
}

func runtimeCaptureTypeExpression(metadata *Metadata, ctx PackageContext, captures []Capture, capture Capture) (string, []captureImport, error) {
	renderer := newCaptureTypeRenderer(metadata, ctx, captures)
	expression, err := renderer.render(capture.runtimeType)
	if err != nil {
		return "", nil, fmt.Errorf("cannot express concrete capture type %s: %w", capture.runtimeType, err)
	}
	imports := renderer.importsFor(capture.imports)
	for _, imp := range imports {
		if imp.path == "" || (imp.alias != "." && (!token.IsIdentifier(imp.alias) || token.Lookup(imp.alias).IsKeyword())) {
			return "", nil, fmt.Errorf("invalid synthesized type import %q for %q", imp.alias, imp.path)
		}
	}
	return expression, imports, nil
}

func quoteStructTag(tag reflect.StructTag) string { return strconv.Quote(string(tag)) }
