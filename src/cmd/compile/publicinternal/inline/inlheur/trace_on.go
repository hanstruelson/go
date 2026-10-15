// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build debugtrace

package inlheur

import (
	"cmd/compile/publicinternal/ir"
	"strconv"
)

func enableDebugTraceIfEnv(target *ir.Package) {
	if target == nil {
		return
	}
	v := target.Getenv("DEBUG_TRACE_INLHEUR")
	if v == "" {
		target.InlineHeuristicsDebugTrace = 0
		return
	}
	if v[0] == '*' {
		if !UnitTesting(target) {
			return
		}
		v = v[1:]
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	target.InlineHeuristicsDebugTrace = i
}

func disableDebugTrace(target *ir.Package) {
	if target != nil {
		target.InlineHeuristicsDebugTrace = 0
	}
}

func debugTraceFor(target *ir.Package) int {
	if target == nil {
		return 0
	}
	return target.InlineHeuristicsDebugTrace
}
