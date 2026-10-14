// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"cmd/compile/internal/base"
	"cmd/internal/obj"
)

// Context owns the target-dependent layout and universe state used by one
// compiler invocation. Types created in this context retain it so layout and
// universe queries do not depend on whichever target was initialized most
// recently.
//
// The package-level layout variables and constructors remain as compatibility
// entry points for compiler utilities that do not have an explicit target.
// Compiler paths should use a Context and the target-aware constructors.
type Context struct {
	Link     *obj.Link
	PtrSize  int
	RegSize  int
	MaxWidth int64

	SlicePtrOffset int64
	SliceLenOffset int64
	SliceCapOffset int64
	SliceSize      int64
	StringSize     int64

	CalcSizeDisabled bool
	SkipSizeForTrace bool

	Types          [NTYPE]*Type
	AnyType        *Type
	ByteType       *Type
	RuneType       *Type
	ErrorType      *Type
	ComparableType *Type
	fakeRecvType   *Type

	defercalc         int
	deferredTypeStack []*Type
}

var defaultContext = &Context{}

// NewContext creates a type context with layout for one target architecture.
func NewContext(ptrSize, regSize int, maxWidth int64) *Context {
	return &Context{PtrSize: ptrSize, RegSize: regSize, MaxWidth: maxWidth}
}

// LinkSym returns the linker symbol for name in pkgPrefix. Reserved compiler
// packages use the toolchain's special colon separator.
func (c *Context) LinkSym(pkgPrefix, name string, abi obj.ABI) *obj.LSym {
	if c == nil || c.Link == nil {
		return base.PkgLinksym(pkgPrefix, name, abi)
	}
	sep := "."
	if pkgPrefix == "go" || pkgPrefix == "type" {
		sep = ":"
	}
	return c.Link.LookupABIInit(pkgPrefix+sep+name, abi, func(sym *obj.LSym) { sym.Pkg = pkgPrefix })
}

// Linkname returns the linker symbol for an explicit //go:linkname name.
func (c *Context) Linkname(name string, abi obj.ABI) *obj.LSym {
	if c == nil || c.Link == nil {
		return base.Linkname(name, abi)
	}
	return c.Link.LookupABIInit(name, abi, func(sym *obj.LSym) { sym.Pkg = "_" })
}

// Context returns the target context that owns t. Types created by legacy
// package-level helpers use the default context.
func (t *Type) Context() *Context {
	if t == nil || t.context == nil {
		return defaultContext
	}
	return t.context
}

func contextForType(t *Type) *Context {
	return t.Context()
}

func contextForFields(fields []*Field) *Context {
	for _, f := range fields {
		if f != nil && f.Type != nil {
			return contextForType(f.Type)
		}
	}
	return defaultContext
}

func ptrSizeFor(c *Context) int {
	if c == defaultContext {
		return PtrSize
	}
	return c.PtrSize
}

func regSizeFor(c *Context) int {
	if c == defaultContext {
		return RegSize
	}
	return c.RegSize
}

func maxWidthFor(c *Context) int64 {
	if c == defaultContext {
		return MaxWidth
	}
	return c.MaxWidth
}

func calcSizeDisabledFor(c *Context) bool {
	if c == defaultContext {
		return CalcSizeDisabled
	}
	return c.CalcSizeDisabled
}
