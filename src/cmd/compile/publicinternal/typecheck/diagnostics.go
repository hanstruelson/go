// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/src"
	"fmt"
	"publicinternal/types/errors"
)

func errorf(target *ir.Package, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.ErrorfAt(target.Pos, 0, format, args...)
		return
	}
	panic("typecheck diagnostic without a compilation target")
}

func errorfType(typ *types.Type, format string, args ...any) {
	if typ != nil && typ.Context().Diagnostics != nil {
		typ.Context().Diagnostics.ErrorfAt(src.NoXPos, 0, format, args...)
		return
	}
	panic("typecheck diagnostic without a compilation target")
}

func fatalfType(typ *types.Type, pos src.XPos, format string, args ...any) {
	if typ != nil && typ.Context().Diagnostics != nil {
		typ.Context().Diagnostics.FatalfAt(pos, format, args...)
		return
	}
	panic("typecheck diagnostic without a compilation target")
}

func errorfFor(target *ir.Package, typ *types.Type, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.ErrorfAt(target.Pos, 0, format, args...)
		return
	}
	errorfType(typ, format, args...)
}

func fatalfFor(target *ir.Package, typ *types.Type, pos src.XPos, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.FatalfAt(pos, format, args...)
		return
	}
	fatalfType(typ, pos, format, args...)
}

func errorfAt(target *ir.Package, pos src.XPos, code errors.Code, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.ErrorfAt(pos, code, format, args...)
		return
	}
	panic("typecheck diagnostic without a compilation target")
}

func fatalf(target *ir.Package, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.Fatalf(format, args...)
		return
	}
	panic("typecheck diagnostic without a compilation target")
}

func fatalfAt(target *ir.Package, pos src.XPos, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.FatalfAt(pos, format, args...)
		return
	}
	panic("typecheck diagnostic without a compilation target")
}

func assert(target *ir.Package, condition bool) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.Assert(condition)
		return
	}
	panic("typecheck assertion without a compilation target")
}

func assertf(target *ir.Package, condition bool, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.Assertf(condition, format, args...)
		return
	}
	panic("typecheck assertion without a compilation target")
}

func assertfAt(target *ir.Package, condition bool, pos src.XPos, format string, args ...any) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.AssertfAt(condition, pos, format, args...)
		return
	}
	panic("typecheck assertion without a compilation target")
}

func errorsFor(target *ir.Package) int {
	if target != nil && target.Diagnostics != nil {
		return target.Diagnostics.Errors()
	}
	panic("typecheck error count requested without a compilation target")
}

func fmtPosFor(target *ir.Package, pos src.XPos) string {
	if target != nil && target.Diagnostics != nil {
		return target.Diagnostics.FmtPos(pos)
	}
	return fmt.Sprint(pos)
}

func updateErrorDot(target *ir.Package, line, name, expr string) {
	if target != nil && target.Diagnostics != nil {
		target.Diagnostics.UpdateErrorDot(line, name, expr)
		return
	}
	panic("typecheck error update without a compilation target")
}
