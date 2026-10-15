// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Indexed package import.
// See iexport.go for the export data format.

package typecheck

import (
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/types"
)

func SetBaseTypeIndex(target *ir.Package, t *types.Type, i, pi int64) {
	if t.Obj() == nil {
		fatalf(target, "SetBaseTypeIndex on non-defined type %v", t)
	}
	if i != -1 && pi != -1 {
		if target.TypeSymIdx == nil {
			target.TypeSymIdx = make(map[*types.Type][2]int64)
		}
		target.TypeSymIdx[t] = [2]int64{i, pi}
	}
}

func BaseTypeIndex(target *ir.Package, t *types.Type) int64 {
	tbase := t
	if t.IsPtr() && t.Sym() == nil && t.Elem().Sym() != nil {
		tbase = t.Elem()
	}
	i, ok := target.TypeSymIdx[tbase]
	if !ok {
		return -1
	}
	if t != tbase {
		return i[1]
	}
	return i[0]
}
