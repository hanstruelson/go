// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"fmt"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

// DeclFunc declares the parameters for fn and adds it to target.Funcs.
func DeclFunc(target *ir.Package, fn *ir.Func) {
	fn.DeclareParams(true)
	fn.Target = target
	fn.Nname.Defn = fn
	target.Funcs = append(target.Funcs, fn)
}

// TempAt makes a new Node off the books.
//
// N.B., the new Node is a function-local variable defaulting to function scope.
// It helps in some cases if an ODCL is also created and placed in a narrower scope,
// such as if the variable can be used in a loop body and potentially escape.
// TODO: Consider some mechanism to more conveniently create a block scoped temporary.
func TempAt(pos src.XPos, curfn *ir.Func, typ *types.Type) *ir.Name {
	if curfn == nil {
		base.FatalfAt(pos, "no curfn for TempAt")
	}
	if typ == nil {
		base.FatalfAt(pos, "TempAt called with nil type")
	}
	if typ.Kind() == types.TFUNC && typ.Recv() != nil {
		base.FatalfAt(pos, "misuse of method type: %v", typ)
	}
	types.CalcSize(typ)

	sym := &types.Sym{
		Name: autotmpname(curfn.Target, len(curfn.Dcl)),
		Pkg:  curfn.Target.LocalPkg,
	}
	name := curfn.NewLocal(pos, sym, typ)
	name.SetEsc(ir.EscNever)
	name.SetUsed(true)
	name.SetAutoTemp(true)

	return name
}

// autotmpname returns the name for an autotmp variable numbered n.
func autotmpname(target *ir.Package, n int) string {
	if target == nil {
		return fmt.Sprintf(".autotmp_%d", n)
	}
	// Grow the target's temporary-name cache, if needed.
	if n >= len(target.AutoTmpNames) {
		target.AutoTmpNames = append(target.AutoTmpNames, make([]string, n+1-len(target.AutoTmpNames))...)
		target.AutoTmpNames = target.AutoTmpNames[:cap(target.AutoTmpNames)]
	}

	s := target.AutoTmpNames[n]
	if s == "" {
		// Give each tmp a different name so that they can be registerized.
		// Add a preceding . to avoid clashing with legal names.
		prefix := ".autotmp_%d"

		s = fmt.Sprintf(prefix, n)
		target.AutoTmpNames[n] = s
	}
	return s
}

// f is method type, with receiver.
// return function type, receiver as first argument (or not).
func NewMethodType(pos src.XPos, sig *types.Type, recv *types.Type) *types.Type {
	nrecvs := 0
	if recv != nil {
		nrecvs++
	}

	// TODO(mdempsky): Move this function to types.
	// TODO(mdempsky): Preserve positions, names, and package from sig+recv.

	params := make([]*types.Field, nrecvs+sig.NumParams())
	if recv != nil {
		params[0] = types.NewField(pos, nil, recv)
	}
	for i, param := range sig.Params() {
		d := types.NewField(pos, nil, param.Type)
		d.SetIsDDD(param.IsDDD())
		params[nrecvs+i] = d
	}

	results := make([]*types.Field, sig.NumResults())
	for i, t := range sig.Results() {
		results[i] = types.NewField(pos, nil, t.Type)
	}

	return types.NewSignature(nil, params, results)
}
