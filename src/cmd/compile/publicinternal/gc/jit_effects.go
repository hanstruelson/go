// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"cmd/compile/publicinternal/escape"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/goobj"
	"strings"
)

// jitDirectEffects records source-level effects on one function symbol. The
// linker combines these direct facts with object relocation call edges.
func jitDirectEffects(fn *ir.Func) uint8 {
	var effects uint8
	seenTypes := make(map[*types.Type]bool)
	if typ := fn.Type(); typ != nil {
		if jitTypeContainsUnsafe(typ, seenTypes) {
			effects |= goobj.JITEffectUnsafe
		}
		for _, param := range typ.Params() {
			if escape.ParseLeaks(param.Note).Heap() >= 0 && jitTypeMayHoldCallback(param.Type, make(map[*types.Type]bool)) {
				effects |= goobj.JITEffectRetainsCallbacks
			}
		}
	}
	ir.Visit(fn, func(node ir.Node) {
		if jitTypeContainsUnsafe(node.Type(), seenTypes) {
			effects |= goobj.JITEffectUnsafe
		}
		switch node := node.(type) {
		case *ir.ConvExpr:
			if node.X != nil && node.X.Type() != nil && node.X.Type().IsUnsafePtr() {
				effects |= goobj.JITEffectUnsafe
			}
		case *ir.CallExpr:
			// FuncPCABI intrinsics turn a function symbol into an integer code
			// address. The result is not an owner-bearing Go reference, so any
			// module whose code can be observed this way must remain pinned.
			if ir.IsFuncPCIntrinsic(node) {
				effects |= goobj.JITEffectCodePointer
			}
			switch node.Op() {
			case ir.OCALLINTER:
				// Interface dispatch is dynamic, but it is still a typed Go call:
				// the linker records the method set and the runtime itab/type graph
				// retains the selected implementation. Keep it distinct from an
				// arbitrary function value call so the JIT loader can validate all
				// possible method effects before permitting reclamation.
				effects |= goobj.JITEffectInterfaceCall
			case ir.OCALLFUNC:
				name, ok := node.Fun.(*ir.Name)
				if !ok || name.Class != ir.PFUNC || name.Sym() == nil || name.Sym().Pkg == nil {
					effects |= goobj.JITEffectUnknown
					return
				}
				pkg, symbol := name.Sym().Pkg.Path, name.Sym().Name
				switch {
				case pkg == "unsafe" || strings.HasPrefix(pkg, "internal/unsafe"):
					effects |= goobj.JITEffectUnsafe
				case pkg == "C" || strings.Contains(symbol, "_Cfunc_"):
					effects |= goobj.JITEffectForeign
				case pkg == "runtime" && (symbol == "FuncForPC" || symbol == "ResolveJITFunction"):
					effects |= goobj.JITEffectCodePointer
				case pkg == "reflect" && (strings.HasSuffix(symbol, ".Pointer") || strings.HasSuffix(symbol, ".UnsafePointer") || symbol == "Pointer" || symbol == "UnsafePointer"):
					effects |= goobj.JITEffectCodePointer
				}
			}
		}
		switch node.Op() {
		case ir.OUNSAFEADD, ir.OUNSAFESLICE, ir.OUNSAFESLICEDATA, ir.OUNSAFESTRING, ir.OUNSAFESTRINGDATA:
			effects |= goobj.JITEffectUnsafe
		}
	})
	return effects
}

func jitTypeMayHoldCallback(typ *types.Type, seen map[*types.Type]bool) bool {
	if typ == nil || seen[typ] {
		return false
	}
	if typ.Kind() == types.TFUNC || typ.Kind() == types.TINTER {
		return true
	}
	seen[typ] = true
	switch typ.Kind() {
	case types.TARRAY, types.TCHAN, types.TPTR, types.TSLICE:
		return jitTypeMayHoldCallback(typ.Elem(), seen)
	case types.TMAP:
		return jitTypeMayHoldCallback(typ.Key(), seen) || jitTypeMayHoldCallback(typ.Elem(), seen)
	case types.TSTRUCT:
		for _, field := range typ.Fields() {
			if jitTypeMayHoldCallback(field.Type, seen) {
				return true
			}
		}
	}
	return false
}

func jitTypeContainsUnsafe(typ *types.Type, seen map[*types.Type]bool) bool {
	if typ == nil {
		return false
	}
	if typ.IsUnsafePtr() {
		return true
	}
	if seen[typ] {
		return false
	}
	seen[typ] = true
	switch typ.Kind() {
	case types.TARRAY, types.TCHAN, types.TPTR, types.TSLICE:
		return jitTypeContainsUnsafe(typ.Elem(), seen)
	case types.TMAP:
		return jitTypeContainsUnsafe(typ.Key(), seen) || jitTypeContainsUnsafe(typ.Elem(), seen)
	case types.TSTRUCT, types.TINTER:
		for _, field := range typ.Fields() {
			if jitTypeContainsUnsafe(field.Type, seen) {
				return true
			}
		}
	case types.TFUNC:
		for _, field := range typ.Params() {
			if jitTypeContainsUnsafe(field.Type, seen) {
				return true
			}
		}
		for _, field := range typ.Results() {
			if jitTypeContainsUnsafe(field.Type, seen) {
				return true
			}
		}
	}
	return false
}
