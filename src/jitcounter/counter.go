// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package counter provides state shared by statically compiled code and the
// linker metadata demonstration.
package counter

var Value int

func Increment() { Value++ }

// GrowStack keeps pointer-bearing frames live across recursive calls so the
// JIT integration example can exercise stack copying through an Eval frame.
func GrowStack(depth int) int {
	var roots [64]*int
	roots[0] = new(int)
	*roots[0] = depth
	if depth > 0 {
		return GrowStack(depth-1) + *roots[0]
	}
	return *roots[0]
}
