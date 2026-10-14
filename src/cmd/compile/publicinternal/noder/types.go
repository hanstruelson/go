// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/types"
	"cmd/compile/publicinternal/types2"
)

func basicType(target *ir.Package, kind types2.BasicKind) *types.Type {
	switch kind {
	case types2.Invalid:
		return nil
	case types2.Bool:
		return target.Type(types.TBOOL)
	case types2.Int:
		return target.Type(types.TINT)
	case types2.Int8:
		return target.Type(types.TINT8)
	case types2.Int16:
		return target.Type(types.TINT16)
	case types2.Int32:
		return target.Type(types.TINT32)
	case types2.Int64:
		return target.Type(types.TINT64)
	case types2.Uint:
		return target.Type(types.TUINT)
	case types2.Uint8:
		return target.Type(types.TUINT8)
	case types2.Uint16:
		return target.Type(types.TUINT16)
	case types2.Uint32:
		return target.Type(types.TUINT32)
	case types2.Uint64:
		return target.Type(types.TUINT64)
	case types2.Uintptr:
		return target.Type(types.TUINTPTR)
	case types2.Float32:
		return target.Type(types.TFLOAT32)
	case types2.Float64:
		return target.Type(types.TFLOAT64)
	case types2.Complex64:
		return target.Type(types.TCOMPLEX64)
	case types2.Complex128:
		return target.Type(types.TCOMPLEX128)
	case types2.String:
		return target.Type(types.TSTRING)
	case types2.UnsafePointer:
		return target.Type(types.TUNSAFEPTR)
	case types2.UntypedBool:
		return types.UntypedBool
	case types2.UntypedInt:
		return types.UntypedInt
	case types2.UntypedRune:
		return types.UntypedRune
	case types2.UntypedFloat:
		return types.UntypedFloat
	case types2.UntypedComplex:
		return types.UntypedComplex
	case types2.UntypedString:
		return types.UntypedString
	case types2.UntypedNil:
		return target.Type(types.TNIL)
	default:
		panic("unexpected types2 basic kind")
	}
}

var dirs = [...]types.ChanDir{
	types2.SendRecv: types.Cboth,
	types2.SendOnly: types.Csend,
	types2.RecvOnly: types.Crecv,
}

// deref2 does a single deref of types2 type t, if it is a pointer type.
func deref2(t types2.Type) types2.Type {
	if ptr := types2.AsPointer(t); ptr != nil {
		t = ptr.Elem()
	}
	return t
}
