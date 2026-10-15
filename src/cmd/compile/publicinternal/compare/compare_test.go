// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compare

import (
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/typecheck"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/src"
	"cmd/publicinternal/sys"
	"testing"
)

var testTarget *ir.Package
var testArch *obj.LinkArch

type typefn func() *types.Type

func init() {
	testArch = &obj.LinkArch{Arch: &sys.Arch{PtrSize: 8, RegSize: 8, Alignment: 1, CanMergeLoads: true}}
	link := &obj.Link{Arch: testArch}
	testTarget = &ir.Package{Ctxt: link, TypeContext: types.NewContext(8, 8, 1<<50)}
	testTarget.TypeContext.Link = link
	testTarget.TypeContext.Diagnostics = base.NewDiagnostics(nil)
	testTarget.LocalPkg = testTarget.Pkg("compare/test", "test")
	testTarget.BuiltinPkg = testTarget.Pkg("go.builtin", "")
	testTarget.UnsafePkg = testTarget.Pkg("unsafe", "unsafe")
	testTarget.LocalPkg.Local = true
	typecheck.InitUniverse(testTarget)
}

func TestEqStructCost(t *testing.T) {
	repeat := func(n int, typ *types.Type) []*types.Type {
		typs := make([]*types.Type, n)
		for i := range typs {
			typs[i] = typ
		}
		return typs
	}

	tt := []struct {
		name             string
		cost             int64
		nonMergeLoadCost int64
		fieldTypes       []*types.Type
	}{
		{"struct without fields", 0, 0, nil},
		{"struct with 1 byte field", 1, 1, repeat(1, testTarget.ByteType)},
		{"struct with 8 byte fields", 1, 8, repeat(8, testTarget.ByteType)},
		{"struct with 16 byte fields", 2, 16, repeat(16, testTarget.ByteType)},
		{"struct with 32 byte fields", 4, 32, repeat(32, testTarget.ByteType)},
		{"struct with 2 int32 fields", 1, 2, repeat(2, testTarget.Type(types.TINT32))},
		{"struct with 2 int32 fields and 1 int64", 2, 3,
			[]*types.Type{
				testTarget.Type(types.TINT32),
				testTarget.Type(types.TINT32),
				testTarget.Type(types.TINT64),
			},
		},
		{"struct with 1 int field and 1 string", 3, 3,
			[]*types.Type{
				testTarget.Type(types.TINT64),
				testTarget.Type(types.TSTRING),
			},
		},
		{"struct with 2 strings", 4, 4, repeat(2, testTarget.Type(types.TSTRING))},
		{"struct with 1 large byte array field", 26, 101,
			[]*types.Type{
				types.NewArray(testTarget.Type(types.TUINT16), 101),
			},
		},
		{"struct with string array field", 4, 4,
			[]*types.Type{
				types.NewArray(testTarget.Type(types.TSTRING), 2),
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			fields := make([]*types.Field, len(tc.fieldTypes))
			for i, ftyp := range tc.fieldTypes {
				fields[i] = types.NewField(src.NoXPos, typecheck.LookupNum(testTarget, "f", i), ftyp)
			}
			typ := types.NewStructIn(testTarget.TypeContext, fields)
			types.CalcSize(typ)

			want := tc.cost
			testArch.CanMergeLoads = true
			actual := EqStructCost(testArch, typ)
			if actual != want {
				t.Errorf("CanMergeLoads=true EqStructCost(%v) = %d, want %d", typ, actual, want)
			}

			testArch.CanMergeLoads = false
			want = tc.nonMergeLoadCost
			actual = EqStructCost(testArch, typ)
			if actual != want {
				t.Errorf("CanMergeLoads=false EqStructCost(%v) = %d, want %d", typ, actual, want)
			}
		})
	}
}
