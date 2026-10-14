// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/staticdata"
	"cmd/compile/internal/staticinit"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
)

// walkCompLit walks a composite literal node:
// OARRAYLIT, OSLICELIT, OMAPLIT, OSTRUCTLIT (all CompLitExpr), or OPTRLIT (AddrExpr).
func (w *walkState) walkCompLit(n ir.Node, init *ir.Nodes) ir.Node {
	if isStaticCompositeLiteral(w.curfunc.Target, n) && !ssa.CanSSA(int64(w.curfunc.Target.PtrSize), n.Type()) {
		n := n.(*ir.CompLitExpr) // not OPTRLIT
		// n can be directly represented in the read-only data section.
		// Make direct reference to the static data. See issue 12841.
		vstat := readonlystaticname(w.curfunc.Target, n.Type())
		w.fixedlit(initKindStatic, n, vstat, init)
		return typecheck.Expr(w.curfunc, vstat)
	}
	var_ := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, n.Type())
	w.anylit(n, var_, init)
	return var_
}

// readonlystaticname returns a name backed by a read-only static data symbol.
func readonlystaticname(target *ir.Package, t *types.Type) *ir.Name {
	n := staticinit.StaticName(target, t)
	n.MarkReadonly()
	n.Linksym().Set(obj.AttrContentAddressable, true)
	n.Linksym().Set(obj.AttrLocal, true)
	return n
}

func isSimpleName(nn ir.Node) bool {
	if nn.Op() != ir.ONAME || ir.IsBlank(nn) {
		return false
	}
	n := nn.(*ir.Name)
	return n.OnStack()
}

// initGenType is a bitmap indicating the types of generation that will occur for a static value.
type initGenType uint8

const (
	initDynamic initGenType = 1 << iota // contains some dynamic values, for which init code will be generated
	initConst                           // contains some constant values, which may be written into data symbols
)

// getdyn calculates the initGenType for n.
// If top is false, getdyn is recursing.
func getdyn(target *ir.Package, n ir.Node, top bool) initGenType {
	switch n.Op() {
	default:
		if isStaticLiteral(target, n) {
			return initConst
		}
		return initDynamic

	case ir.OSLICELIT:
		n := n.(*ir.CompLitExpr)
		if !top {
			return initDynamic
		}
		if n.Len/4 > int64(len(n.List)) {
			// <25% of entries have explicit values.
			// Very rough estimation, it takes 4 bytes of instructions
			// to initialize 1 byte of result. So don't use a static
			// initializer if the dynamic initialization code would be
			// smaller than the static value.
			// See issue 23780.
			return initDynamic
		}

	case ir.OARRAYLIT, ir.OSTRUCTLIT:
	}
	lit := n.(*ir.CompLitExpr)

	var mode initGenType
	for _, n1 := range lit.List {
		switch n1.Op() {
		case ir.OKEY:
			n1 = n1.(*ir.KeyExpr).Value
		case ir.OSTRUCTKEY:
			n1 = n1.(*ir.StructKeyExpr).Value
		}
		mode |= getdyn(target, n1, false)
		if mode == initDynamic|initConst {
			break
		}
	}
	return mode
}

// isStaticLiteral reports whether n is a compile-time (non-composite)
// constant, which can be represented in the read-only data section.
func isStaticLiteral(target *ir.Package, n ir.Node) bool {
	// A string reference requires a relocation, not allowed
	// in static data in FIPS mode.
	return ir.IsConstNode(n) && !(target.Ctxt.IsFIPS() && n.Type().IsString())
}

// isStaticCompositeLiteral reports whether a composite literal n
// is a compile-time constant, which can be represented in the
// read-only data section.
func isStaticCompositeLiteral(target *ir.Package, n ir.Node) bool {
	switch n.Op() {
	case ir.OSLICELIT:
		return false
	case ir.OARRAYLIT:
		n := n.(*ir.CompLitExpr)
		for _, r := range n.List {
			if r.Op() == ir.OKEY {
				r = r.(*ir.KeyExpr).Value
			}
			if !isStaticCompositeLiteral(target, r) {
				return false
			}
		}
		return true
	case ir.OSTRUCTLIT:
		n := n.(*ir.CompLitExpr)
		for _, r := range n.List {
			r := r.(*ir.StructKeyExpr)
			if !isStaticCompositeLiteral(target, r.Value) {
				return false
			}
		}
		return true
	case ir.ONIL:
		return true
	case ir.OLITERAL:
		return isStaticLiteral(target, n)
	case ir.OCONVIFACE:
		// See staticinit.Schedule.StaticAssign's OCONVIFACE case for comments.
		if target.Ctxt.IsFIPS() && target.Ctxt.Flag_shared {
			return false
		}
		n := n.(*ir.ConvExpr)
		val := ir.Node(n)
		for val.Op() == ir.OCONVIFACE {
			val = val.(*ir.ConvExpr).X
		}
		if val.Type().IsInterface() {
			return val.Op() == ir.ONIL
		}
		if types.IsDirectIface(val.Type()) && val.Op() == ir.ONIL {
			return true
		}
		return isStaticCompositeLiteral(target, val)
	}
	return false
}

// initKind is a kind of static initialization: static, dynamic, or local.
// Static initialization represents literals and
// literal components of composite literals.
// Dynamic initialization represents non-literals and
// non-literal components of composite literals.
// LocalCode initialization represents initialization
// that occurs purely in generated code local to the function of use.
// Initialization code is sometimes generated in passes,
// first static then dynamic.
type initKind uint8

const (
	initKindStatic initKind = iota + 1
	initKindDynamic
	initKindLocalCode
)

// fixedlit handles struct, array, and slice literals.
// TODO: expand documentation.
func (w *walkState) fixedlit(kind initKind, n *ir.CompLitExpr, var_ ir.Node, init *ir.Nodes) {
	isBlank := var_ == w.curfunc.Target.BlankNode
	var splitnode func(ir.Node) (a ir.Node, value ir.Node)
	switch n.Op() {
	case ir.OARRAYLIT, ir.OSLICELIT:
		var k int64
		splitnode = func(r ir.Node) (ir.Node, ir.Node) {
			if r.Op() == ir.OKEY {
				kv := r.(*ir.KeyExpr)
				k = typecheck.IndexConstFor(w.curfunc.Target, kv.Key)
				r = kv.Value
			}
			a := ir.NewIndexExpr(w.curfunc.Target.Pos, var_, ir.NewInt(w.curfunc.Target.Pos, k))
			k++
			if isBlank {
				return w.curfunc.Target.BlankNode, r
			}
			return a, r
		}
	case ir.OSTRUCTLIT:
		splitnode = func(rn ir.Node) (ir.Node, ir.Node) {
			r := rn.(*ir.StructKeyExpr)
			if r.Sym().IsBlank() || isBlank {
				return w.curfunc.Target.BlankNode, r.Value
			}
			ir.SetPosFor(w.curfunc.Target, r)
			return ir.NewSelectorExpr(w.curfunc.Target.Pos, ir.OXDOT, var_, r.Sym()), r.Value
		}
	default:
		base.Fatalf("fixedlit bad op: %v", n.Op())
	}

	for _, r := range n.List {
		a, value := splitnode(r)
		if a == w.curfunc.Target.BlankNode && !staticinit.AnySideEffects(value) {
			// Discard.
			continue
		}

		switch value.Op() {
		case ir.OSLICELIT:
			value := value.(*ir.CompLitExpr)
			if kind == initKindDynamic {
				w.slicelit(value, a, init)
				continue
			}

		case ir.OARRAYLIT, ir.OSTRUCTLIT:
			value := value.(*ir.CompLitExpr)
			w.fixedlit(kind, value, a, init)
			continue
		}

		islit := isStaticLiteral(w.curfunc.Target, value)
		if (kind == initKindStatic && !islit) || (kind == initKindDynamic && islit) {
			continue
		}

		// build list of assignments: var[index] = expr
		ir.SetPosFor(w.curfunc.Target, a)
		as := ir.NewAssignStmt(w.curfunc.Target.Pos, a, value)
		as = typecheck.Stmt(w.curfunc, as).(*ir.AssignStmt)
		switch kind {
		case initKindStatic:
			genAsStatic(w.curfunc.Target, as)
		case initKindDynamic, initKindLocalCode:
			w.appendWalkStmt(init, w.orderStmtInPlace(as, map[string][]*ir.Name{}))
		default:
			base.Fatalf("fixedlit: bad kind %d", kind)
		}

	}
}

func isSmallSliceLit(n *ir.CompLitExpr) bool {
	if n.Op() != ir.OSLICELIT {
		return false
	}

	return n.Type().Elem().Size() == 0 || n.Len <= ir.MaxSmallArraySize/n.Type().Elem().Size()
}

func (w *walkState) slicelit(n *ir.CompLitExpr, var_ ir.Node, init *ir.Nodes) {
	// make an array type corresponding the number of elements we have
	t := types.NewArray(n.Type().Elem(), n.Len)
	t.SetNoalg(true)
	types.CalcSize(t)

	// recipe for var = []t{...}
	// 1. make a static array
	//	var vstat [...]t
	// 2. assign (data statements) the constant part
	//	vstat = constpart{}
	// 3. make an auto pointer to array and allocate heap to it
	//	var vauto *[...]t = new([...]t)
	// 4. copy the static array to the auto array
	//	*vauto = vstat
	// 5. for each dynamic part assign to the array
	//	vauto[i] = dynamic part
	// 6. assign slice of allocated heap to var
	//	var = vauto[:]
	//
	// an optimization is done if there is no constant part
	//	3. var vauto *[...]t = new([...]t)
	//	5. vauto[i] = dynamic part
	//	6. var = vauto[:]

	// if the literal contains constants,
	// make static initialized array (1),(2)
	var vstat ir.Node

	mode := getdyn(w.curfunc.Target, n, true)
	if mode&initConst != 0 && !isSmallSliceLit(n) {
		vstat = readonlystaticname(w.curfunc.Target, t)
		w.fixedlit(initKindStatic, n, vstat, init)
	}

	// make new auto *array (3 declare)
	vauto := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, types.NewPtr(t))

	// set auto to point at new temp or heap (3 assign)
	var a ir.Node
	if x := n.Prealloc; x != nil {
		// temp allocated during order.go for dddarg
		if !types.Identical(t, x.Type()) {
			panic("dotdotdot base type does not match order's assigned type")
		}
		a = w.initStackTemp(init, x, vstat)
	} else if n.Esc() == ir.EscNone {
		a = w.initStackTemp(init, typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, t), vstat)
	} else {
		a = ir.NewUnaryExpr(w.curfunc.Target.Pos, ir.ONEW, ir.TypeNode(t))
	}
	w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, vauto, a))

	if vstat != nil && n.Prealloc == nil && n.Esc() != ir.EscNone {
		// If we allocated on the heap with ONEW, copy the static to the
		// heap (4). We skip this for stack temporaries, because
		// initStackTemp already handled the copy.
		a = ir.NewStarExpr(w.curfunc.Target.Pos, vauto)
		w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, a, vstat))
	}

	// put dynamics into array (5)
	var index int64
	for _, value := range n.List {
		if value.Op() == ir.OKEY {
			kv := value.(*ir.KeyExpr)
			index = typecheck.IndexConstFor(w.curfunc.Target, kv.Key)
			value = kv.Value
		}
		a := ir.NewIndexExpr(w.curfunc.Target.Pos, vauto, ir.NewInt(w.curfunc.Target.Pos, index))
		a.SetBounded(true)
		index++

		// TODO need to check bounds?

		switch value.Op() {
		case ir.OSLICELIT:
			break

		case ir.OARRAYLIT, ir.OSTRUCTLIT:
			value := value.(*ir.CompLitExpr)
			k := initKindDynamic
			if vstat == nil {
				// Generate both static and dynamic initializations.
				// See issue #31987.
				k = initKindLocalCode
			}
			w.fixedlit(k, value, a, init)
			continue
		}

		if vstat != nil && isStaticLiteral(w.curfunc.Target, value) { // already set by copy from static value
			continue
		}

		// build list of vauto[c] = expr
		ir.SetPosFor(w.curfunc.Target, value)
		as := ir.NewAssignStmt(w.curfunc.Target.Pos, a, value)
		w.appendWalkStmt(init, w.orderStmtInPlace(typecheck.Stmt(w.curfunc, as), map[string][]*ir.Name{}))
	}

	// make slice out of heap (6)
	a = ir.NewAssignStmt(w.curfunc.Target.Pos, var_, ir.NewSliceExpr(w.curfunc.Target.Pos, ir.OSLICE, vauto, nil, nil, nil))
	w.appendWalkStmt(init, w.orderStmtInPlace(typecheck.Stmt(w.curfunc, a), map[string][]*ir.Name{}))
}

func (w *walkState) maplit(n *ir.CompLitExpr, m ir.Node, init *ir.Nodes) {
	// make the map var
	args := []ir.Node{ir.TypeNode(n.Type()), ir.NewInt(w.curfunc.Target.Pos, n.Len+int64(len(n.List)))}
	a := typecheck.Expr(w.curfunc, ir.NewCallExpr(w.curfunc.Target.Pos, ir.OMAKE, nil, args)).(*ir.MakeExpr)
	a.RType = n.RType
	a.SetEsc(n.Esc())
	w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, m, a))

	entries := n.List

	// The order pass already removed any dynamic (runtime-computed) entries.
	// All remaining entries are static. Double-check that.
	for _, r := range entries {
		r := r.(*ir.KeyExpr)
		if !isStaticCompositeLiteral(w.curfunc.Target, r.Key) || !isStaticCompositeLiteral(w.curfunc.Target, r.Value) {
			base.Fatalf("maplit: entry is not a literal: %v", r)
		}
	}

	if len(entries) > 25 {
		// For a large number of entries, put them in an array and loop.

		// build types [count]Tindex and [count]Tvalue
		tk := types.NewArray(n.Type().Key(), int64(len(entries)))
		tk.SetNoalg(true)
		te := types.NewArray(n.Type().Elem(), int64(len(entries)))
		te.SetNoalg(true)

		types.CalcSize(tk)
		types.CalcSize(te)

		// make and initialize static arrays
		vstatk := readonlystaticname(w.curfunc.Target, tk)
		vstate := readonlystaticname(w.curfunc.Target, te)

		datak := ir.NewCompLitExpr(w.curfunc.Target.Pos, ir.OARRAYLIT, nil, nil)
		datae := ir.NewCompLitExpr(w.curfunc.Target.Pos, ir.OARRAYLIT, nil, nil)
		for _, r := range entries {
			r := r.(*ir.KeyExpr)
			datak.List.Append(r.Key)
			datae.List.Append(r.Value)
		}
		w.fixedlit(initKindStatic, datak, vstatk, init)
		w.fixedlit(initKindStatic, datae, vstate, init)

		// loop adding structure elements to map
		// for i = 0; i < len(vstatk); i++ {
		//	map[vstatk[i]] = vstate[i]
		// }
		i := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, w.curfunc.Target.Type(types.TINT))
		rhs := ir.NewIndexExpr(w.curfunc.Target.Pos, vstate, i)
		rhs.SetBounded(true)

		kidx := ir.NewIndexExpr(w.curfunc.Target.Pos, vstatk, i)
		kidx.SetBounded(true)

		// typechecker rewrites OINDEX to OINDEXMAP
		lhs := typecheck.AssignExpr(w.curfunc, ir.NewIndexExpr(w.curfunc.Target.Pos, m, kidx)).(*ir.IndexExpr)
		base.AssertfAt(lhs.Op() == ir.OINDEXMAP, lhs.Pos(), "want OINDEXMAP, have %+v", lhs)
		lhs.RType = n.RType

		zero := ir.NewAssignStmt(w.curfunc.Target.Pos, i, ir.NewInt(w.curfunc.Target.Pos, 0))
		cond := ir.NewBinaryExpr(w.curfunc.Target.Pos, ir.OLT, i, ir.NewInt(w.curfunc.Target.Pos, tk.NumElem()))
		incr := ir.NewAssignStmt(w.curfunc.Target.Pos, i, ir.NewBinaryExpr(w.curfunc.Target.Pos, ir.OADD, i, ir.NewInt(w.curfunc.Target.Pos, 1)))

		var body ir.Node = ir.NewAssignStmt(w.curfunc.Target.Pos, lhs, rhs)
		body = typecheck.Stmt(w.curfunc, body)
		body = w.orderStmtInPlace(body, map[string][]*ir.Name{})

		loop := ir.NewForStmt(w.curfunc.Target.Pos, nil, cond, incr, nil, false)
		loop.Body = []ir.Node{body}
		loop.SetInit([]ir.Node{zero})

		w.appendWalkStmt(init, loop)
		return
	}
	// For a small number of entries, just add them directly.

	// Build list of var[c] = expr.
	// Use temporaries so that mapassign1 can have addressable key, elem.
	// TODO(josharian): avoid map key temporaries for mapfast_* assignments with literal keys.
	// TODO(khr): assign these temps in order phase so we can reuse them across multiple maplits?
	tmpkey := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, m.Type().Key())
	tmpelem := typecheck.TempAt(w.curfunc.Target.Pos, w.curfunc, m.Type().Elem())

	for _, r := range entries {
		r := r.(*ir.KeyExpr)
		index, elem := r.Key, r.Value

		ir.SetPosFor(w.curfunc.Target, index)
		w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, tmpkey, index))

		ir.SetPosFor(w.curfunc.Target, elem)
		w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, tmpelem, elem))

		ir.SetPosFor(w.curfunc.Target, tmpelem)

		// typechecker rewrites OINDEX to OINDEXMAP
		lhs := typecheck.AssignExpr(w.curfunc, ir.NewIndexExpr(w.curfunc.Target.Pos, m, tmpkey)).(*ir.IndexExpr)
		base.AssertfAt(lhs.Op() == ir.OINDEXMAP, lhs.Pos(), "want OINDEXMAP, have %+v", lhs)
		lhs.RType = n.RType

		var a ir.Node = ir.NewAssignStmt(w.curfunc.Target.Pos, lhs, tmpelem)
		a = typecheck.Stmt(w.curfunc, a)
		a = w.orderStmtInPlace(a, map[string][]*ir.Name{})
		w.appendWalkStmt(init, a)
	}
}

func (w *walkState) anylit(n ir.Node, var_ ir.Node, init *ir.Nodes) {
	t := n.Type()
	switch n.Op() {
	default:
		base.Fatalf("anylit: not lit, op=%v node=%v", n.Op(), n)

	case ir.ONAME:
		n := n.(*ir.Name)
		w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, var_, n))

	case ir.OMETHEXPR:
		n := n.(*ir.SelectorExpr)
		w.anylit(n.FuncName(w.curfunc.Target), var_, init)

	case ir.OPTRLIT:
		n := n.(*ir.AddrExpr)
		if !t.IsPtr() {
			base.Fatalf("anylit: not ptr")
		}

		var r ir.Node
		if n.Prealloc != nil {
			// n.Prealloc is stack temporary used as backing store.
			r = w.initStackTemp(init, n.Prealloc, nil)
		} else {
			r = ir.NewUnaryExpr(w.curfunc.Target.Pos, ir.ONEW, ir.TypeNode(n.X.Type()))
			r.SetEsc(n.Esc())
		}
		w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, var_, r))

		var_ = ir.NewStarExpr(w.curfunc.Target.Pos, var_)
		var_ = typecheck.AssignExpr(w.curfunc, var_)
		w.anylit(n.X, var_, init)

	case ir.OSTRUCTLIT, ir.OARRAYLIT:
		n := n.(*ir.CompLitExpr)
		if !t.IsStruct() && !t.IsArray() {
			base.Fatalf("anylit: not struct/array")
		}

		if isSimpleName(var_) && len(n.List) > 4 {
			// lay out static data
			vstat := readonlystaticname(w.curfunc.Target, t)

			w.fixedlit(initKindStatic, n, vstat, init)

			// copy static to var
			w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, var_, vstat))

			// add expressions to automatic
			w.fixedlit(initKindDynamic, n, var_, init)
			break
		}

		var components int64
		if n.Op() == ir.OARRAYLIT {
			components = t.NumElem()
		} else {
			components = int64(t.NumFields())
		}
		// initialization of an array or struct with unspecified components (missing fields or arrays)
		if isSimpleName(var_) || int64(len(n.List)) < components {
			w.appendWalkStmt(init, ir.NewAssignStmt(w.curfunc.Target.Pos, var_, nil))
		}

		w.fixedlit(initKindLocalCode, n, var_, init)

	case ir.OSLICELIT:
		n := n.(*ir.CompLitExpr)
		w.slicelit(n, var_, init)

	case ir.OMAPLIT:
		n := n.(*ir.CompLitExpr)
		if !t.IsMap() {
			base.Fatalf("anylit: not map")
		}
		w.maplit(n, var_, init)
	}
}

// oaslit handles special composite literal assignments.
// It returns true if n's effects have been added to init,
// in which case n should be dropped from the program by the caller.
func (w *walkState) oaslit(n *ir.AssignStmt, init *ir.Nodes) bool {
	if n.X == nil || n.Y == nil {
		// not a special composite literal assignment
		return false
	}
	if n.X.Type() == nil || n.Y.Type() == nil {
		// not a special composite literal assignment
		return false
	}
	if !isSimpleName(n.X) {
		// not a special composite literal assignment
		return false
	}
	x := n.X.(*ir.Name)
	if !types.Identical(n.X.Type(), n.Y.Type()) {
		// not a special composite literal assignment
		return false
	}
	if x.Addrtaken() {
		// If x is address-taken, the RHS may (implicitly) uses LHS.
		// Not safe to do a special composite literal assignment
		// (which may expand to multiple assignments).
		return false
	}

	switch n.Y.Op() {
	default:
		// not a special composite literal assignment
		return false

	case ir.OSTRUCTLIT, ir.OARRAYLIT, ir.OSLICELIT, ir.OMAPLIT:
		if ir.Any(n.Y, func(y ir.Node) bool { return ir.Uses(y, x) }) {
			// not safe to do a special composite literal assignment if RHS uses LHS.
			return false
		}
		w.anylit(n.Y, n.X, init)
	}

	return true
}

func genAsStatic(target *ir.Package, as *ir.AssignStmt) {
	if as.X.Type() == nil {
		base.Fatalf("genAsStatic as.Left not typechecked")
	}

	name, offset, ok := staticinit.StaticLoc(target, as.X)
	if !ok || (name.Class != ir.PEXTERN && as.X != target.BlankNode) {
		base.Fatalf("genAsStatic: lhs %v", as.X)
	}

	switch r := as.Y; r.Op() {
	case ir.OLITERAL:
		staticdata.InitConst(target, name, offset, r, int(r.Type().Size()))
		return
	case ir.OMETHEXPR:
		r := r.(*ir.SelectorExpr)
		staticdata.InitAddr(target, name, offset, staticdata.FuncLinksym(target, r.FuncName(target)))
		return
	case ir.ONAME:
		r := r.(*ir.Name)
		if r.Offset_ != 0 {
			base.Fatalf("genAsStatic %+v", as)
		}
		if r.Class == ir.PFUNC {
			staticdata.InitAddr(target, name, offset, staticdata.FuncLinksym(target, r))
			return
		}
	}
	base.Fatalf("genAsStatic: rhs %v", as.Y)
}
