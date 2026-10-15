// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
)

// LookupRuntime returns a function or variable declared in
// _builtin/runtime.go. If types_ is non-empty, successive occurrences
// of the "any" placeholder type will be substituted.
func LookupRuntime(target *ir.Package, name string, types_ ...*types.Type) *ir.Name {
	s := target.RuntimePkg.Lookup(name)
	if s == nil || s.Def == nil {
		fatalf(target, "LookupRuntime: can't find runtime.%s", name)
	}
	n := s.Def.(*ir.Name)
	if len(types_) != 0 {
		n = substArgTypes(n, types_...)
	}
	return n
}

// SubstArgTypes substitutes the given list of types for
// successive occurrences of the "any" placeholder in the
// type syntax expression n.Type.
func substArgTypes(old *ir.Name, types_ ...*types.Type) *ir.Name {
	for _, t := range types_ {
		types.CalcSize(t)
	}
	n := ir.NewNameAt(old.Pos(), old.Sym(), types.SubstAny(old.Type(), &types_))
	n.Class = old.Class
	n.Func = old.Func
	if len(types_) > 0 {
		fatalf(ir.TargetOf(old), "SubstArgTypes: too many argument types")
	}
	return n
}

// AutoLabel generates a new Name node for use with
// an automatically generated label.
// prefix is a short mnemonic (e.g. ".s" for switch)
// to help with debugging.
// It should begin with "." to avoid conflicts with
// user labels.
func AutoLabel(curfunc *ir.Func, prefix string) *types.Sym {
	if prefix[0] != '.' {
		fatalf(targetForFunc(curfunc), "autolabel prefix must start with '.', have %q", prefix)
	}
	fn := curfunc
	if curfunc == nil {
		fatalf(targetForFunc(curfunc), "autolabel outside function")
	}
	n := fn.Label
	fn.Label++
	return LookupNum(fn.Target, prefix, int(n))
}

func Lookup(target *ir.Package, name string) *types.Sym {
	return target.LocalPkg.Lookup(name)
}

// InitRuntime loads the definitions for the low-level runtime functions,
// so that the compiler can generate calls to them,
// but does not make them visible to user code.
func InitRuntime(target *ir.Package, timer *base.Timings) {
	timer.Start("fe", "loadsys")
	target.RuntimePkg = target.Pkg("go.runtime", "runtime")
	target.RuntimePkg.Prefix = "runtime"

	typs := runtimeTypes(target)
	for _, d := range &runtimeDecls {
		sym := target.RuntimePkg.Lookup(d.name)
		typ := typs[d.typ]
		switch d.tag {
		case funcTag:
			importfunc(target, sym, typ)
		case varTag:
			importvar(sym, typ)
		default:
			fatalf(target, "unhandled declaration tag %v", d.tag)
		}
	}
}

// LookupRuntimeFuncFor resolves a runtime symbol in target's linker context.
func LookupRuntimeFuncFor(target *ir.Package, name string) *obj.LSym {
	return LookupRuntimeABIFor(target, name, obj.ABIInternal)
}

// LookupRuntimeVarFor resolves a runtime variable in target's linker context.
func LookupRuntimeVarFor(target *ir.Package, name string) *obj.LSym {
	return LookupRuntimeABIFor(target, name, obj.ABI0)
}

// LookupRuntimeABIFor resolves a runtime symbol using target's linker context.
func LookupRuntimeABIFor(target *ir.Package, name string, abi obj.ABI) *obj.LSym {
	if target == nil || target.TypeContext == nil {
		panic("LookupRuntimeABIFor requires a target type context")
	}
	return target.TypeContext.LinkSym("runtime", name, abi)
}

// InitCoverage loads the definitions for routines called
// by code coverage instrumentation (similar to InitRuntime above).
func InitCoverage(target *ir.Package) {
	typs := coverageTypes(target)
	for _, d := range &coverageDecls {
		sym := target.CoveragePkg.Lookup(d.name)
		typ := typs[d.typ]
		switch d.tag {
		case funcTag:
			importfunc(target, sym, typ)
		case varTag:
			importvar(sym, typ)
		default:
			fatalf(target, "unhandled declaration tag %v", d.tag)
		}
	}
}

// LookupCoverage looks up the Go function 'name' in package
// runtime/coverage. This function must follow the internal calling
// convention.
func LookupCoverage(target *ir.Package, name string) *ir.Name {
	sym := target.CoveragePkg.Lookup(name)
	if sym == nil {
		fatalf(target, "LookupCoverage: can't find runtime/coverage.%s", name)
	}
	return sym.Def.(*ir.Name)
}
