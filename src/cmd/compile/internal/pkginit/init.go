// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkginit

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/noder"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/staticinit"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
)

// MakeTask makes an initialization record for the package, if necessary.
// See runtime/proc.go:initTask for its layout.
// The 3 tasks for initialization are:
//  1. Initialize all of the packages the current package depends on.
//  2. Initialize all the variables that have initializers.
//  3. Run any init functions.
func MakeTask(compileCtx *noder.Context) {
	var deps []*obj.LSym // initTask records for packages the current package depends on
	var fns []*obj.LSym  // functions to call for package initialization

	// Find imported packages with init tasks.
	for _, pkg := range compileCtx.Target.Imports {
		n, ok := pkg.Lookup(".inittask").Def.(*ir.Name)
		if !ok {
			continue
		}
		if n.Op() != ir.ONAME || n.Class != ir.PEXTERN {
			base.Fatalf("bad inittask: %v", n)
		}
		deps = append(deps, n.Linksym())
	}
	if compileCtx.Target.Flags.ASan {
		// Make an initialization function to call runtime.asanregisterglobals to register an
		// array of instrumented global variables when -asan is enabled. An instrumented global
		// variable is described by a structure.
		// See the _asan_global structure declared in src/runtime/asan/asan.go.
		//
		// func init {
		// 		var globals []_asan_global {...}
		// 		asanregisterglobals(&globals[0], len(globals))
		// }
		for _, n := range compileCtx.Target.Externs {
			if canInstrumentGlobal(compileCtx.Target, n) {
				name := n.Sym().Name
				if compileCtx.Target.InstrumentedGlobalsMap == nil {
					compileCtx.Target.InstrumentedGlobalsMap = make(map[string]ir.Node)
				}
				compileCtx.Target.InstrumentedGlobalsMap[name] = n
				compileCtx.Target.InstrumentedGlobalsSlice = append(compileCtx.Target.InstrumentedGlobalsSlice, n)
			}
		}
		ni := len(compileCtx.Target.InstrumentedGlobalsMap)
		if ni != 0 {
			// Make an init._ function.
			pos := compileCtx.Target.AutoPos
			ir.SetPosValue(compileCtx.Target, pos)

			sym := compileCtx.Renameinit()
			fnInit := ir.NewFunc(compileCtx.Target, pos, pos, sym, types.NewSignature(nil, nil, nil))
			typecheck.DeclFunc(compileCtx.Target, fnInit)

			// Get an array of instrumented global variables.
			globals := instrumentGlobals(compileCtx.Target, fnInit)

			// Call runtime.asanregisterglobals function to poison redzones.
			// runtime.asanregisterglobals(unsafe.Pointer(&globals[0]), ni)
			asancall := ir.NewCallExpr(compileCtx.Target.Pos, ir.OCALL, typecheck.LookupRuntime(compileCtx.Target, "asanregisterglobals"), nil)
			asancall.Args.Append(typecheck.ConvNop(fnInit, typecheck.NodAddr(fnInit,
				ir.NewIndexExpr(compileCtx.Target.Pos, globals, ir.NewInt(compileCtx.Target.Pos, 0))), compileCtx.Target.Type(types.TUNSAFEPTR)))
			asancall.Args.Append(typecheck.DefaultLitFor(compileCtx.Target, ir.NewInt(compileCtx.Target.Pos, int64(ni)), compileCtx.Target.Type(types.TUINTPTR)))

			fnInit.Body.Append(asancall)
			typecheck.Stmts(fnInit, fnInit.Body)

			compileCtx.Target.Inits = append(compileCtx.Target.Inits, fnInit)
		}
	}

	// Record user init functions.
	for _, fn := range compileCtx.Target.Inits {
		if staticinit.CanOptimize(fn) {
			s := staticinit.Schedule{
				Plans: make(map[ir.Node]*staticinit.Plan),
				Temps: make(map[ir.Node]*ir.Name),
			}
			for _, n := range fn.Body {
				s.StaticInit(fn, n)
			}
			fn.Body = s.Out
			ir.WithPos(fn, func() {
				typecheck.Stmts(fn, fn.Body)
			})

			if len(fn.Body) == 0 {
				fn.Body = []ir.Node{ir.NewBlockStmt(src.NoXPos, nil)}
			}
		}

		// Skip init functions with empty bodies.
		if len(fn.Body) == 1 {
			if stmt := fn.Body[0]; stmt.Op() == ir.OBLOCK && len(stmt.(*ir.BlockStmt).List) == 0 {
				continue
			}
		}
		fns = append(fns, fn.Nname.Linksym())
	}

	if len(deps) == 0 && len(fns) == 0 && compileCtx.Target.LocalPkg.Path != "main" && compileCtx.Target.LocalPkg.Path != "runtime" {
		return // nothing to initialize
	}

	// Make an .inittask structure.
	sym := typecheck.Lookup(compileCtx.Target, ".inittask")
	task := ir.NewNameAt(compileCtx.Target.Pos, sym, compileCtx.Target.Type(types.TUINT8)) // fake type
	task.Class = ir.PEXTERN
	sym.Def = task
	lsym := task.Linksym()
	ot := 0
	ot = objw.Uint32(compileCtx.Target.Ctxt, lsym, ot, 0) // state: not initialized yet
	ot = objw.Uint32(compileCtx.Target.Ctxt, lsym, ot, uint32(len(fns)))
	for _, f := range fns {
		ot = objw.SymPtr(compileCtx.Target.Ctxt, lsym, ot, f, 0)
	}

	// Add relocations which tell the linker all of the packages
	// that this package depends on (and thus, all of the packages
	// that need to be initialized before this one).
	for _, d := range deps {
		lsym.AddRel(compileCtx.Target.Ctxt, obj.Reloc{Type: objabi.R_INITORDER, Sym: d})
	}
	// An initTask has pointers, but none into the Go heap.
	// It's not quite read only, the state field must be modifiable.
	objw.Global(compileCtx.Target.Ctxt, lsym, int32(ot), obj.NOPTR)
}
