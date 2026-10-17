// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

#include "textflag.h"

TEXT ·callEntry(SB), NOSPLIT, $0-8
	MOVQ entry+0(FP), AX
	CALL AX
	RET

TEXT ·callEntryInt(SB), NOSPLIT, $0-16
	MOVQ entry+0(FP), AX
	CALL AX
	MOVQ AX, ret+8(FP)
	RET
