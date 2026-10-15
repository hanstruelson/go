// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"cmd/compile/publicinternal/base"
	"cmd/publicinternal/obj"
)

// Context owns the target-dependent layout and universe state used by one
// compiler invocation. Types created in this context retain it so layout and
// universe queries do not depend on whichever target was initialized most
// recently.
type Context struct {
	Link *obj.Link
	// Diagnostics owns errors for compilations that use this type universe.
	Diagnostics *base.Diagnostics
	PtrSize     int
	RegSize     int
	MaxWidth    int64

	SlicePtrOffset int64
	SliceLenOffset int64
	SliceCapOffset int64
	SliceSize      int64
	StringSize     int64

	CalcSizeDisabled bool
	SkipSizeForTrace bool
	TraceIndent      []byte

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

// NewContext creates a type context with layout for one target architecture.
func NewContext(ptrSize, regSize int, maxWidth int64) *Context {
	return &Context{PtrSize: ptrSize, RegSize: regSize, MaxWidth: maxWidth}
}

// Type returns the canonical type of kind k in this compilation context.
func (c *Context) Type(k Kind) *Type {
	if c == nil {
		panic("nil type context")
	}
	return c.Types[k]
}

// LinkSym returns the linker symbol for name in pkgPrefix. Reserved compiler
// packages use the toolchain's special colon separator.
func (c *Context) LinkSym(pkgPrefix, name string, abi obj.ABI) *obj.LSym {
	if c == nil || c.Link == nil {
		panic("type context has no linker context")
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
		panic("type context has no linker context")
	}
	return c.Link.LookupABIInit(name, abi, func(sym *obj.LSym) { sym.Pkg = "_" })
}

// Context returns the target context that owns t.
func (t *Type) Context() *Context {
	if t == nil || t.context == nil {
		panic("type has no compilation context")
	}
	return t.context
}

func contextForType(t *Type) *Context {
	return t.Context()
}

func contextForTypes(types []*Type) *Context {
	var context *Context
	for _, typ := range types {
		if typ == nil || typ.context == nil {
			continue
		}
		if context != nil && context != typ.context {
			panic("types belong to different compilation contexts")
		}
		context = typ.context
	}
	if context == nil {
		panic("cannot infer compilation context from context-free types")
	}
	return context
}

func contextForFields(fields []*Field) *Context {
	var types []*Type
	for _, f := range fields {
		if f != nil && f.Type != nil {
			types = append(types, f.Type)
		}
	}
	return contextForTypes(types)
}

func ptrSizeFor(c *Context) int {
	if c == nil {
		panic("nil type context")
	}
	return c.PtrSize
}

func regSizeFor(c *Context) int {
	if c == nil {
		panic("nil type context")
	}
	return c.RegSize
}

func maxWidthFor(c *Context) int64 {
	if c == nil {
		panic("nil type context")
	}
	return c.MaxWidth
}

func calcSizeDisabledFor(c *Context) bool {
	if c == nil {
		panic("nil type context")
	}
	return c.CalcSizeDisabled
}
