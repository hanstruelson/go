// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"testing"

	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/ssa"
	"cmd/compile/publicinternal/typecheck"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/obj/arm64"
	"cmd/publicinternal/obj/s390x"
	"cmd/publicinternal/obj/x86"
	"cmd/publicinternal/src"
	"cmd/publicinternal/sys"
)

var CheckFunc = checkFunc
var Opt = opt
var Deadcode = deadcode
var Copyelim = copyelim

var testTarget *ir.Package

func testConfig(tb testing.TB) *Conf      { return testConfigArch(tb, "amd64") }
func testConfigS390X(tb testing.TB) *Conf { return testConfigArch(tb, "s390x") }
func testConfigARM64(tb testing.TB) *Conf { return testConfigArch(tb, "arm64") }

func testConfigArch(tb testing.TB, arch string) *Conf {
	var linkArch *obj.LinkArch
	switch arch {
	case "amd64":
		linkArch = &x86.Linkamd64
	case "s390x":
		linkArch = &s390x.Links390x
	case "arm64":
		linkArch = &arm64.Linkarm64
	default:
		tb.Fatalf("unknown arch %s", arch)
	}
	ctxt := obj.Linknew(linkArch)
	if ctxt.Arch.PtrSize != 8 {
		tb.Fatal("testTypes is 64-bit only")
	}
	testTarget.Ctxt = ctxt
	testTarget.TypeContext.Link = ctxt
	c := &Conf{
		config: newConfig(testTarget, arch, testTypes, ctxt, true, false),
		tb:     tb,
	}
	return c
}

type Conf struct {
	config *ssa.Config
	tb     testing.TB
	fe     ssa.Frontend
}

func (c *Conf) Frontend() ssa.Frontend {
	if c.fe == nil {
		pkg := types.NewPkgInstanceFor(testTarget.TypeContext, "my/import/path", "path")
		fn := ir.NewFunc(nil, src.NoXPos, src.NoXPos, pkg.Lookup("function"), types.NewSignatureIn(testTarget.TypeContext, nil, nil, nil))
		fn.Target = testTarget
		fn.DeclareParams(true)
		fn.LSym = &obj.LSym{Name: "my/import/path.function"}

		c.fe = TestFrontend{
			t:    c.tb,
			ctxt: c.config.Ctxt,
			f:    fn,
		}
	}
	return c.fe
}

func (c *Conf) Temp(typ *types.Type) *ir.Name {
	n := ir.NewNameAt(src.NoXPos, &types.Sym{Name: "aFakeAuto"}, typ)
	n.Class = ir.PAUTO
	return n
}

// TestFrontend is a test-only frontend.
// It assumes 64 bit integers and pointers.
type TestFrontend struct {
	t    testing.TB
	ctxt *obj.Link
	f    *ir.Func
}

func (TestFrontend) StringData(s string) *obj.LSym {
	return nil
}
func (d TestFrontend) SplitSlot(parent *ssa.LocalSlot, suffix string, offset int64, t *types.Type) ssa.LocalSlot {
	return ssa.LocalSlot{N: parent.N, Type: t, Off: offset}
}
func (d TestFrontend) Syslook(s string) *obj.LSym {
	return d.ctxt.Lookup(s)
}
func (TestFrontend) UseWriteBarrier() bool {
	return true // only writebarrier_test cares
}

func (d TestFrontend) Logf(msg string, args ...any) { d.t.Logf(msg, args...) }
func (d TestFrontend) Log() bool                    { return true }

func (d TestFrontend) Fatalf(_ src.XPos, msg string, args ...any) { d.t.Fatalf(msg, args...) }
func (d TestFrontend) Warnl(_ src.XPos, msg string, args ...any)  { d.t.Logf(msg, args...) }
func (d TestFrontend) Debug_checknil() bool                       { return false }

func (d TestFrontend) Func() *ir.Func {
	return d.f
}

var testTypes ssa.Types

func init() {
	ctxt := &obj.Link{Arch: &obj.LinkArch{Arch: &sys.Arch{Alignment: 1, CanMergeLoads: true}}}
	typeContext := types.NewContext(8, 8, 1<<50)
	testTarget = &ir.Package{
		Ctxt:        ctxt,
		TypeContext: typeContext,
		LocalPkg:    types.NewPkgInstanceFor(typeContext, "ssacompile/test", "test"),
	}
	testTarget.TypeContext.Link = testTarget.Ctxt
	testTarget.LocalPkg.Local = true
	typecheck.InitUniverse(testTarget)
	testTypes.SetTypPtrs(testTarget)
}
