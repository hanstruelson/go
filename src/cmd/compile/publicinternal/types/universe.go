// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"cmd/compile/publicinternal/base"
	"cmd/publicinternal/src"
)

var basicTypes = [...]struct {
	name  string
	etype Kind
}{
	{"int8", TINT8},
	{"int16", TINT16},
	{"int32", TINT32},
	{"int64", TINT64},
	{"uint8", TUINT8},
	{"uint16", TUINT16},
	{"uint32", TUINT32},
	{"uint64", TUINT64},
	{"float32", TFLOAT32},
	{"float64", TFLOAT64},
	{"complex64", TCOMPLEX64},
	{"complex128", TCOMPLEX128},
	{"bool", TBOOL},
	{"string", TSTRING},
}

var typedefs = [...]struct {
	name  string
	etype Kind
}{
	{"int", TINT},
	{"uint", TUINT},
	{"uintptr", TUINTPTR},
}

func InitTypes(builtinPkg, unsafePkg *Pkg, defTypeName func(sym *Sym, typ *Type) Object) {
	InitTypesFor(defaultContext, builtinPkg, unsafePkg, defTypeName)
	Types = defaultContext.Types
	AnyType = defaultContext.AnyType
	ByteType = defaultContext.ByteType
	RuneType = defaultContext.RuneType
	ErrorType = defaultContext.ErrorType
	ComparableType = defaultContext.ComparableType
}

// InitTypesFor initializes the predeclared type universe for context.
func InitTypesFor(context *Context, builtinPkg, unsafePkg *Pkg, defTypeName func(sym *Sym, typ *Type) Object) {
	ptrSize := ptrSizeFor(context)
	if ptrSize == 0 {
		base.Fatalf("InitTypes called before PtrSize was set")
	}

	context.SlicePtrOffset = 0
	context.SliceLenOffset = RoundUp(context.SlicePtrOffset+int64(ptrSize), int64(ptrSize))
	context.SliceCapOffset = RoundUp(context.SliceLenOffset+int64(ptrSize), int64(ptrSize))
	context.SliceSize = RoundUp(context.SliceCapOffset+int64(ptrSize), int64(ptrSize))

	// string is same as slice wo the cap
	context.StringSize = RoundUp(context.SliceLenOffset+int64(ptrSize), int64(ptrSize))
	if context == defaultContext {
		SlicePtrOffset = context.SlicePtrOffset
		SliceLenOffset = context.SliceLenOffset
		SliceCapOffset = context.SliceCapOffset
		SliceSize = context.SliceSize
		StringSize = context.StringSize
	}

	context.Types[TANY] = newTypeIn(context, TANY) // note: an old placeholder type, NOT the new builtin 'any' alias for interface{}
	context.Types[TINTER] = NewInterfaceIn(context, nil)
	CheckSize(context.Types[TINTER])

	defBasic := func(kind Kind, pkg *Pkg, name string) *Type {
		typ := newTypeIn(context, kind)
		obj := defTypeName(pkg.Lookup(name), typ)
		typ.obj = obj
		if kind != TANY {
			CheckSize(typ)
		}
		return typ
	}

	for _, s := range &basicTypes {
		context.Types[s.etype] = defBasic(s.etype, builtinPkg, s.name)
	}

	for _, s := range &typedefs {
		context.Types[s.etype] = defBasic(s.etype, builtinPkg, s.name)
	}

	// We create separate byte and rune types for better error messages
	// rather than just creating type alias *Sym's for the uint8 and
	// int32  Hence, (bytetype|runtype).Sym.isAlias() is false.
	// TODO(gri) Should we get rid of this special case (at the cost
	// of less informative error messages involving bytes and runes)?
	// NOTE(rsc): No, the error message quality is important.
	// (Alternatively, we could introduce an OTALIAS node representing
	// type aliases, albeit at the cost of having to deal with it everywhere).
	context.ByteType = defBasic(TUINT8, builtinPkg, "byte")
	context.RuneType = defBasic(TINT32, builtinPkg, "rune")

	// error type
	DeferCheckSizeFor(context)
	context.ErrorType = defBasic(TFORW, builtinPkg, "error")
	context.ErrorType.SetUnderlying(makeErrorInterface(context, builtinPkg))
	ResumeCheckSizeFor(context)

	// comparable type (interface)
	DeferCheckSizeFor(context)
	context.ComparableType = defBasic(TFORW, builtinPkg, "comparable")
	context.ComparableType.SetUnderlying(makeComparableInterface(context))
	ResumeCheckSizeFor(context)

	// any type (interface)
	DeferCheckSizeFor(context)
	context.AnyType = defBasic(TFORW, builtinPkg, "any")
	context.AnyType.SetUnderlying(NewInterfaceIn(context, nil))
	ResumeCheckSizeFor(context)

	context.Types[TUNSAFEPTR] = defBasic(TUNSAFEPTR, unsafePkg, "Pointer")

	context.Types[TBLANK] = newTypeIn(context, TBLANK)
	context.Types[TNIL] = newTypeIn(context, TNIL)

}

func makeErrorInterface(context *Context, builtinPkg *Pkg) *Type {
	sig := NewSignatureIn(context, FakeRecvFor(context), nil, []*Field{
		NewField(src.NoXPos, nil, context.Types[TSTRING]),
	})
	method := NewField(src.NoXPos, builtinPkg.Lookup("Error"), sig)
	return NewInterfaceIn(context, []*Field{method})
}

// makeComparableInterface makes the predefined "comparable" interface in the
// built-in package. It has a unique name, but no methods.
func makeComparableInterface(context *Context) *Type {
	return NewInterfaceIn(context, nil)
}
