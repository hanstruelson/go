// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build debugtrace

package inlheur

import (
	"cmd/compile/internal/ir"
	"os"
	"strconv"
)

var debugTrace = 0

func enableDebugTrace(x int) {
	debugTrace = x
}

func enableDebugTraceIfEnv(target *ir.Package) {
	v := os.Getenv("DEBUG_TRACE_INLHEUR")
	if v == "" {
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
	debugTrace = i
}

func disableDebugTrace() {
	debugTrace = 0
}
