// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// Operator and constant-kind tables depend only on type kinds, not on a
// compilation target. Initialize them once and then treat them as immutable.
func init() {
	for et := types.Kind(0); et < types.NTYPE; et++ {
		if types.IsInt[et] || et == types.TIDEAL {
			okforeq[et] = true
			okforarith[et] = true
			okforadd[et] = true
			okforand[et] = true
			ir.OKForConst[et] = true
		}

		if types.IsFloat[et] {
			okforeq[et] = true
			okforadd[et] = true
			okforarith[et] = true
			ir.OKForConst[et] = true
		}

		if types.IsComplex[et] {
			okforeq[et] = true
			okforadd[et] = true
			okforarith[et] = true
			ir.OKForConst[et] = true
		}
	}

	okforadd[types.TSTRING] = true
	okforbool[types.TBOOL] = true
	okforcap[types.TARRAY] = true
	okforcap[types.TCHAN] = true
	okforcap[types.TSLICE] = true
	ir.OKForConst[types.TBOOL] = true
	ir.OKForConst[types.TSTRING] = true
	okforlen[types.TARRAY] = true
	okforlen[types.TCHAN] = true
	okforlen[types.TMAP] = true
	okforlen[types.TSLICE] = true
	okforlen[types.TSTRING] = true

	okforeq[types.TPTR] = true
	okforeq[types.TUNSAFEPTR] = true
	okforeq[types.TINTER] = true
	okforeq[types.TCHAN] = true
	okforeq[types.TSTRING] = true
	okforeq[types.TBOOL] = true
	okforeq[types.TMAP] = true    // nil only; refined in typecheck
	okforeq[types.TFUNC] = true   // nil only; refined in typecheck
	okforeq[types.TSLICE] = true  // nil only; refined in typecheck
	okforeq[types.TARRAY] = true  // only if element type is comparable; refined in typecheck
	okforeq[types.TSTRUCT] = true // only if all struct fields are comparable; refined in typecheck

	for i := range okfor {
		okfor[i] = okfornone[:]
	}

	// binary
	okfor[ir.OADD] = okforadd[:]
	okfor[ir.OAND] = okforand[:]
	okfor[ir.OANDAND] = okforbool[:]
	okfor[ir.OANDNOT] = okforand[:]
	okfor[ir.ODIV] = okforarith[:]
	okfor[ir.OEQ] = okforeq[:]
	okfor[ir.OGE] = types.IsOrdered[:]
	okfor[ir.OGT] = types.IsOrdered[:]
	okfor[ir.OLE] = types.IsOrdered[:]
	okfor[ir.OLT] = types.IsOrdered[:]
	okfor[ir.OMOD] = okforand[:]
	okfor[ir.OMUL] = okforarith[:]
	okfor[ir.ONE] = okforeq[:]
	okfor[ir.OOR] = okforand[:]
	okfor[ir.OOROR] = okforbool[:]
	okfor[ir.OSUB] = okforarith[:]
	okfor[ir.OXOR] = okforand[:]
	okfor[ir.OLSH] = okforand[:]
	okfor[ir.ORSH] = okforand[:]

	// unary
	okfor[ir.OBITNOT] = okforand[:]
	okfor[ir.ONEG] = okforarith[:]
	okfor[ir.ONOT] = okforbool[:]
	okfor[ir.OPLUS] = okforarith[:]

	// special
	okfor[ir.OCAP] = okforcap[:]
	okfor[ir.OLEN] = okforlen[:]
}
