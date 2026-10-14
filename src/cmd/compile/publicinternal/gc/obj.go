// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/noder"
	"cmd/compile/publicinternal/objw"
	"cmd/compile/publicinternal/pkginit"
	"cmd/compile/publicinternal/reflectdata"
	"cmd/compile/publicinternal/staticdata"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/archive"
	"cmd/publicinternal/bio"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// These modes say which kind of object file to generate.
// The default use of the toolchain is to set both bits,
// generating a combined compiler+linker object, one that
// serves to describe the package to both the compiler and the linker.
// In fact the compiler and linker read nearly disjoint sections of
// that file, though, so in a distributed build setting it can be more
// efficient to split the output into two files, supplying the compiler
// object only to future compilations and the linker object only to
// future links.
//
// By default a combined object is written, but if -linkobj is specified
// on the command line then the default -o output is a compiler object
// and the -linkobj output is a linker object.
const (
	modeCompilerObj = 1 << iota
	modeLinkerObj
)

func dumpexport(frontend *noder.Context) {
	target := frontend.Target
	if target.Flags.LinkObj == "" {
		return
	}
	dumpobj1(frontend, target.Flags.LowerO, modeCompilerObj)
	if target.Flags.ExportFD > 0 {
		notifyExport(target.Flags.ExportFD)
	}
}

func notifyExport(exportFD int) {
	f := os.NewFile(uintptr(exportFD), "exportfd")
	if _, err := f.Write([]byte{'\n'}); err != nil {
		base.FlushErrors()
		fmt.Printf("can't write to export fd %d: %v\n", exportFD, err)
		base.ErrorExit()
	}
	if err := f.Close(); err != nil {
		base.FlushErrors()
		fmt.Printf("can't close export fd %d: %v\n", exportFD, err)
		base.ErrorExit()
	}
}

func dumpobj(frontend *noder.Context) {
	target := frontend.Target
	if target.Flags.LinkObj == "" {
		dumpobj1(frontend, target.Flags.LowerO, modeCompilerObj|modeLinkerObj)
		return
	}
	dumpobj1(frontend, target.Flags.LinkObj, modeLinkerObj)
}

func dumpobj1(frontend *noder.Context, outfile string, mode int) {
	bout, err := bio.Create(outfile)
	if err != nil {
		base.FlushErrors()
		fmt.Printf("can't create %s: %v\n", outfile, err)
		base.ErrorExit()
	}

	bout.WriteString("!<arch>\n")

	if mode&modeCompilerObj != 0 {
		start := startArchiveEntry(bout)
		dumpCompilerObj(frontend, bout)
		finishArchiveEntry(bout, start, "__.PKGDEF")
	}
	if mode&modeLinkerObj != 0 {
		start := startArchiveEntry(bout)
		dumpLinkerObj(frontend.Target, bout)
		finishArchiveEntry(bout, start, "_go_.o")
	}

	if err := bout.Close(); err != nil {
		base.FlushErrors()
		fmt.Printf("error while writing to file %s: %v\n", outfile, err)
		base.ErrorExit()
	}
}

func printObjHeader(target *ir.Package, bout *bio.Writer) {
	bout.WriteString(objabi.HeaderString())
	if target.Flags.BuildID != "" {
		fmt.Fprintf(bout, "build id %q\n", target.Flags.BuildID)
	}
	if target.LocalPkg.Name == "main" {
		fmt.Fprintf(bout, "main\n")
	}
	fmt.Fprintf(bout, "\n") // header ends with blank line
}

func startArchiveEntry(bout *bio.Writer) int64 {
	var arhdr [archive.HeaderSize]byte
	bout.Write(arhdr[:])
	return bout.Offset()
}

func finishArchiveEntry(bout *bio.Writer, start int64, name string) {
	bout.Flush()
	size := bout.Offset() - start
	if size&1 != 0 {
		bout.WriteByte(0)
	}
	bout.MustSeek(start-archive.HeaderSize, 0)

	var arhdr [archive.HeaderSize]byte
	archive.FormatHeader(arhdr[:], name, size)
	bout.Write(arhdr[:])
	bout.Flush()
	bout.MustSeek(start+size+(size&1), 0)
}

func dumpCompilerObj(frontend *noder.Context, bout *bio.Writer) {
	printObjHeader(frontend.Target, bout)
	noder.WriteExports(frontend, bout)
}

func dumpdata(target *ir.Package) {
	reflectdata.WriteGCSymbols(target)
	reflectdata.WritePluginTable(target)
	dumpembeds(target)

	if target.ZeroSize > 0 {
		zero := target.TypeContext.LinkSym("go:map", "zero", obj.ABI0)
		objw.Global(target.Ctxt, zero, int32(target.ZeroSize), obj.DUPOK|obj.RODATA)
		zero.Set(obj.AttrStatic, true)
	}

	staticdata.WriteFuncSyms(target)
	addGCLocals(target)
}

func dumpLinkerObj(target *ir.Package, bout *bio.Writer) {
	printObjHeader(target, bout)

	if len(target.CgoPragmas) != 0 {
		// write empty export section; must be before cgo section
		fmt.Fprintf(bout, "\n$$\n\n$$\n\n")
		fmt.Fprintf(bout, "\n$$  // cgo\n")
		if err := json.NewEncoder(bout).Encode(target.CgoPragmas); err != nil {
			base.Fatalf("serializing pragcgobuf: %v", err)
		}
		fmt.Fprintf(bout, "\n$$\n\n")
	}

	fmt.Fprintf(bout, "\n!\n")

	obj.WriteObjFile(target.Ctxt, bout)
}

func dumpGlobal(target *ir.Package, n *ir.Name) {
	if n.Type() == nil {
		base.Fatalf("external %v nil type\n", n)
	}
	if n.Class == ir.PFUNC {
		return
	}
	if n.Sym().Pkg != target.LocalPkg {
		return
	}
	types.CalcSize(n.Type())
	ggloblnod(target, n)
	if n.CoverageAuxVar() || n.Linksym().Static() {
		return
	}
	target.Ctxt.DwarfGlobal(types.TypeSymName(n.Type()), n.Linksym())
}

func dumpGlobalConst(target *ir.Package, n *ir.Name) {
	// only export typed constants
	t := n.Type()
	if t == nil {
		return
	}
	if n.Sym().Pkg != target.LocalPkg {
		return
	}
	// only export integer constants for now
	if !t.IsInteger() {
		return
	}
	v := n.Val()
	if t.IsUntyped() {
		// Export untyped integers as int (if they fit).
		t = target.Type(types.TINT)
		if ir.ConstOverflow(v, t) {
			return
		}
	} else {
		// If the type of the constant is an instantiated generic, we need to emit
		// that type so the linker knows about it. See issue 51245.
		_ = reflectdata.TypeLinksym(target, t)
	}
	target.Ctxt.DwarfIntConst(n.Sym().Name, types.TypeSymName(t), ir.IntVal(t, v))
}

// addGCLocals adds gcargs, gclocals, gcregs, and stack object symbols to Ctxt.Data.
//
// This is done during the sequential phase after compilation, since
// global symbols can't be declared during parallel compilation.
func addGCLocals(target *ir.Package) {
	for _, s := range target.Ctxt.Text {
		fn := s.Func()
		if fn == nil {
			continue
		}
		for _, gcsym := range []*obj.LSym{fn.GCArgs, fn.GCLocals} {
			if gcsym != nil && !gcsym.OnList() {
				objw.Global(target.Ctxt, gcsym, int32(len(gcsym.P)), obj.RODATA|obj.DUPOK)
			}
		}
		if x := fn.StackObjects; x != nil {
			objw.Global(target.Ctxt, x, int32(len(x.P)), obj.RODATA)
			x.Set(obj.AttrStatic, true)
		}
		if x := fn.OpenCodedDeferInfo; x != nil {
			objw.Global(target.Ctxt, x, int32(len(x.P)), obj.RODATA|obj.DUPOK)
		}
		if x := fn.ArgInfo; x != nil {
			objw.Global(target.Ctxt, x, int32(len(x.P)), obj.RODATA|obj.DUPOK)
			x.Set(obj.AttrStatic, true)
		}
		if x := fn.ArgLiveInfo; x != nil {
			objw.Global(target.Ctxt, x, int32(len(x.P)), obj.RODATA|obj.DUPOK)
			x.Set(obj.AttrStatic, true)
		}
		if x := fn.WrapInfo; x != nil && !x.OnList() {
			objw.Global(target.Ctxt, x, int32(len(x.P)), obj.RODATA|obj.DUPOK)
			x.Set(obj.AttrStatic, true)
		}
		for _, jt := range fn.JumpTables {
			objw.Global(target.Ctxt, jt.Sym, int32(len(jt.Targets)*target.Ctxt.Arch.PtrSize), obj.RODATA)
		}
	}
}

func ggloblnod(target *ir.Package, nam *ir.Name) {
	s := nam.Linksym()

	// main_inittask and runtime_inittask in package runtime (and in
	// test/initempty.go) aren't real variable declarations, but
	// linknamed variables pointing to the compiler's generated
	// .inittask symbol. The real symbol was already written out in
	// pkginit.Task, so we need to avoid writing them out a second time
	// here, otherwise target.Ctxt.Globl will fail.
	if strings.HasSuffix(s.Name, "..inittask") && s.OnList() {
		return
	}

	s.Gotype = reflectdata.TypeLinksym(target, nam.Type())
	flags := 0
	if nam.Readonly() {
		flags = obj.RODATA
	}
	if nam.Type() != nil && !nam.Type().HasPointers() {
		flags |= obj.NOPTR
	}
	size := nam.Type().Size()
	linkname := nam.Sym().Linkname
	name := nam.Sym().Name

	var saveType objabi.SymKind
	if nam.CoverageAuxVar() {
		saveType = s.Type
	}

	// We've skipped linkname'd globals's instrument, so we can skip them here as well.
	if target.Flags.ASan && linkname == "" && target.InstrumentedGlobalsMap[name] != nil {
		// Write the new size of instrumented global variables that have
		// trailing redzones into object file.
		rzSize := pkginit.GetRedzoneSizeForGlobal(size)
		sizeWithRZ := rzSize + size
		target.Ctxt.Globl(s, sizeWithRZ, flags)
	} else {
		target.Ctxt.Globl(s, size, flags)
	}
	if nam.Libfuzzer8BitCounter() {
		s.Type = objabi.SLIBFUZZER_8BIT_COUNTER
	}
	if nam.CoverageAuxVar() && saveType == objabi.SCOVERAGE_COUNTER {
		// restore specialized counter type (which Globl call above overwrote)
		s.Type = saveType
	}
	if nam.Sym().Linkname != "" {
		// Make sure linkname'd symbol is non-package. When a symbol is
		// both imported and linkname'd, s.Pkg may not set to "_" in
		// types.Sym.Linksym because LSym already exists. Set it here.
		s.Pkg = "_"
	}
}

func dumpembeds(target *ir.Package) {
	for _, v := range target.Embeds {
		staticdata.WriteEmbed(target, v)
	}
}
