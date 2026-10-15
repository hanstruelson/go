// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/objw"
	"cmd/compile/publicinternal/ssa"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
)

// ArchInfoFor returns the backend architecture configuration for target.
func ArchInfoFor(target *ir.Package) *ArchInfo {
	if arch, ok := target.ArchInfo.(*ArchInfo); ok {
		return arch
	}
	panic("compiler target is missing architecture information")
}

// interface to back end

type ArchInfo struct {
	LinkArch *obj.LinkArch
	// TargetArch is an optional requested GOARCH supplied by reusable
	// compilation requests. Architecture initializers with multiple variants
	// use it instead of the process build configuration when it is non-empty.
	TargetArch string
	// TargetFeatures contains architecture feature levels for this request.
	TargetFeatures ir.ArchFeatures
	// InitError reports an architecture configuration failure. Initializers
	// must return this error instead of terminating the process so reusable
	// compiler clients can report it to the caller.
	InitError error

	REGSP     int
	MAXWIDTH  int64
	SoftFloat bool

	PadFrame func(int64) int64

	// ZeroRange zeroes a range of memory the on stack.
	//  - it is only called at function entry
	//  - it is ok to clobber (non-arg) registers.
	//  - currently used only for small things, so it can be simple.
	//    - pointers to heap-allocated return values
	//    - open-coded deferred functions
	// (Max size in make.bash is 40 bytes.)
	ZeroRange func(*objw.Progs, *obj.Prog, int64, int64, *uint32) *obj.Prog

	Ginsnop func(*objw.Progs) *obj.Prog

	// SSAMarkMoves marks any MOVXconst ops that need to avoid clobbering flags.
	SSAMarkMoves func(*State, *ssa.Block)

	// SSAGenValue emits Prog(s) for the Value.
	SSAGenValue func(*State, *ssa.Value)

	// SSAGenBlock emits end-of-block Progs. SSAGenValue should be called
	// for all values in the block before SSAGenBlock.
	SSAGenBlock func(s *State, b, next *ssa.Block)

	// LoadRegResult emits instructions that loads register-assigned result
	// at n+off (n is PPARAMOUT) to register reg. The result is already in
	// memory. Used in open-coded defer return path.
	LoadRegResult func(s *State, f *ssa.Func, t *types.Type, reg int16, n *ir.Name, off int64) *obj.Prog

	// SpillArgReg emits instructions that spill reg to n+off.
	SpillArgReg func(pp *objw.Progs, p *obj.Prog, f *ssa.Func, t *types.Type, reg int16, n *ir.Name, off int64) *obj.Prog

	// SSAGenFinish, if non-nil, is called after all of a function's Progs
	// have been generated and defframe has run: branch and jump-table
	// targets are resolved and the frame size is final. It allows the
	// backend to run a last Prog-level pass. Used on arm64 to fuse adjacent
	// spill/reload MOVDs into STP/LDP.
	SSAGenFinish func(pp *objw.Progs)
}
