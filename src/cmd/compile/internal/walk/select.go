// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

func (w *walkState) walkSelect(sel *ir.SelectStmt) {
	lno := ir.SetPosFor(w.curfunc.Target, sel)
	if sel.Walked() {
		base.Fatalf("double walkSelect")
	}
	sel.SetWalked(true)

	init := ir.TakeInit(sel)

	init = append(init, w.walkSelectCases(sel.Cases)...)
	sel.Cases = nil

	sel.Compiled = init
	w.walkStmtList(sel.Compiled)

	ir.SetPosValue(w.curfunc.Target, lno)
}

func (w *walkState) walkSelectCases(cases []*ir.CommClause) []ir.Node {
	ncas := len(cases)
	sellineno := w.curfunc.Target.Pos

	// optimization: zero-case select
	if ncas == 0 {
		return []ir.Node{w.mkcallstmt("block")}
	}

	// optimization: one-case select: single op.
	if ncas == 1 {
		cas := cases[0]
		ir.SetPosFor(w.curfunc.Target, cas)
		l := cas.Init()
		if cas.Comm != nil { // not default:
			n := cas.Comm
			l = append(l, ir.TakeInit(n)...)
			switch n.Op() {
			default:
				base.Fatalf("select %v", n.Op())

			case ir.OSEND:
				// already ok

			case ir.OSELRECV2:
				r := n.(*ir.AssignListStmt)
				if ir.IsBlank(r.Lhs[0]) && ir.IsBlank(r.Lhs[1]) {
					n = r.Rhs[0]
					break
				}
				r.SetOp(ir.OAS2RECV)
			}

			l = append(l, n)
		}

		l = append(l, cas.Body...)
		l = append(l, ir.NewBranchStmt(w.curfunc.Target.Pos, ir.OBREAK, nil))
		return l
	}

	// convert case value arguments to addresses.
	// this rewrite is used by both the general code and the next optimization.
	var dflt *ir.CommClause
	for _, cas := range cases {
		ir.SetPosFor(w.curfunc.Target, cas)
		n := cas.Comm
		if n == nil {
			dflt = cas
			continue
		}
		switch n.Op() {
		case ir.OSEND:
			n := n.(*ir.SendStmt)
			n.Value = typecheck.NodAddr(w.curfunc, n.Value)
			n.Value = typecheck.Expr(w.curfunc, n.Value)

		case ir.OSELRECV2:
			n := n.(*ir.AssignListStmt)
			if !ir.IsBlank(n.Lhs[0]) {
				n.Lhs[0] = typecheck.NodAddr(w.curfunc, n.Lhs[0])
				n.Lhs[0] = typecheck.Expr(w.curfunc, n.Lhs[0])
			}
		}
	}

	// optimization: two-case select but one is default: single non-blocking op.
	if ncas == 2 && dflt != nil {
		cas := cases[0]
		if cas == dflt {
			cas = cases[1]
		}

		n := cas.Comm
		ir.SetPosFor(w.curfunc.Target, n)
		r := ir.NewIfStmt(w.curfunc.Target.Pos, nil, nil, nil)
		r.SetInit(cas.Init())
		var cond ir.Node
		switch n.Op() {
		default:
			base.Fatalf("select %v", n.Op())

		case ir.OSEND:
			// if selectnbsend(c, v) { body } else { default body }
			n := n.(*ir.SendStmt)
			ch := n.Chan
			cond = w.mkcall1(chanfn(w.curfunc.Target, "selectnbsend", 2, ch.Type()), w.curfunc.Target.Type(types.TBOOL), r.PtrInit(), ch, n.Value)

		case ir.OSELRECV2:
			n := n.(*ir.AssignListStmt)
			recv := n.Rhs[0].(*ir.UnaryExpr)
			ch := recv.X
			elem := n.Lhs[0]
			if ir.IsBlank(elem) {
				elem = typecheck.NodNilAt(w.curfunc.Target, w.curfunc.Target.Pos)
			}
			cond = typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, w.curfunc.Target.Type(types.TBOOL))
			fn := chanfn(w.curfunc.Target, "selectnbrecv", 2, ch.Type())
			call := w.mkcall1(fn, fn.Type().ResultsTuple(), r.PtrInit(), elem, ch)
			as := ir.NewAssignListStmt(r.Pos(), ir.OAS2, []ir.Node{cond, n.Lhs[1]}, []ir.Node{call})
			r.PtrInit().Append(typecheck.Stmt(w.curfunc, as))
		}

		r.Cond = typecheck.Expr(w.curfunc, cond)
		r.Body = cas.Body
		r.Else = append(dflt.Init(), dflt.Body...)
		return []ir.Node{r, ir.NewBranchStmt(w.curfunc.Target.Pos, ir.OBREAK, nil)}
	}

	if dflt != nil {
		ncas--
	}
	casorder := make([]*ir.CommClause, ncas)
	nsends, nrecvs := 0, 0

	var init []ir.Node

	// generate sel-struct
	ir.SetPosValue(w.curfunc.Target, sellineno)
	selv := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, types.NewArray(scasetype(w.curfunc.Target), int64(ncas)))
	init = append(init, typecheck.Stmt(w.curfunc, ir.NewAssignStmt(w.curfunc.Target.Pos, selv, nil)))

	// No initialization for order; runtime.selectgo is responsible for that.
	order := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, types.NewArray(w.curfunc.Target.Type(types.TUINT16), 2*int64(ncas)))

	var pc0, pcs ir.Node
	if w.curfunc.Target.Flags.Race {
		pcs = typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, types.NewArray(w.curfunc.Target.Type(types.TUINTPTR), int64(ncas)))
		pc0 = typecheck.Expr(w.curfunc, typecheck.NodAddr(w.curfunc, ir.NewIndexExpr(w.curfunc.Target.Pos, pcs, ir.NewInt(w.curfunc.Target.Pos, 0))))
	} else {
		pc0 = typecheck.NodNilAt(w.curfunc.Target, w.curfunc.Target.Pos)
	}

	// register cases
	for _, cas := range cases {
		ir.SetPosFor(w.curfunc.Target, cas)

		init = append(init, ir.TakeInit(cas)...)

		n := cas.Comm
		if n == nil { // default:
			continue
		}

		var i int
		var c, elem ir.Node
		switch n.Op() {
		default:
			base.Fatalf("select %v", n.Op())
		case ir.OSEND:
			n := n.(*ir.SendStmt)
			i = nsends
			nsends++
			c = n.Chan
			elem = n.Value
		case ir.OSELRECV2:
			n := n.(*ir.AssignListStmt)
			nrecvs++
			i = ncas - nrecvs
			recv := n.Rhs[0].(*ir.UnaryExpr)
			c = recv.X
			elem = n.Lhs[0]
		}

		casorder[i] = cas

		setField := func(f string, val ir.Node) {
			r := ir.NewAssignStmt(w.curfunc.Target.Pos, ir.NewSelectorExpr(w.curfunc.Target.Pos, ir.ODOT, ir.NewIndexExpr(w.curfunc.Target.Pos, selv, ir.NewInt(w.curfunc.Target.Pos, int64(i))), typecheck.Lookup(w.curfunc.Target, f)), val)
			init = append(init, typecheck.Stmt(w.curfunc, r))
		}

		c = typecheck.ConvNop(w.curfunc, c, w.curfunc.Target.Type(types.TUNSAFEPTR))
		setField("c", c)
		if !ir.IsBlank(elem) {
			elem = typecheck.ConvNop(w.curfunc, elem, w.curfunc.Target.Type(types.TUNSAFEPTR))
			setField("elem", elem)
		}

		// TODO(mdempsky): There should be a cleaner way to
		// handle this.
		if w.curfunc.Target.Flags.Race {
			r := w.mkcallstmt("selectsetpc", typecheck.NodAddr(w.curfunc, ir.NewIndexExpr(w.curfunc.Target.Pos, pcs, ir.NewInt(w.curfunc.Target.Pos, int64(i)))))
			init = append(init, r)
		}
	}
	if nsends+nrecvs != ncas {
		base.Fatalf("walkSelectCases: miscount: %v + %v != %v", nsends, nrecvs, ncas)
	}

	// run the select
	ir.SetPosValue(w.curfunc.Target, sellineno)
	chosen := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, w.curfunc.Target.Type(types.TINT))
	recvOK := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, w.curfunc.Target.Type(types.TBOOL))
	r := ir.NewAssignListStmt(w.curfunc.Target.Pos, ir.OAS2, nil, nil)
	r.Lhs = []ir.Node{chosen, recvOK}
	fn := typecheck.LookupRuntime(w.curfunc.Target, "selectgo")
	var fnInit ir.Nodes
	r.Rhs = []ir.Node{w.mkcall1(fn, fn.Type().ResultsTuple(), &fnInit, bytePtrToIndex(w.curfunc, selv, 0), bytePtrToIndex(w.curfunc, order, 0), pc0, ir.NewInt(w.curfunc.Target.Pos, int64(nsends)), ir.NewInt(w.curfunc.Target.Pos, int64(nrecvs)), ir.NewBool(w.curfunc.Target.Pos, dflt == nil))}
	init = append(init, fnInit...)
	init = append(init, typecheck.Stmt(w.curfunc, r))

	// selv, order, and pcs (if race) are no longer alive after selectgo.

	// dispatch cases
	dispatch := func(cond ir.Node, cas *ir.CommClause) {
		var list ir.Nodes

		if n := cas.Comm; n != nil && n.Op() == ir.OSELRECV2 {
			n := n.(*ir.AssignListStmt)
			if !ir.IsBlank(n.Lhs[1]) {
				x := ir.NewAssignStmt(w.curfunc.Target.Pos, n.Lhs[1], recvOK)
				list.Append(typecheck.Stmt(w.curfunc, x))
			}
		}

		list.Append(cas.Body.Take()...)
		list.Append(ir.NewBranchStmt(w.curfunc.Target.Pos, ir.OBREAK, nil))

		var r ir.Node
		if cond != nil {
			cond = typecheck.Expr(w.curfunc, cond)
			cond = typecheck.DefaultLitFor(w.curfunc.Target, cond, nil)
			r = ir.NewIfStmt(w.curfunc.Target.Pos, cond, list, nil)
		} else {
			r = ir.NewBlockStmt(w.curfunc.Target.Pos, list)
		}

		init = append(init, r)
	}

	if dflt != nil {
		ir.SetPosFor(w.curfunc.Target, dflt)
		dispatch(ir.NewBinaryExpr(w.curfunc.Target.Pos, ir.OLT, chosen, ir.NewInt(w.curfunc.Target.Pos, 0)), dflt)
	}
	for i, cas := range casorder {
		ir.SetPosFor(w.curfunc.Target, cas)
		if i == len(casorder)-1 {
			dispatch(nil, cas)
			break
		}
		dispatch(ir.NewBinaryExpr(w.curfunc.Target.Pos, ir.OEQ, chosen, ir.NewInt(w.curfunc.Target.Pos, int64(i))), cas)
	}

	return init
}

// bytePtrToIndex returns a Node representing "(*byte)(&n[i])".
func bytePtrToIndex(curfunc *ir.Func, n ir.Node, i int64) ir.Node {
	s := typecheck.NodAddr(curfunc, ir.NewIndexExpr(curfunc.Target.Pos, n, ir.NewInt(curfunc.Target.Pos, i)))
	t := types.NewPtr(curfunc.Target.Type(types.TUINT8))
	return typecheck.ConvNop(curfunc, s, t)
}

// Keep in sync with src/runtime/select.go.
func scasetype(target *ir.Package) *types.Type {
	if target.ScaseType == nil {
		n := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, target.RuntimePkg.Lookup("scase"))
		target.ScaseType = types.NewNamed(n)
		n.SetType(target.ScaseType)
		n.SetTypecheck(1)

		target.ScaseType.SetUnderlying(types.NewStruct([]*types.Field{
			types.NewField(target.Pos, target.LocalPkg.Lookup("c"), target.Type(types.TUNSAFEPTR)),
			types.NewField(target.Pos, target.LocalPkg.Lookup("elem"), target.Type(types.TUNSAFEPTR)),
		}))
	}
	return target.ScaseType
}
