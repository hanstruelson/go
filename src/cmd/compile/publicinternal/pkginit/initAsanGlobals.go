// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkginit

import (
	"strings"

	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/typecheck"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/src"
)

// instrumentGlobals declares a global array of _asan_global structures and initializes it.
func instrumentGlobals(target *ir.Package, fn *ir.Func) *ir.Name {
	asanGlobalStruct, asanLocationStruct, defStringstruct := createtypes(target)
	lname := func(name string) *types.Sym { return typecheck.Lookup(target, name) }
	// Make a global array of asanGlobalStruct type.
	// var asanglobals []asanGlobalStruct
	arraytype := types.NewArray(asanGlobalStruct, int64(len(target.InstrumentedGlobalsMap)))
	symG := lname(".asanglobals")
	globals := ir.NewNameAt(target.Pos, symG, arraytype)
	globals.Class = ir.PEXTERN
	symG.Def = globals
	target.Externs = append(target.Externs, globals)
	// Make a global array of asanLocationStruct type.
	// var asanL []asanLocationStruct
	arraytype = types.NewArray(asanLocationStruct, int64(len(target.InstrumentedGlobalsMap)))
	symL := lname(".asanL")
	asanlocation := ir.NewNameAt(target.Pos, symL, arraytype)
	asanlocation.Class = ir.PEXTERN
	symL.Def = asanlocation
	target.Externs = append(target.Externs, asanlocation)
	// Make three global string variables to pass the global name and module name
	// and the name of the source file that defines it.
	// var asanName string
	// var asanModulename string
	// var asanFilename string
	symL = lname(".asanName")
	asanName := ir.NewNameAt(target.Pos, symL, target.Type(types.TSTRING))
	asanName.Class = ir.PEXTERN
	symL.Def = asanName
	target.Externs = append(target.Externs, asanName)

	symL = lname(".asanModulename")
	asanModulename := ir.NewNameAt(target.Pos, symL, target.Type(types.TSTRING))
	asanModulename.Class = ir.PEXTERN
	symL.Def = asanModulename
	target.Externs = append(target.Externs, asanModulename)

	symL = lname(".asanFilename")
	asanFilename := ir.NewNameAt(target.Pos, symL, target.Type(types.TSTRING))
	asanFilename.Class = ir.PEXTERN
	symL.Def = asanFilename
	target.Externs = append(target.Externs, asanFilename)

	var init ir.Nodes
	var c ir.Node
	// globals[i].odrIndicator = 0 is the default, no need to set it explicitly here.
	for i, n := range target.InstrumentedGlobalsSlice {
		setField := func(f string, val ir.Node, i int) {
			r := ir.NewAssignStmt(target.Pos, ir.NewSelectorExpr(target.Pos, ir.ODOT,
				ir.NewIndexExpr(target.Pos, globals, ir.NewInt(target.Pos, int64(i))), lname(f)), val)
			init.Append(typecheck.Stmt(fn, r))
		}
		// globals[i].beg = uintptr(unsafe.Pointer(&n))
		c = typecheck.ConvNop(fn, typecheck.NodAddr(fn, n), target.Type(types.TUNSAFEPTR))
		c = typecheck.ConvNop(fn, c, target.Type(types.TUINTPTR))
		setField("beg", c, i)
		// Assign globals[i].size.
		g := n.(*ir.Name)
		size := g.Type().Size()
		c = typecheck.DefaultLitFor(target, ir.NewInt(target.Pos, size), target.Type(types.TUINTPTR))
		setField("size", c, i)
		// Assign globals[i].sizeWithRedzone.
		rzSize := GetRedzoneSizeForGlobal(size)
		sizeWithRz := rzSize + size
		c = typecheck.DefaultLitFor(target, ir.NewInt(target.Pos, sizeWithRz), target.Type(types.TUINTPTR))
		setField("sizeWithRedzone", c, i)
		// The C string type is terminated by a null character "\0", Go should use three-digit
		// octal "\000" or two-digit hexadecimal "\x00" to create null terminated string.
		// asanName = symbol's linkname + "\000"
		// globals[i].name = (*defString)(unsafe.Pointer(&asanName)).data
		name := g.Linksym().Name
		init.Append(typecheck.Stmt(fn, ir.NewAssignStmt(target.Pos, asanName, ir.NewString(target.Pos, name+"\000"))))
		c = typecheck.ConvNop(fn, typecheck.NodAddr(fn, asanName), target.Type(types.TUNSAFEPTR))
		c = typecheck.ConvNop(fn, c, types.NewPtr(defStringstruct))
		c = ir.NewSelectorExpr(target.Pos, ir.ODOT, c, lname("data"))
		setField("name", c, i)

		// Set the name of package being compiled as a unique identifier of a module.
		// asanModulename = pkgName + "\000"
		init.Append(typecheck.Stmt(fn, ir.NewAssignStmt(target.Pos, asanModulename, ir.NewString(target.Pos, target.LocalPkg.Name+"\000"))))
		c = typecheck.ConvNop(fn, typecheck.NodAddr(fn, asanModulename), target.Type(types.TUNSAFEPTR))
		c = typecheck.ConvNop(fn, c, types.NewPtr(defStringstruct))
		c = ir.NewSelectorExpr(target.Pos, ir.ODOT, c, lname("data"))
		setField("moduleName", c, i)
		// Assign asanL[i].filename, asanL[i].line, asanL[i].column
		// and assign globals[i].location = uintptr(unsafe.Pointer(&asanL[i]))
		asanLi := ir.NewIndexExpr(target.Pos, asanlocation, ir.NewInt(target.Pos, int64(i)))
		filename := ir.NewString(target.Pos, target.Ctxt.PosTable.Pos(n.Pos()).Filename()+"\000")
		init.Append(typecheck.Stmt(fn, ir.NewAssignStmt(target.Pos, asanFilename, filename)))
		c = typecheck.ConvNop(fn, typecheck.NodAddr(fn, asanFilename), target.Type(types.TUNSAFEPTR))
		c = typecheck.ConvNop(fn, c, types.NewPtr(defStringstruct))
		c = ir.NewSelectorExpr(target.Pos, ir.ODOT, c, lname("data"))
		init.Append(typecheck.Stmt(fn, ir.NewAssignStmt(target.Pos, ir.NewSelectorExpr(target.Pos, ir.ODOT, asanLi, lname("filename")), c)))
		line := ir.NewInt(target.Pos, int64(n.Pos().Line()))
		init.Append(typecheck.Stmt(fn, ir.NewAssignStmt(target.Pos, ir.NewSelectorExpr(target.Pos, ir.ODOT, asanLi, lname("line")), line)))
		col := ir.NewInt(target.Pos, int64(n.Pos().Col()))
		init.Append(typecheck.Stmt(fn, ir.NewAssignStmt(target.Pos, ir.NewSelectorExpr(target.Pos, ir.ODOT, asanLi, lname("column")), col)))
		c = typecheck.ConvNop(fn, typecheck.NodAddr(fn, asanLi), target.Type(types.TUNSAFEPTR))
		c = typecheck.ConvNop(fn, c, target.Type(types.TUINTPTR))
		setField("sourceLocation", c, i)
	}
	fn.Body.Append(init...)
	return globals
}

// createtypes creates the asanGlobal, asanLocation and defString struct type.
// Go compiler does not refer to the C types, we represent the struct field
// by a uintptr, then use type conversion to make copies of the data.
// E.g., (*defString)(asanGlobal.name).data to C string.
//
// Keep in sync with src/runtime/asan/asan.go.
// type asanGlobal struct {
//	beg               uintptr
//	size              uintptr
//	size_with_redzone uintptr
//	name              uintptr
//	moduleName        uintptr
//	hasDynamicInit    uintptr
//	sourceLocation    uintptr
//	odrIndicator      uintptr
// }
//
// type asanLocation struct {
//	filename uintptr
//	line     int32
//	column   int32
// }
//
// defString is synthesized struct type meant to capture the underlying
// implementations of string.
// type defString struct {
//	data uintptr
//	len  uintptr
// }

func createtypes(target *ir.Package) (*types.Type, *types.Type, *types.Type) {
	up := target.Type(types.TUINTPTR)
	i32 := target.Type(types.TINT32)
	fname := func(name string) *types.Sym { return typecheck.Lookup(target, name) }
	nxp := src.NoXPos
	nfield := types.NewField
	asanGlobal := types.NewStruct([]*types.Field{
		nfield(nxp, fname("beg"), up),
		nfield(nxp, fname("size"), up),
		nfield(nxp, fname("sizeWithRedzone"), up),
		nfield(nxp, fname("name"), up),
		nfield(nxp, fname("moduleName"), up),
		nfield(nxp, fname("hasDynamicInit"), up),
		nfield(nxp, fname("sourceLocation"), up),
		nfield(nxp, fname("odrIndicator"), up),
	})
	types.CalcSize(asanGlobal)

	asanLocation := types.NewStruct([]*types.Field{
		nfield(nxp, fname("filename"), up),
		nfield(nxp, fname("line"), i32),
		nfield(nxp, fname("column"), i32),
	})
	types.CalcSize(asanLocation)

	defString := types.NewStruct([]*types.Field{
		types.NewField(nxp, fname("data"), up),
		types.NewField(nxp, fname("len"), up),
	})
	types.CalcSize(defString)

	return asanGlobal, asanLocation, defString
}

// Calculate redzone for globals.
func GetRedzoneSizeForGlobal(size int64) int64 {
	maxRZ := int64(1 << 18)
	minRZ := int64(32)
	redZone := (size / minRZ / 4) * minRZ
	switch {
	case redZone > maxRZ:
		redZone = maxRZ
	case redZone < minRZ:
		redZone = minRZ
	}
	// Round up to multiple of minRZ.
	if size%minRZ != 0 {
		redZone += minRZ - (size % minRZ)
	}
	return redZone
}

func canInstrumentGlobal(target *ir.Package, g ir.Node) bool {
	if g.Op() != ir.ONAME {
		return false
	}
	n := g.(*ir.Name)
	if n.Class == ir.PFUNC {
		return false
	}
	if n.Sym().Pkg != target.LocalPkg {
		return false
	}
	// Do not instrument any _cgo_ related global variables, because they are declared in C code.
	if strings.Contains(n.Sym().Name, "cgo") {
		return false
	}

	// Do not instrument counter globals in publicinternal/fuzz. These globals are replaced by the linker.
	// See go.dev/issue/72766 for more details.
	if n.Sym().Pkg.Path == "publicinternal/fuzz" && (n.Sym().Name == "_counters" || n.Sym().Name == "_ecounters") {
		return false
	}

	// Do not instrument globals that are linknamed, because their home package will do the work.
	if n.Sym().Linkname != "" {
		return false
	}

	return true
}
