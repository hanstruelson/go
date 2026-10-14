// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"go/constant"

	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

var (
	okfor [ir.OEND][]bool
)

var (
	okforeq    [types.NTYPE]bool
	okforadd   [types.NTYPE]bool
	okforand   [types.NTYPE]bool
	okfornone  [types.NTYPE]bool
	okforbool  [types.NTYPE]bool
	okforcap   [types.NTYPE]bool
	okforlen   [types.NTYPE]bool
	okforarith [types.NTYPE]bool
)

var builtinFuncs = [...]struct {
	name string
	op   ir.Op
}{
	{"append", ir.OAPPEND},
	{"cap", ir.OCAP},
	{"clear", ir.OCLEAR},
	{"close", ir.OCLOSE},
	{"complex", ir.OCOMPLEX},
	{"copy", ir.OCOPY},
	{"delete", ir.ODELETE},
	{"imag", ir.OIMAG},
	{"len", ir.OLEN},
	{"make", ir.OMAKE},
	{"max", ir.OMAX},
	{"min", ir.OMIN},
	{"new", ir.ONEW},
	{"panic", ir.OPANIC},
	{"print", ir.OPRINT},
	{"println", ir.OPRINTLN},
	{"real", ir.OREAL},
	{"recover", ir.ORECOVER},
}

var unsafeFuncs = [...]struct {
	name string
	op   ir.Op
}{
	{"Add", ir.OUNSAFEADD},
	{"Slice", ir.OUNSAFESLICE},
	{"SliceData", ir.OUNSAFESLICEDATA},
	{"String", ir.OUNSAFESTRING},
	{"StringData", ir.OUNSAFESTRINGDATA},
}

// InitUniverse initializes the universe block.
func InitUniverse(target *ir.Package) {
	types.InitTypesFor(target.TypeContext, target.BuiltinPkg, target.UnsafePkg, func(sym *types.Sym, typ *types.Type) types.Object {
		n := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, sym)
		n.SetType(typ)
		n.SetTypecheck(1)
		sym.Def = n
		return n
	})
	target.BasicTypes = target.TypeContext.Types
	target.AnyType = target.TypeContext.AnyType
	target.ByteType = target.TypeContext.ByteType
	target.RuneType = target.TypeContext.RuneType
	target.ErrorType = target.TypeContext.ErrorType
	target.ComparableType = target.TypeContext.ComparableType

	for _, s := range &builtinFuncs {
		ir.NewBuiltin(target.BuiltinPkg.Lookup(s.name), s.op)
	}

	for _, s := range &unsafeFuncs {
		ir.NewBuiltin(target.UnsafePkg.Lookup(s.name), s.op)
	}

	s := target.BuiltinPkg.Lookup("true")
	s.Def = ir.NewConstAt(src.NoXPos, s, types.UntypedBool, constant.MakeBool(true))

	s = target.BuiltinPkg.Lookup("false")
	s.Def = ir.NewConstAt(src.NoXPos, s, types.UntypedBool, constant.MakeBool(false))

	s = Lookup(target, "_")
	target.BlankSym = s
	target.BlankNode = ir.NewNameAt(src.NoXPos, s, target.Type(types.TBLANK))
	s.Def = target.BlankNode

	s = target.BuiltinPkg.Lookup("_")
	s.Def = ir.NewNameAt(src.NoXPos, s, target.Type(types.TBLANK))

	s = target.BuiltinPkg.Lookup("nil")
	s.Def = NodNilAt(target, src.NoXPos)

}
