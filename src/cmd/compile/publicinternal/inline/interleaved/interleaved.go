// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package interleaved implements the interleaved devirtualization and
// inlining pass.
package interleaved

import (
	"cmd/compile/publicinternal/devirtualize"
	"cmd/compile/publicinternal/inline"
	"cmd/compile/publicinternal/inline/inlheur"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/pgoir"
	"fmt"
)

// DevirtualizeAndInlinePackage interleaves devirtualization and inlining on
// all functions within pkg.
func DevirtualizeAndInlinePackage(pkg *ir.Package, profile *pgoir.Profile, inliner *inline.Context, inlineCall inline.InlineCallFunc) {
	if pkg.Flags.W > 1 {
		for _, fn := range pkg.Funcs {
			s := fmt.Sprintf("\nbefore devirtualize-and-inline %v", fn.Sym())
			ir.DumpList(s, fn.Body)
		}
	}

	if profile != nil && pkg.Debug.PGODevirtualize > 0 {
		// TODO(mdempsky): Integrate into DevirtualizeAndInlineFunc below.
		ir.VisitFuncsBottomUp(pkg.Funcs, func(list []*ir.Func, recursive bool) {
			for _, fn := range list {
				devirtualize.ProfileGuided(fn, profile)
			}
		})
	}

	if pkg.Flags.LowerL != 0 {
		inlheur.SetupScoreAdjustments(inliner.Heuristics())
	}

	var inlProfile *pgoir.Profile // copy of profile for inlining
	if pkg.Debug.PGOInline != 0 {
		inlProfile = profile
	}

	// First compute inlinability of all functions in the package.
	inline.CanInlineFuncs(inliner, pkg.Funcs, inlProfile, pkg)

	inlState := make(map[*ir.Func]*inlClosureState)
	calleeUseCounts := make(map[*ir.Func]int)

	var state devirtualize.State

	// Pre-process all the functions, adding parentheses around call sites and starting their "inl state".
	for _, fn := range pkg.Funcs {
		bigCaller := pkg.Flags.LowerL != 0 && inline.IsBigFunc(fn)
		if bigCaller && pkg.Flags.LowerM > 1 {
			fmt.Printf("%v: function %v considered 'big'; reducing max cost of inlinees\n", ir.LineFor(fn.Target, fn), fn)
		}

		s := &inlClosureState{bigCaller: bigCaller, profile: profile, inliner: inliner, inlineCall: inlineCall, fn: fn, callSites: make(map[*ir.ParenExpr]bool), useCounts: calleeUseCounts}
		s.parenthesize()
		inlState[fn] = s

		// Do a first pass at counting call sites.
		for i := range s.parens {
			s.resolve(&state, i)
		}
	}

	ir.VisitFuncsBottomUp(pkg.Funcs, func(list []*ir.Func, recursive bool) {

		anyInlineHeuristics := false

		// inline heuristics, placed here because they have static state and that's what seems to work.
		for _, fn := range list {
			if pkg.Flags.LowerL != 0 {
				if inlheur.Enabled(pkg) && !fn.Wrapper() {
					inlheur.ScoreCalls(inliner.Heuristics(), fn)
					anyInlineHeuristics = true
				}
				if pkg.Debug.DumpInlFuncProps != "" && !fn.Wrapper() {
					inlheur.DumpFuncProps(inliner.Heuristics(), fn, pkg.Debug.DumpInlFuncProps)
				}
			}
		}

		if anyInlineHeuristics {
			defer inlheur.ScoreCallsCleanup(inliner.Heuristics())
		}

		// Iterate to a fixed point over all the functions.
		done := false
		for !done {
			done = true
			for _, fn := range list {
				s := inlState[fn]

				ir.WithPos(fn, func() {
					l1 := len(s.parens)
					l0 := 0

					// Batch iterations so that newly discovered call sites are
					// resolved in a batch before inlining attempts.
					// Do this to avoid discovering new closure calls 1 at a time
					// which might cause first call to be seen as a single (high-budget)
					// call before the second is observed.
					for {
						for i := l0; i < l1; i++ { // can't use "range parens" here
							paren := s.parens[i]
							if origCall, inlinedCall := s.edit(&state, i); inlinedCall != nil {
								// Update AST and recursively mark nodes.
								paren.X = inlinedCall
								ir.EditChildren(inlinedCall, s.mark) // mark may append to parens
								state.InlinedCall(s.fn, origCall, inlinedCall)
								done = false
							}
						}
						l0, l1 = l1, len(s.parens)
						if l0 == l1 {
							break
						}
						for i := l0; i < l1; i++ {
							s.resolve(&state, i)
						}

					}

				}) // WithFunc

			}
		}
	})

	if pkg.Flags.LowerL != 0 {
		if pkg.Debug.DumpInlFuncProps != "" {
			inlheur.DumpFuncProps(inliner.Heuristics(), nil, pkg.Debug.DumpInlFuncProps)
		}
		if inlheur.Enabled(pkg) {
			inline.PostProcessCallSites(inliner, inlProfile)
			inlheur.TearDown(inliner.Heuristics())
		}
	}

	// remove parentheses
	for _, fn := range pkg.Funcs {
		inlState[fn].unparenthesize()
	}

}

// DevirtualizeAndInlineFunc interleaves devirtualization and inlining
// on a single function.
func DevirtualizeAndInlineFunc(fn *ir.Func, profile *pgoir.Profile, inliner *inline.Context, inlineCall inline.InlineCallFunc) {
	ir.WithPos(fn, func() {
		if fn.Target.Flags.LowerL != 0 {
			if inlheur.Enabled(fn.Target) && !fn.Wrapper() {
				inlheur.ScoreCalls(inliner.Heuristics(), fn)
				defer inlheur.ScoreCallsCleanup(inliner.Heuristics())
			}
			if fn.Target.Debug.DumpInlFuncProps != "" && !fn.Wrapper() {
				inlheur.DumpFuncProps(inliner.Heuristics(), fn, fn.Target.Debug.DumpInlFuncProps)
			}
		}

		bigCaller := fn.Target.Flags.LowerL != 0 && inline.IsBigFunc(fn)
		if bigCaller && fn.Target.Flags.LowerM > 1 {
			fmt.Printf("%v: function %v considered 'big'; reducing max cost of inlinees\n", ir.LineFor(fn.Target, fn), fn)
		}

		s := &inlClosureState{bigCaller: bigCaller, profile: profile, inliner: inliner, inlineCall: inlineCall, fn: fn, callSites: make(map[*ir.ParenExpr]bool), useCounts: make(map[*ir.Func]int)}
		s.parenthesize()
		s.fixpoint()
		s.unparenthesize()
	})
}

type callSite struct {
	fn         *ir.Func
	whichParen int
}

type inlClosureState struct {
	fn         *ir.Func
	profile    *pgoir.Profile
	inliner    *inline.Context
	inlineCall inline.InlineCallFunc
	callSites  map[*ir.ParenExpr]bool // callSites[p] == "p appears in parens" (do not append again)
	resolved   []*ir.Func             // for each call in parens, the resolved target of the call
	useCounts  map[*ir.Func]int       // shared among all InlClosureStates
	parens     []*ir.ParenExpr
	bigCaller  bool
}

// resolve attempts to resolve a call to a potentially inlineable callee
// and updates use counts on the callees.  Returns the call site count
// for that callee.
func (s *inlClosureState) resolve(state *devirtualize.State, i int) (*ir.Func, int) {
	p := s.parens[i]
	if i < len(s.resolved) {
		if callee := s.resolved[i]; callee != nil {
			return callee, s.useCounts[callee]
		}
	}
	n := p.X
	call, ok := n.(*ir.CallExpr)
	if !ok { // previously inlined
		return nil, -1
	}
	devirtualize.StaticCall(s.fn, state, call)
	if callee := inline.InlineCallTarget(s.inliner, s.fn, call, s.profile); callee != nil {
		for len(s.resolved) <= i {
			s.resolved = append(s.resolved, nil)
		}
		s.resolved[i] = callee
		c := s.useCounts[callee] + 1
		s.useCounts[callee] = c
		return callee, c
	}
	return nil, 0
}

func (s *inlClosureState) edit(state *devirtualize.State, i int) (*ir.CallExpr, *ir.InlinedCallExpr) {
	n := s.parens[i].X
	call, ok := n.(*ir.CallExpr)
	if !ok {
		return nil, nil
	}
	// This is redundant with earlier calls to
	// resolve, but because things can change it
	// must be re-checked.
	callee, count := s.resolve(state, i)
	if count <= 0 {
		return nil, nil
	}
	if inlCall := inline.TryInlineCall(s.inliner, s.fn, call, s.bigCaller, s.profile, count == 1 && callee.ClosureParent != nil, s.inlineCall); inlCall != nil {
		return call, inlCall
	}
	return nil, nil
}

// Mark inserts parentheses, and is called repeatedly.
// These inserted parentheses mark the call sites where
// inlining will be attempted.
func (s *inlClosureState) mark(n ir.Node) ir.Node {
	// Consider the expression "f(g())". We want to be able to replace
	// "g()" in-place with its inlined representation. But if we first
	// replace "f(...)" with its inlined representation, then "g()" will
	// instead appear somewhere within this new AST.
	//
	// To mitigate this, each matched node n is wrapped in a ParenExpr,
	// so we can reliably replace n in-place by assigning ParenExpr.X.
	// It's safe to use ParenExpr here, because typecheck already
	// removed them all.

	p, _ := n.(*ir.ParenExpr)
	if p != nil && s.callSites[p] {
		return n // already visited n.X before wrapping
	}

	if p != nil {
		n = p.X // in this case p was copied in from a (marked) inlined function, this is a new unvisited node.
	}

	ok := match(n)

	// can't wrap TailCall's child into ParenExpr
	if t, ok := n.(*ir.TailCallStmt); ok {
		ir.EditChildren(t.Call, s.mark)
	} else {
		ir.EditChildren(n, s.mark)
	}

	if ok {
		if p == nil {
			p = ir.NewParenExpr(n.Pos(), n)
			p.SetType(n.Type())
			p.SetTypecheck(n.Typecheck())
			s.callSites[p] = true
		}

		s.parens = append(s.parens, p)
		n = p
	} else if p != nil {
		n = p // didn't change anything, restore n
	}
	return n
}

// parenthesize applies s.mark to all the nodes within
// s.fn to mark calls and simplify rewriting them in place.
func (s *inlClosureState) parenthesize() {
	ir.EditChildren(s.fn, s.mark)
}

func (s *inlClosureState) unparenthesize() {
	if s == nil {
		return
	}
	if len(s.parens) == 0 {
		return // short circuit
	}

	var unparen func(ir.Node) ir.Node
	unparen = func(n ir.Node) ir.Node {
		if paren, ok := n.(*ir.ParenExpr); ok {
			n = paren.X
		}
		ir.EditChildren(n, unparen)
		return n
	}
	ir.EditChildren(s.fn, unparen)
}

// fixpoint repeatedly edits a function until it stabilizes, returning
// whether anything changed in any of the fixpoint iterations.
//
// It applies s.edit(n) to each node n within the parentheses in s.parens.
// If s.edit(n) returns nil, no change is made. Otherwise, the result
// replaces n in fn's body, and fixpoint iterates at least once more.
//
// After an iteration where all edit calls return nil, fixpoint
// returns.
func (s *inlClosureState) fixpoint() bool {
	changed := false
	var state devirtualize.State
	ir.WithPos(s.fn, func() {
		done := false
		for !done {
			done = true
			for i := 0; i < len(s.parens); i++ { // can't use "range parens" here
				paren := s.parens[i]
				if origCall, inlinedCall := s.edit(&state, i); inlinedCall != nil {
					// Update AST and recursively mark nodes.
					paren.X = inlinedCall
					ir.EditChildren(inlinedCall, s.mark) // mark may append to parens
					state.InlinedCall(s.fn, origCall, inlinedCall)
					done = false
					changed = true
				}
			}
		}
	})
	return changed
}

func match(n ir.Node) bool {
	switch n := n.(type) {
	case *ir.CallExpr:
		return true
	case *ir.TailCallStmt:
		n.Call.NoInline = true // can't inline yet
	}
	return false
}
