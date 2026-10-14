// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package inlheur

import (
	"cmd/compile/publicinternal/ir"
)

// Context holds the mutable state for inlining heuristics for one package.
type Context struct {
	Target          *ir.Package
	fpmap           map[*ir.Func]fnInlHeur
	callSiteTab     CallSiteTab
	scoreCallsCache scoreCallsCacheType
	allCallSites    CallSiteTab
	dumpBuffer      map[*ir.Func]fnInlHeur
	adjValues       map[scoreAdjustTyp]int
}

// NewContext creates fresh inlining heuristic state for one compilation.
func NewContext(target *ir.Package) *Context {
	return &Context{Target: target, fpmap: make(map[*ir.Func]fnInlHeur), adjValues: defaultAdjValues()}
}
