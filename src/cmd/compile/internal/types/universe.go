// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"cmd/compile/internal/base"
	"cmd/internal/src"
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
	if PtrSize == 0 {
		base.Fatalf("InitTypes called before PtrSize was set")
	}

	SlicePtrOffset = 0
	SliceLenOffset = RoundUp(SlicePtrOffset+int64(PtrSize), int64(PtrSize))
	SliceCapOffset = RoundUp(SliceLenOffset+int64(PtrSize), int64(PtrSize))
	SliceSize = RoundUp(SliceCapOffset+int64(PtrSize), int64(PtrSize))

	// string is same as slice wo the cap
	StringSize = RoundUp(SliceLenOffset+int64(PtrSize), int64(PtrSize))

	Types[TANY] = newType(TANY) // note: an old placeholder type, NOT the new builtin 'any' alias for interface{}
	Types[TINTER] = NewInterface(nil)
	CheckSize(Types[TINTER])

	defBasic := func(kind Kind, pkg *Pkg, name string) *Type {
		typ := newType(kind)
		obj := defTypeName(pkg.Lookup(name), typ)
		typ.obj = obj
		if kind != TANY {
			CheckSize(typ)
		}
		return typ
	}

	for _, s := range &basicTypes {
		Types[s.etype] = defBasic(s.etype, builtinPkg, s.name)
	}

	for _, s := range &typedefs {
		Types[s.etype] = defBasic(s.etype, builtinPkg, s.name)
	}

	// We create separate byte and rune types for better error messages
	// rather than just creating type alias *Sym's for the uint8 and
	// int32  Hence, (bytetype|runtype).Sym.isAlias() is false.
	// TODO(gri) Should we get rid of this special case (at the cost
	// of less informative error messages involving bytes and runes)?
	// NOTE(rsc): No, the error message quality is important.
	// (Alternatively, we could introduce an OTALIAS node representing
	// type aliases, albeit at the cost of having to deal with it everywhere).
	ByteType = defBasic(TUINT8, builtinPkg, "byte")
	RuneType = defBasic(TINT32, builtinPkg, "rune")

	// error type
	DeferCheckSize()
	ErrorType = defBasic(TFORW, builtinPkg, "error")
	ErrorType.SetUnderlying(makeErrorInterface(builtinPkg))
	ResumeCheckSize()

	// comparable type (interface)
	DeferCheckSize()
	ComparableType = defBasic(TFORW, builtinPkg, "comparable")
	ComparableType.SetUnderlying(makeComparableInterface())
	ResumeCheckSize()

	// any type (interface)
	DeferCheckSize()
	AnyType = defBasic(TFORW, builtinPkg, "any")
	AnyType.SetUnderlying(NewInterface(nil))
	ResumeCheckSize()

	Types[TUNSAFEPTR] = defBasic(TUNSAFEPTR, unsafePkg, "Pointer")

	Types[TBLANK] = newType(TBLANK)
	Types[TNIL] = newType(TNIL)

}

func makeErrorInterface(builtinPkg *Pkg) *Type {
	sig := NewSignature(FakeRecv(), nil, []*Field{
		NewField(src.NoXPos, nil, Types[TSTRING]),
	})
	method := NewField(src.NoXPos, builtinPkg.Lookup("Error"), sig)
	return NewInterface([]*Field{method})
}

// makeComparableInterface makes the predefined "comparable" interface in the
// built-in package. It has a unique name, but no methods.
func makeComparableInterface() *Type {
	return NewInterface(nil)
}
