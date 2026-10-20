// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"fmt"
	"go/token"
	"reflect"
	"runtime"
	"strings"
	"unsafe"
)

// Capture is a compiler-generated descriptor for one lexical name visible at
// a direct jit.Eval call. Its fields are private so callers cannot forge an
// address/type pairing. Applications should call Eval; the compiler creates
// these values as part of rewriting that call.
type Capture struct {
	bindingID   uint64
	kind        captureKind
	name        string
	typeExpr    string
	literal     string
	typed       bool
	declaration string
	address     unsafe.Pointer
	imports     []captureImport
	unsupported string
	runtimeType reflect.Type
	typeRoot    unsafe.Pointer
}

type captureKind uint8

const (
	captureValue captureKind = iota + 1
	captureConstant
	captureTypeAlias
)

type captureImport struct {
	alias string
	path  string
}

// CaptureValue is the compiler support constructor used when rewriting a
// direct jit.Eval call. Applications should not call it directly.
//go:noinline
func CaptureValue[T any](bindingID uint64, name, typeExpr string, address *T, unsupported string, imports ...string) Capture {
	runtimeType := reflect.TypeFor[T]()
	capture := Capture{
		bindingID:   bindingID,
		kind:        captureValue,
		name:        name,
		typeExpr:    typeExpr,
		unsupported: unsupported,
		runtimeType: runtimeType,
		typeRoot:    reflectTypeRoot(runtimeType),
	}
	if address != nil {
		capture.address = unsafe.Pointer(address)
	}
	capture.setImports(imports)
	return capture
}

// CaptureConstant records a local constant for a direct jit.Eval call.
// Applications should not call it directly.
func CaptureConstant(bindingID uint64, name, typeExpr, literal string, typed bool, unsupported string, imports ...string) Capture {
	capture := Capture{bindingID: bindingID, kind: captureConstant, name: name, typeExpr: typeExpr, literal: literal, typed: typed, unsupported: unsupported}
	capture.setImports(imports)
	return capture
}

// CaptureTypeAlias records a local type alias for a direct jit.Eval call.
// Defined local types are rejected because recreating one would change Go
// type identity. Applications should not call it directly.
func CaptureTypeAlias(bindingID uint64, name, declaration, unsupported string, imports ...string) Capture {
	capture := Capture{bindingID: bindingID, kind: captureTypeAlias, name: name, declaration: declaration, unsupported: unsupported}
	capture.setImports(imports)
	return capture
}

func (capture *Capture) setImports(imports []string) {
	if len(imports)%2 != 0 {
		capture.unsupported = "compiler emitted an incomplete capture import list"
		return
	}
	for i := 0; i < len(imports); i += 2 {
		capture.imports = append(capture.imports, captureImport{alias: imports[i], path: imports[i+1]})
	}
}

func (capture Capture) validate() error {
	if capture.bindingID == 0 {
		return fmt.Errorf("jit: capture %q has no binding identity", capture.name)
	}
	if !token.IsIdentifier(capture.name) || token.Lookup(capture.name).IsKeyword() || capture.name == "_" {
		return fmt.Errorf("jit: invalid captured name %q", capture.name)
	}
	if capture.unsupported != "" {
		return fmt.Errorf("jit: cannot capture %q: %s", capture.name, capture.unsupported)
	}
	switch capture.kind {
	case captureValue:
		if capture.address == nil {
			return fmt.Errorf("jit: capture %q has no address", capture.name)
		}
		if capture.runtimeType == nil || capture.typeRoot == nil {
			return fmt.Errorf("jit: capture %q has no runtime type identity", capture.name)
		}
		if runtime.JITCaptureAddressOnCurrentStack(capture.address) {
			return fmt.Errorf("jit: captured variable %q still resides on the caller goroutine stack", capture.name)
		}
		if strings.TrimSpace(capture.typeExpr) == "" {
			return fmt.Errorf("jit: capture %q has no source type", capture.name)
		}
	case captureConstant:
		if capture.literal == "" || (capture.typed && strings.TrimSpace(capture.typeExpr) == "") {
			return fmt.Errorf("jit: capture %q has incomplete constant metadata", capture.name)
		}
	case captureTypeAlias:
		if strings.TrimSpace(capture.declaration) == "" {
			return fmt.Errorf("jit: capture %q has no type declaration", capture.name)
		}
	default:
		return fmt.Errorf("jit: capture %q has invalid kind %d", capture.name, capture.kind)
	}
	for _, imp := range capture.imports {
		if imp.path == "" || (imp.alias != "." && (!token.IsIdentifier(imp.alias) || token.Lookup(imp.alias).IsKeyword())) {
			return fmt.Errorf("jit: capture %q has invalid type import %q for %q", capture.name, imp.alias, imp.path)
		}
	}
	return nil
}

// reflectTypeRoot returns the runtime descriptor pointer stored in a
// reflect.Type interface. Capture types are compiler-provided; retaining this
// root lets the module owner discover and retain any JIT module that owns the
// captured type descriptor.
func reflectTypeRoot(typ reflect.Type) unsafe.Pointer {
	if typ == nil {
		return nil
	}
	type iface struct {
		tab  unsafe.Pointer
		data unsafe.Pointer
	}
	return (*iface)(unsafe.Pointer(&typ)).data
}
