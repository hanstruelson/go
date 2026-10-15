// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

import (
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/src"
	"publicinternal/types/errors"
)

func fatalfTarget(target *Package, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.Fatalf(format, args...)
		return
	}
	panic("IR diagnostic without a compilation target")
}

func fatalfAtTarget(target *Package, pos src.XPos, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.FatalfAt(pos, format, args...)
		return
	}
	panic("IR diagnostic without a compilation target")
}

func errorfAtTarget(target *Package, pos src.XPos, code errors.Code, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.ErrorfAt(pos, code, format, args...)
		return
	}
	panic("IR diagnostic without a compilation target")
}

func warnfAtTarget(target *Package, pos src.XPos, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.WarnfAt(pos, format, args...)
		return
	}
	panic("IR diagnostic without a compilation target")
}

func fatalfType(typ *types.Type, pos src.XPos, format string, args ...any) {
	if typ != nil && typ.Context().Diagnostics != nil {
		typ.Context().Diagnostics.FatalfAt(pos, format, args...)
		return
	}
	panic("type diagnostic without a compilation context")
}

func errorfType(typ *types.Type, pos src.XPos, code errors.Code, format string, args ...any) {
	if typ != nil && typ.Context().Diagnostics != nil {
		typ.Context().Diagnostics.ErrorfAt(pos, code, format, args...)
		return
	}
	panic("type diagnostic without a compilation context")
}
