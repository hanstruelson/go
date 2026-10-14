// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mips64

import (
	"cmd/compile/internal/objw"
	"cmd/internal/obj"
	"cmd/internal/obj/mips"
)

func zerorange(pp *objw.Progs, p *obj.Prog, off, cnt int64, _ *uint32) *obj.Prog {
	if cnt%int64(pp.CurFunc.Target.PtrSize) != 0 {
		panic("zeroed region not aligned")
	}

	for cnt != 0 {
		p = pp.Append(p, mips.AMOVV, obj.TYPE_REG, mips.REGZERO, 0, obj.TYPE_MEM, mips.REGSP, off+8)
		cnt -= int64(pp.CurFunc.Target.PtrSize)
		off += int64(pp.CurFunc.Target.PtrSize)
	}

	return p
}

func ginsnop(pp *objw.Progs) *obj.Prog {
	p := pp.Prog(mips.ANOOP)
	return p
}
