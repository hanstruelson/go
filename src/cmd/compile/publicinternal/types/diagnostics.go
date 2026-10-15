// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"cmd/publicinternal/src"
	"publicinternal/types/errors"
)

func errorfContext(context *Context, pos src.XPos, code errors.Code, format string, args ...any) {
	if context != nil && context.Diagnostics != nil {
		context.Diagnostics.ErrorfAt(pos, code, format, args...)
		return
	}
	panic("type diagnostic without a compilation context")
}

func fatalfContext(context *Context, pos src.XPos, format string, args ...any) {
	if context != nil && context.Diagnostics != nil {
		context.Diagnostics.FatalfAt(pos, format, args...)
		return
	}
	panic("type diagnostic without a compilation context")
}

func fatalfType(t *Type, pos src.XPos, format string, args ...any) {
	if t != nil {
		fatalfContext(contextForType(t), pos, format, args...)
		return
	}
	panic("type diagnostic without a compilation context")
}

func assertfContext(context *Context, condition bool, format string, args ...any) {
	if context != nil && context.Diagnostics != nil {
		context.Diagnostics.Assertf(condition, format, args...)
		return
	}
	panic("type assertion without a compilation context")
}
