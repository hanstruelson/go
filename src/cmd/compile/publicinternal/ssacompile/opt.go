// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"cmd/compile/publicinternal/ssa"
	"cmd/compile/publicinternal/ssa/rewrite/divisible"
	"cmd/compile/publicinternal/ssa/rewrite/divmod"
	"cmd/compile/publicinternal/ssa/rewrite/generic"
)

// machine-independent optimization.
func opt(f *ssa.Func) {
	applyRewrite(f, generic.RewriteBlock, generic.RewriteValue, ssa.RemoveDeadValues)
}

func divisiblePass(f *ssa.Func) {
	applyRewrite(f, divisible.RewriteBlock, divisible.RewriteValue, ssa.RemoveDeadValues)
}

func divmodPass(f *ssa.Func) {
	applyRewrite(f, divmod.RewriteBlock, divmod.RewriteValue, ssa.RemoveDeadValues)
}
