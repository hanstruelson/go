// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"unsafe"
)

func runtimeTypeAddress(name string) (address uintptr, ok bool) {
	defer func() {
		if recover() != nil {
			address, ok = 0, false
		}
	}()
	typ, ok := runtimeTypeFromExpr(strings.TrimPrefix(name, "type:"))
	if !ok || typ == nil {
		return 0, false
	}
	type runtimeTypeInterface struct {
		tab  unsafe.Pointer
		data unsafe.Pointer
	}
	return uintptr((*runtimeTypeInterface)(unsafe.Pointer(&typ)).data), true
}

func runtimeTypeFromExpr(source string) (reflect.Type, bool) {
	expr, err := parser.ParseExpr(source)
	if err != nil {
		return nil, false
	}
	return runtimeTypeFromAST(expr)
}

func runtimeTypeFromAST(expr ast.Expr) (reflect.Type, bool) {
	switch expr := expr.(type) {
	case *ast.Ident:
		types := map[string]reflect.Type{
			"bool": reflect.TypeOf(false), "byte": reflect.TypeOf(byte(0)), "complex64": reflect.TypeOf(complex64(0)),
			"complex128": reflect.TypeOf(complex128(0)), "error": reflect.TypeOf((*error)(nil)).Elem(), "float32": reflect.TypeOf(float32(0)),
			"float64": reflect.TypeOf(float64(0)), "int": reflect.TypeOf(int(0)), "int8": reflect.TypeOf(int8(0)),
			"int16": reflect.TypeOf(int16(0)), "int32": reflect.TypeOf(int32(0)), "int64": reflect.TypeOf(int64(0)),
			"rune": reflect.TypeOf(rune(0)), "string": reflect.TypeOf(""), "uint": reflect.TypeOf(uint(0)),
			"uint8": reflect.TypeOf(uint8(0)), "uint16": reflect.TypeOf(uint16(0)), "uint32": reflect.TypeOf(uint32(0)),
			"uint64": reflect.TypeOf(uint64(0)), "uintptr": reflect.TypeOf(uintptr(0)), "any": reflect.TypeOf((*any)(nil)).Elem(),
		}
		typ, ok := types[expr.Name]
		return typ, ok
	case *ast.SelectorExpr:
		if base, ok := expr.X.(*ast.Ident); ok && base.Name == "unsafe" && expr.Sel.Name == "Pointer" {
			return reflect.TypeOf(unsafe.Pointer(nil)), true
		}
		if base, ok := expr.X.(*ast.Ident); ok && base.Name == "runtime" && expr.Sel.Name == "JITModuleOwner" {
			return reflect.TypeOf((*runtime.JITModuleOwner)(nil)).Elem(), true
		}
	case *ast.ParenExpr:
		return runtimeTypeFromAST(expr.X)
	case *ast.StarExpr:
		elem, ok := runtimeTypeFromAST(expr.X)
		if !ok {
			return nil, false
		}
		return reflect.PointerTo(elem), true
	case *ast.ArrayType:
		elem, ok := runtimeTypeFromAST(expr.Elt)
		if !ok {
			return nil, false
		}
		if expr.Len == nil {
			return reflect.SliceOf(elem), true
		}
		length, ok := expr.Len.(*ast.BasicLit)
		if !ok || length.Kind != token.INT {
			return nil, false
		}
		n, err := strconv.ParseUint(length.Value, 0, 63)
		if err != nil {
			return nil, false
		}
		return reflect.ArrayOf(int(n), elem), true
	case *ast.MapType:
		key, keyOK := runtimeTypeFromAST(expr.Key)
		value, valueOK := runtimeTypeFromAST(expr.Value)
		if !keyOK || !valueOK {
			return nil, false
		}
		return reflect.MapOf(key, value), true
	case *ast.ChanType:
		elem, ok := runtimeTypeFromAST(expr.Value)
		if !ok {
			return nil, false
		}
		dir := reflect.BothDir
		if expr.Dir == ast.RECV {
			dir = reflect.RecvDir
		} else if expr.Dir == ast.SEND {
			dir = reflect.SendDir
		}
		return reflect.ChanOf(dir, elem), true
	case *ast.FuncType:
		params, variadic, ok := runtimeTypeFields(expr.Params)
		if !ok {
			return nil, false
		}
		results, resultVariadic, ok := runtimeTypeFields(expr.Results)
		if !ok || resultVariadic {
			return nil, false
		}
		return reflect.FuncOf(params, results, variadic), true
	case *ast.InterfaceType:
		if expr.Methods != nil && expr.Methods.NumFields() != 0 {
			return nil, false
		}
		return reflect.TypeOf((*any)(nil)).Elem(), true
	}
	return nil, false
}

func runtimeTypeFields(fields *ast.FieldList) ([]reflect.Type, bool, bool) {
	if fields == nil {
		return nil, false, true
	}
	var types []reflect.Type
	variadic := false
	for i, field := range fields.List {
		var typ reflect.Type
		var ok bool
		if ellipsis, isVariadic := field.Type.(*ast.Ellipsis); isVariadic {
			if i != len(fields.List)-1 {
				return nil, false, false
			}
			typ, ok = runtimeTypeFromAST(ellipsis.Elt)
			if ok {
				typ, variadic = reflect.SliceOf(typ), true
			}
		} else {
			typ, ok = runtimeTypeFromAST(field.Type)
		}
		if !ok {
			return nil, false, false
		}
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			types = append(types, typ)
		}
	}
	return types, variadic, true
}
