// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package compare contains code for generating comparison
// routines for structs, strings and interfaces.
package compare

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"fmt"
	"math/bits"
	"sort"
)

// IsRegularMemory reports whether t can be compared/hashed as regular memory.
func IsRegularMemory(t *types.Type) bool {
	return types.AlgType(t) == types.AMEM
}

// Memrun finds runs of struct fields for which memory-only algs are appropriate.
// t is the parent struct type, and start is the field index at which to start the run.
// size is the length in bytes of the memory included in the run.
// next is the index just after the end of the memory run.
func Memrun(arch *obj.LinkArch, t *types.Type, start int) (size int64, next int) {
	next = start
	for {
		next++
		if next == t.NumFields() {
			break
		}
		// Stop run after a padded field.
		if types.IsPaddedField(t, next-1) {
			break
		}
		// Also, stop before a blank or non-memory field.
		if f := t.Field(next); f.Sym.IsBlank() || !IsRegularMemory(f.Type) {
			break
		}
		// For issue 46283, don't combine fields if the resulting load would
		// require a larger alignment than the component fields.
		if arch.Alignment > 1 {
			align := t.Alignment()
			if off := t.Field(start).Offset; off&(align-1) != 0 {
				// Offset is less aligned than the containing type.
				// Use offset to determine alignment.
				align = 1 << uint(bits.TrailingZeros64(uint64(off)))
			}
			size := t.Field(next).End() - t.Field(start).Offset
			if size > align {
				break
			}
		}
	}
	return t.Field(next-1).End() - t.Field(start).Offset, next
}

// EqCanPanic reports whether == on type t could panic (has an interface somewhere).
// t must be comparable.
func EqCanPanic(t *types.Type) bool {
	switch t.Kind() {
	default:
		return false
	case types.TINTER:
		return true
	case types.TARRAY:
		return EqCanPanic(t.Elem())
	case types.TSTRUCT:
		for _, f := range t.Fields() {
			if !f.Sym.IsBlank() && EqCanPanic(f.Type) {
				return true
			}
		}
		return false
	}
}

// EqStructCost returns the cost of an equality comparison of two structs.
//
// The cost is determined using an algorithm which takes into consideration
// the size of the registers in the current architecture and the size of the
// memory-only fields in the struct.
func EqStructCost(arch *obj.LinkArch, t *types.Type) int64 {
	cost := int64(0)

	for i, fields := 0, t.Fields(); i < len(fields); {
		f := fields[i]

		// Skip blank-named fields.
		if f.Sym.IsBlank() {
			i++
			continue
		}

		n, _, next := eqStructFieldCost(arch, t, i)

		cost += n
		i = next
	}

	return cost
}

// eqStructFieldCost returns the cost of an equality comparison of two struct fields.
// t is the parent struct type, and i is the index of the field in the parent struct type.
// eqStructFieldCost may compute the cost of several adjacent fields at once. It returns
// the cost, the size of the set of fields it computed the cost for (in bytes), and the
// index of the first field not part of the set of fields for which the cost
// has already been calculated.
func eqStructFieldCost(arch *obj.LinkArch, t *types.Type, i int) (int64, int64, int) {
	var (
		cost    = int64(0)
		regSize = int64(arch.RegSize)

		size int64
		next int
	)

	if arch.Arch.CanMergeLoads {
		// If we can merge adjacent loads then we can calculate the cost of the
		// comparison using the size of the memory run and the size of the registers.
		size, next = Memrun(arch, t, i)
		cost = size / regSize
		if size%regSize != 0 {
			cost++
		}
		return cost, size, next
	}

	// If we cannot merge adjacent loads then we have to use the size of the
	// field and take into account the type to determine how many loads and compares
	// are needed.
	ft := t.Field(i).Type
	size = ft.Size()
	next = i + 1

	return calculateCostForType(arch, ft), size, next
}

func calculateCostForType(arch *obj.LinkArch, t *types.Type) int64 {
	var cost int64
	switch t.Kind() {
	case types.TSTRUCT:
		return EqStructCost(arch, t)
	case types.TSLICE:
		// Slices are not comparable.
		base.Fatalf("calculateCostForType: unexpected slice type")
	case types.TARRAY:
		elemCost := calculateCostForType(arch, t.Elem())
		cost = t.NumElem() * elemCost
	case types.TSTRING, types.TINTER, types.TCOMPLEX64, types.TCOMPLEX128:
		cost = 2
	case types.TINT64, types.TUINT64:
		cost = 8 / int64(arch.RegSize)
	default:
		cost = 1
	}
	return cost
}

// EqStruct compares two structs np and nq for equality.
// It works by building a list of boolean conditions to satisfy.
// Conditions must be evaluated in the returned order and
// properly short-circuited by the caller.
// The first return value is the flattened list of conditions,
// the second value is a boolean indicating whether any of the
// comparisons could panic.
func EqStruct(curfunc *ir.Func, t *types.Type, np, nq ir.Node) ([]ir.Node, bool) {
	// The conditions are a list-of-lists. Conditions are reorderable
	// within each inner list. The outer lists must be evaluated in order.
	var conds [][]ir.Node
	conds = append(conds, []ir.Node{})
	and := func(n ir.Node) {
		i := len(conds) - 1
		conds[i] = append(conds[i], n)
	}

	// Walk the struct using memequal for runs of AMEM
	// and calling specific equality tests for the others.
	for i, fields := 0, t.Fields(); i < len(fields); {
		f := fields[i]

		// Skip blank-named fields.
		if f.Sym.IsBlank() {
			i++
			continue
		}

		typeCanPanic := EqCanPanic(f.Type)

		// Compare non-memory fields with field equality.
		if !IsRegularMemory(f.Type) {
			if typeCanPanic {
				// Enforce ordering by starting a new set of reorderable conditions.
				conds = append(conds, []ir.Node{})
			}
			switch {
			case f.Type.IsString():
				p := typecheck.DotField(ir.CurrentPos(curfunc), typecheck.Expr(curfunc, np), i)
				q := typecheck.DotField(ir.CurrentPos(curfunc), typecheck.Expr(curfunc, nq), i)
				eqlen, eqmem := EqString(curfunc, p, q)
				and(eqlen)
				and(eqmem)
			default:
				and(eqfield(curfunc, np, nq, i))
			}
			if typeCanPanic {
				// Also enforce ordering after something that can panic.
				conds = append(conds, []ir.Node{})
			}
			i++
			continue
		}

		cost, size, next := eqStructFieldCost(curfunc.Target.Ctxt.Arch, t, i)
		if cost <= 4 {
			// Cost of 4 or less: use plain field equality.
			for j := i; j < next; j++ {
				and(eqfield(curfunc, np, nq, j))
			}
		} else {
			// Higher cost: use memequal.
			cc := eqmem(curfunc, np, nq, i, size)
			and(cc)
		}
		i = next
	}

	// Sort conditions to put runtime calls last.
	// Preserve the rest of the ordering.
	var flatConds []ir.Node
	for _, c := range conds {
		isCall := func(n ir.Node) bool {
			return n.Op() == ir.OCALL || n.Op() == ir.OCALLFUNC
		}
		sort.SliceStable(c, func(i, j int) bool {
			return !isCall(c[i]) && isCall(c[j])
		})
		flatConds = append(flatConds, c...)
	}
	return flatConds, len(conds) > 1
}

// EqString returns the nodes
//
//	len(s) == len(t)
//
// and
//
//	memequal(s.ptr, t.ptr, len(s))
//
// which can be used to construct string equality comparison.
// eqlen must be evaluated before eqmem, and shortcircuiting is required.
func EqString(curfunc *ir.Func, s, t ir.Node) (eqlen *ir.BinaryExpr, eqmem *ir.CallExpr) {
	s = typecheck.Conv(curfunc, s, curfunc.Target.Type(types.TSTRING))
	t = typecheck.Conv(curfunc, t, curfunc.Target.Type(types.TSTRING))
	sptr := ir.NewConvExpr(ir.CurrentPos(curfunc), ir.OCONVNOP, curfunc.Target.Type(types.TUNSAFEPTR), ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OSPTR, s))
	tptr := ir.NewConvExpr(ir.CurrentPos(curfunc), ir.OCONVNOP, curfunc.Target.Type(types.TUNSAFEPTR), ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OSPTR, t))
	slen := typecheck.Conv(curfunc, ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OLEN, s), curfunc.Target.Type(types.TUINTPTR))
	tlen := typecheck.Conv(curfunc, ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OLEN, t), curfunc.Target.Type(types.TUINTPTR))

	// Pick the 3rd arg to memequal. Both slen and tlen are fine to use, because we short
	// circuit the memequal call if they aren't the same. But if one is a constant some
	// memequal optimizations are easier to apply.
	probablyConstant := func(n ir.Node) bool {
		if n.Op() == ir.OCONVNOP {
			n = n.(*ir.ConvExpr).X
		}
		if n.Op() == ir.OLITERAL {
			return true
		}
		if n.Op() != ir.ONAME {
			return false
		}
		name := n.(*ir.Name)
		if name.Class != ir.PAUTO {
			return false
		}
		if def := name.Defn; def == nil {
			// n starts out as the empty string
			return true
		} else if def.Op() == ir.OAS && (def.(*ir.AssignStmt).Y == nil || def.(*ir.AssignStmt).Y.Op() == ir.OLITERAL) {
			// n starts out as a constant string
			return true
		}
		return false
	}
	cmplen := slen
	if probablyConstant(t) && !probablyConstant(s) {
		cmplen = tlen
	}

	fn := typecheck.LookupRuntime(curfunc.Target, "memequal")
	call := typecheck.Call(curfunc, ir.CurrentPos(curfunc), fn, []ir.Node{sptr, tptr, ir.Copy(cmplen)}, false).(*ir.CallExpr)

	cmp := ir.NewBinaryExpr(ir.CurrentPos(curfunc), ir.OEQ, slen, tlen)
	cmp = typecheck.Expr(curfunc, cmp).(*ir.BinaryExpr)
	cmp.SetType(curfunc.Target.Type(types.TBOOL))
	return cmp, call
}

// EqInterface returns the nodes
//
//	s.tab == t.tab (or s.typ == t.typ, as appropriate)
//
// and
//
//	ifaceeq(s.tab, s.data, t.data) (or efaceeq(s.typ, s.data, t.data), as appropriate)
//
// which can be used to construct interface equality comparison.
// eqtab must be evaluated before eqdata, and shortcircuiting is required.
func EqInterface(curfunc *ir.Func, s, t ir.Node) (eqtab *ir.BinaryExpr, eqdata *ir.CallExpr) {
	if !types.Identical(s.Type(), t.Type()) {
		base.Fatalf("EqInterface %v %v", s.Type(), t.Type())
	}
	// func ifaceeq(tab *uintptr, x, y unsafe.Pointer) (ret bool)
	// func efaceeq(typ *uintptr, x, y unsafe.Pointer) (ret bool)
	var fn ir.Node
	if s.Type().IsEmptyInterface() {
		fn = typecheck.LookupRuntime(curfunc.Target, "efaceeq")
	} else {
		fn = typecheck.LookupRuntime(curfunc.Target, "ifaceeq")
	}

	stab := ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OITAB, s)
	ttab := ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OITAB, t)
	sdata := ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OIDATA, s)
	tdata := ir.NewUnaryExpr(ir.CurrentPos(curfunc), ir.OIDATA, t)
	sdata.SetType(curfunc.Target.Type(types.TUNSAFEPTR))
	tdata.SetType(curfunc.Target.Type(types.TUNSAFEPTR))
	sdata.SetTypecheck(1)
	tdata.SetTypecheck(1)

	call := typecheck.Call(curfunc, ir.CurrentPos(curfunc), fn, []ir.Node{stab, sdata, tdata}, false).(*ir.CallExpr)

	cmp := ir.NewBinaryExpr(ir.CurrentPos(curfunc), ir.OEQ, stab, ttab)
	cmp = typecheck.Expr(curfunc, cmp).(*ir.BinaryExpr)
	cmp.SetType(curfunc.Target.Type(types.TBOOL))
	return cmp, call
}

// eqfield returns the node
//
//	p.field == q.field
func eqfield(curfunc *ir.Func, p, q ir.Node, field int) ir.Node {
	nx := typecheck.DotField(ir.CurrentPos(curfunc), typecheck.Expr(curfunc, p), field)
	ny := typecheck.DotField(ir.CurrentPos(curfunc), typecheck.Expr(curfunc, q), field)
	return typecheck.Expr(curfunc, ir.NewBinaryExpr(ir.CurrentPos(curfunc), ir.OEQ, nx, ny))
}

// eqmem returns the node
//
//	memequal(&p.field, &q.field, size)
func eqmem(curfunc *ir.Func, p, q ir.Node, field int, size int64) ir.Node {
	nx := typecheck.Expr(curfunc, typecheck.NodAddr(curfunc, typecheck.DotField(ir.CurrentPos(curfunc), p, field)))
	ny := typecheck.Expr(curfunc, typecheck.NodAddr(curfunc, typecheck.DotField(ir.CurrentPos(curfunc), q, field)))

	fn, needsize := eqmemfunc(curfunc.Target, size, nx.Type().Elem())
	call := ir.NewCallExpr(ir.CurrentPos(curfunc), ir.OCALL, fn, nil)
	call.Args.Append(nx)
	call.Args.Append(ny)
	if needsize {
		call.Args.Append(ir.NewInt(ir.CurrentPos(curfunc), size))
	}

	return call
}

func eqmemfunc(target *ir.Package, size int64, t *types.Type) (fn *ir.Name, needsize bool) {
	if !target.Ctxt.Arch.CanMergeLoads && t.Alignment() < int64(target.Ctxt.Arch.Alignment) && t.Alignment() < t.Size() {
		// We can't use larger comparisons if the value might not be aligned
		// enough for the larger comparison. See issues 46283 and 67160.
		size = 0
	}
	switch size {
	case 1, 2, 4, 8, 16:
		buf := fmt.Sprintf("memequal%d", int(size)*8)
		return typecheck.LookupRuntime(target, buf, t, t), false
	}

	return typecheck.LookupRuntime(target, "memequal", t, t), true
}
