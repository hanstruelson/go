// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !debugtrace

package inlheur

import "cmd/compile/internal/ir"

const debugTrace = 0

func enableDebugTrace(x int) {
}

func enableDebugTraceIfEnv(target *ir.Package) {
}

func disableDebugTrace() {
}
