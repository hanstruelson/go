// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflectdata

import (
	"encoding/binary"
	"fmt"
	"publicinternal/abi"
	"slices"
	"sort"
	"strings"

	"cmd/compile/publicinternal/bitvec"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/objw"
	"cmd/compile/publicinternal/rttype"
	"cmd/compile/publicinternal/staticdata"
	"cmd/compile/publicinternal/typebits"
	"cmd/compile/publicinternal/typecheck"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"cmd/publicinternal/src"
)

type ptabEntry struct {
	s *types.Sym
	t *types.Type
}

type typeSig struct {
	name  *types.Sym
	isym  *obj.LSym
	tsym  *obj.LSym
	type_ *types.Type
	mtype *types.Type
}

func commonSize(target *ir.Package) int { return int(rttype.For(target).Type.Size()) } // Sizeof(runtime._type{})

func uncommonSize(target *ir.Package, t *types.Type) int { // Sizeof(runtime.uncommontype{})
	if t.TFlag()&abi.TFlagUncommon == 0 {
		return 0
	}
	return int(rttype.For(target).UncommonType.Size())
}

func makefield(name string, t *types.Type) *types.Field {
	sym := (*types.Pkg)(nil).Lookup(name)
	return types.NewField(src.NoXPos, sym, t)
}

// methods returns the methods of the non-interface type t, sorted by name.
// Generates stub functions as needed.
func methods(target *ir.Package, t *types.Type) []*typeSig {
	if t.HasShape() {
		// Shape types have no methods.
		return nil
	}
	// method type
	mt := types.ReceiverBaseType(t)

	if mt == nil {
		return nil
	}
	typecheck.CalcMethods(mt)

	// make list of methods for t,
	// generating code if necessary.
	var ms []*typeSig
	for _, f := range mt.AllMethods() {
		if f.Sym == nil {
			target.Diagnostics.Fatalf("method with no sym on %v", mt)
		}
		if !f.IsMethod() {
			target.Diagnostics.Fatalf("non-method on %v method %v %v", mt, f.Sym, f)
		}
		if f.Type.Recv() == nil {
			target.Diagnostics.Fatalf("receiver with no type on %v method %v %v", mt, f.Sym, f)
		}
		if f.Nointerface() && !t.IsFullyInstantiated() {
			// Skip creating method wrappers if f is nointerface. But, if
			// t is an instantiated type, we still have to call
			// methodWrapper, because methodWrapper generates the actual
			// generic method on the type as well.
			continue
		}

		// get receiver type for this particular method.
		// if pointer receiver but non-pointer t and
		// this is not an embedded pointer inside a struct,
		// method does not apply.
		if !types.IsMethodApplicable(t, f) {
			continue
		}

		sig := &typeSig{
			name:  f.Sym,
			isym:  methodWrapper(target, t, f, true),
			tsym:  methodWrapper(target, t, f, false),
			type_: typecheck.NewMethodType(target.AutoPos, f.Type, t),
			mtype: typecheck.NewMethodType(target.AutoPos, f.Type, nil),
		}
		if f.Nointerface() {
			// In the case of a nointerface method on an instantiated
			// type, don't actually append the typeSig.
			continue
		}
		ms = append(ms, sig)
	}

	return ms
}

// imethods returns the methods of the interface type t, sorted by name.
func imethods(target *ir.Package, t *types.Type) []*typeSig {
	var methods []*typeSig
	for _, f := range t.AllMethods() {
		if f.Type.Kind() != types.TFUNC || f.Sym == nil {
			continue
		}
		if f.Sym.IsBlank() {
			target.Diagnostics.Fatalf("unexpected blank symbol in interface method set")
		}
		if n := len(methods); n > 0 {
			last := methods[n-1]
			if types.CompareSyms(last.name, f.Sym) >= 0 {
				target.Diagnostics.Fatalf("sigcmp vs sortinter %v %v", last.name, f.Sym)
			}
		}

		sig := &typeSig{
			name:  f.Sym,
			mtype: f.Type,
			type_: typecheck.NewMethodType(target.AutoPos, f.Type, nil),
		}
		methods = append(methods, sig)

		// NOTE(rsc): Perhaps an oversight that
		// IfaceType.Method is not in the reflect data.
		// Generate the method body, so that compiled
		// code can refer to it.
		methodWrapper(target, t, f, false)
	}

	return methods
}

func dimportpath(target *ir.Package, p *types.Pkg) {
	if p.Pathsym != nil {
		return
	}

	if p == target.LocalPkg && target.Ctxt.Pkgpath == "" {
		panic("missing pkgpath")
	}

	// If we are compiling the runtime package, there are two runtime packages around
	// -- the local package and target.RuntimePkg. We don't want to produce import path symbols for
	// both of them, so just produce one for localpkg.
	if target.Ctxt.Pkgpath == "runtime" && p == target.RuntimePkg {
		return
	}

	s := target.Ctxt.Lookup("type:.importpath." + p.Prefix + ".")
	ot := dnameData(target, s, 0, p.Path, "", nil, false, false)
	objw.Global(target.Ctxt, s, int32(ot), obj.DUPOK|obj.RODATA)
	s.Set(obj.AttrContentAddressable, true)
	s.Align = 1
	p.Pathsym = s
}

func dgopkgpath(target *ir.Package, c rttype.Cursor, pkg *types.Pkg) {
	c = c.Field("Bytes")
	if pkg == nil {
		c.WritePtr(nil)
		return
	}

	dimportpath(target, pkg)
	c.WritePtr(pkg.Pathsym)
}

// dgopkgpathOff writes an offset relocation to the pkg path symbol to c.
func dgopkgpathOff(target *ir.Package, c rttype.Cursor, pkg *types.Pkg) {
	if pkg == nil {
		c.WriteInt32(0)
		return
	}

	dimportpath(target, pkg)
	c.WriteSymPtrOff(pkg.Pathsym, false)
}

// dnameField dumps a reflect.name for a struct field.
func dnameField(target *ir.Package, c rttype.Cursor, spkg *types.Pkg, ft *types.Field) {
	if !types.IsExported(ft.Sym.Name) && ft.Sym.Pkg != spkg {
		target.Diagnostics.Fatalf("package mismatch for %v", ft.Sym)
	}
	nsym := dname(target, ft.Sym.Name, ft.Note, nil, types.IsExported(ft.Sym.Name), ft.Embedded != 0)
	c.Field("Bytes").WritePtr(nsym)
}

// dnameData writes the contents of a reflect.name into s at offset ot.
func dnameData(target *ir.Package, s *obj.LSym, ot int, name, tag string, pkg *types.Pkg, exported, embedded bool) int {
	if len(name) >= 1<<29 {
		target.Diagnostics.Fatalf("name too long: %d %s...", len(name), name[:1024])
	}
	if len(tag) >= 1<<29 {
		target.Diagnostics.Fatalf("tag too long: %d %s...", len(tag), tag[:1024])
	}
	var nameLen [binary.MaxVarintLen64]byte
	nameLenLen := binary.PutUvarint(nameLen[:], uint64(len(name)))
	var tagLen [binary.MaxVarintLen64]byte
	tagLenLen := binary.PutUvarint(tagLen[:], uint64(len(tag)))

	// Encode name and tag. See reflect/type.go for details.
	var bits byte
	l := 1 + nameLenLen + len(name)
	if exported {
		bits |= 1 << 0
	}
	if len(tag) > 0 {
		l += tagLenLen + len(tag)
		bits |= 1 << 1
	}
	if pkg != nil {
		bits |= 1 << 2
	}
	if embedded {
		bits |= 1 << 3
	}
	b := make([]byte, l)
	b[0] = bits
	copy(b[1:], nameLen[:nameLenLen])
	copy(b[1+nameLenLen:], name)
	if len(tag) > 0 {
		tb := b[1+nameLenLen+len(name):]
		copy(tb, tagLen[:tagLenLen])
		copy(tb[tagLenLen:], tag)
	}

	ot = int(s.WriteBytes(target.Ctxt, int64(ot), b))

	if pkg != nil {
		c := rttype.NewCursor(target.Ctxt, s, int64(ot), target.Type(types.TUINT32))
		dgopkgpathOff(target, c, pkg)
		ot += 4
	}

	return ot
}

// dname creates a reflect.name for a struct field or method.
func dname(target *ir.Package, name, tag string, pkg *types.Pkg, exported, embedded bool) *obj.LSym {
	// Write out data as "type:." to signal two things to the
	// linker, first that when dynamically linking, the symbol
	// should be moved to a relro section, and second that the
	// contents should not be decoded as a type.
	sname := "type:.namedata."
	if pkg == nil {
		// In the common case, share data with other packages.
		if name == "" {
			if exported {
				sname += "-noname-exported." + tag
			} else {
				sname += "-noname-unexported." + tag
			}
		} else {
			if exported {
				sname += name + "." + tag
			} else {
				sname += name + "-" + tag
			}
		}
	} else {
		// TODO(mdempsky): We should be able to share these too (except
		// maybe when dynamic linking).
		sname = fmt.Sprintf("%s%s.%d", sname, target.LocalPkg.Prefix, target.DnameGen)
		target.DnameGen++
	}
	if embedded {
		sname += ".embedded"
	}
	s := target.Ctxt.Lookup(sname)
	if len(s.P) > 0 {
		return s
	}
	ot := dnameData(target, s, 0, name, tag, pkg, exported, embedded)
	objw.Global(target.Ctxt, s, int32(ot), obj.DUPOK|obj.RODATA)
	s.Set(obj.AttrContentAddressable, true)
	s.Align = 1
	return s
}

// dextratype dumps the fields of a runtime.uncommontype.
// dataAdd is the offset in bytes after the header where the
// backing array of the []method field should be written.
func dextratype(target *ir.Package, lsym *obj.LSym, off int64, t *types.Type, dataAdd int) {
	m := methods(target, t)
	if t.Sym() == nil && len(m) == 0 {
		target.Diagnostics.Fatalf("extra requested of type with no extra info %v", t)
	}
	noff := types.RoundUp(off, int64(target.PtrSize))
	if noff != off {
		target.Diagnostics.Fatalf("unexpected alignment in dextratype for %v", t)
	}

	for _, a := range m {
		writeType(target, a.type_)
	}

	c := rttype.NewCursor(target.Ctxt, lsym, off, rttype.For(target).UncommonType)
	dgopkgpathOff(target, c.Field("PkgPath"), typePkg(t))

	dataAdd += uncommonSize(target, t)
	mcount := len(m)
	if mcount != int(uint16(mcount)) {
		target.Diagnostics.Fatalf("too many methods on %v: %d", t, mcount)
	}
	xcount := sort.Search(mcount, func(i int) bool { return !types.IsExported(m[i].name.Name) })
	if dataAdd != int(uint32(dataAdd)) {
		target.Diagnostics.Fatalf("methods are too far away on %v: %d", t, dataAdd)
	}

	c.Field("Mcount").WriteUint16(uint16(mcount))
	c.Field("Xcount").WriteUint16(uint16(xcount))
	c.Field("Moff").WriteUint32(uint32(dataAdd))
	// Note: there is an unused uint32 field here.

	// Write the backing array for the []method field.
	array := rttype.NewArrayCursor(target.Ctxt, lsym, off+int64(dataAdd), rttype.For(target).Method, mcount)
	for i, a := range m {
		exported := types.IsExported(a.name.Name)
		var pkg *types.Pkg
		if !exported && a.name.Pkg != typePkg(t) {
			pkg = a.name.Pkg
		}
		nsym := dname(target, a.name.Name, "", pkg, exported, false)

		e := array.Elem(i)
		e.Field("Name").WriteSymPtrOff(nsym, false)
		dmethodptrOff(e.Field("Mtyp"), writeType(target, a.mtype))
		dmethodptrOff(e.Field("Ifn"), a.isym)
		dmethodptrOff(e.Field("Tfn"), a.tsym)
	}
}

func typePkg(t *types.Type) *types.Pkg {
	tsym := t.Sym()
	if tsym == nil {
		switch t.Kind() {
		case types.TARRAY, types.TSLICE, types.TPTR, types.TCHAN:
			if t.Elem() != nil {
				tsym = t.Elem().Sym()
			}
		}
	}
	if tsym != nil && tsym.Pkg != nil && tsym.Pkg.Prefix != "go:builtin" {
		return tsym.Pkg
	}
	return nil
}

func dmethodptrOff(c rttype.Cursor, x *obj.LSym) {
	c.WriteInt32(0)
	c.Reloc(obj.Reloc{Type: objabi.R_METHODOFF, Sym: x})
}

var kinds = []abi.Kind{
	types.TINT:        abi.Int,
	types.TUINT:       abi.Uint,
	types.TINT8:       abi.Int8,
	types.TUINT8:      abi.Uint8,
	types.TINT16:      abi.Int16,
	types.TUINT16:     abi.Uint16,
	types.TINT32:      abi.Int32,
	types.TUINT32:     abi.Uint32,
	types.TINT64:      abi.Int64,
	types.TUINT64:     abi.Uint64,
	types.TUINTPTR:    abi.Uintptr,
	types.TFLOAT32:    abi.Float32,
	types.TFLOAT64:    abi.Float64,
	types.TBOOL:       abi.Bool,
	types.TSTRING:     abi.String,
	types.TPTR:        abi.Pointer,
	types.TSTRUCT:     abi.Struct,
	types.TINTER:      abi.Interface,
	types.TCHAN:       abi.Chan,
	types.TMAP:        abi.Map,
	types.TARRAY:      abi.Array,
	types.TSLICE:      abi.Slice,
	types.TFUNC:       abi.Func,
	types.TCOMPLEX64:  abi.Complex64,
	types.TCOMPLEX128: abi.Complex128,
	types.TUNSAFEPTR:  abi.UnsafePointer,
}

func ABIKindOfType(t *types.Type) abi.Kind {
	return kinds[t.Kind()]
}

// dcommontype dumps the contents of a reflect.rtype (runtime._type) to c.
func dcommontype(target *ir.Package, c rttype.Cursor, t *types.Type) {
	types.CalcSize(t)
	eqfunc := geneq(target, t)

	sptrWeak := true
	var sptr *obj.LSym
	if !t.IsPtr() || t.IsPtrElem() {
		tptr := types.NewPtr(t)
		if t.Sym() != nil || methods(target, tptr) != nil {
			sptrWeak = false
		}
		sptr = writeType(target, tptr)
	}

	gcsym, onDemand, ptrdata := dgcsym(target, t, true, true)
	if !onDemand {
		target.RuntimeTypeMu.Lock()
		delete(target.GCSymTypes, t)
		target.RuntimeTypeMu.Unlock()
	}

	// ../../../../reflect/type.go:/^type.rtype
	// actual type structure
	//	type rtype struct {
	//		size          uintptr
	//		ptrdata       uintptr
	//		hash          uint32
	//		tflag         tflag
	//		align         uint8
	//		fieldAlign    uint8
	//		kind          uint8
	//		equal         func(unsafe.Pointer, unsafe.Pointer) bool
	//		gcdata        *byte
	//		str           nameOff
	//		ptrToThis     typeOff
	//	}
	c.Field("Size_").WriteUintptr(uint64(t.Size()))
	c.Field("PtrBytes").WriteUintptr(uint64(ptrdata))
	c.Field("Hash").WriteUint32(types.TypeHash(t))

	exported := false
	p := t.NameString()
	// If we're writing out type T,
	// we are very likely to write out type *T as well.
	// Use the string "*T"[1:] for "T", so that the two
	// share storage. This is a cheap way to reduce the
	// amount of space taken up by reflect strings.
	if t.TFlag()&abi.TFlagExtraStar != 0 {
		p = "*" + p
		if t.Sym() != nil {
			exported = types.IsExported(t.Sym().Name)
		}
	} else {
		if t.Elem() != nil && t.Elem().Sym() != nil {
			exported = types.IsExported(t.Elem().Sym().Name)
		}
	}

	c.Field("TFlag").WriteUint8(uint8(t.TFlag()))

	// runtime (and common sense) expects alignment to be a power of two.
	i := int(uint8(t.Alignment()))

	if i == 0 {
		i = 1
	}
	if i&(i-1) != 0 {
		target.Diagnostics.Fatalf("invalid alignment %d for %v", uint8(t.Alignment()), t)
	}
	c.Field("Align_").WriteUint8(uint8(t.Alignment()))
	c.Field("FieldAlign_").WriteUint8(uint8(t.Alignment()))

	c.Field("Kind_").WriteUint8(uint8(ABIKindOfType(t)))

	c.Field("Equal").WritePtr(eqfunc)
	c.Field("GCData").WritePtr(gcsym)

	nsym := dname(target, p, "", nil, exported, false)
	c.Field("Str").WriteSymPtrOff(nsym, false)
	c.Field("PtrToThis").WriteSymPtrOff(sptr, sptrWeak)
}

// TrackSym returns the symbol for tracking use of field/method f, assumed
// to be a member of struct/interface type t.
func TrackSym(t *types.Type, f *types.Field) *obj.LSym {
	return t.Context().LinkSym("go:track", t.LinkString()+"."+f.Sym.Name, obj.ABI0)
}

func TypeSymPrefix(target *ir.Package, prefix string, t *types.Type) *types.Sym {
	p := prefix + "." + t.LinkString()
	s := typeSymLookup(target, p)

	// This function is for looking up type-related generated functions
	// (e.g. eq and hash). Make sure they are indeed generated.
	NeedRuntimeType(target, t)

	//print("algsym: %s -> %+S\n", p, s);

	return s
}

func TypeSym(target *ir.Package, t *types.Type) *types.Sym {
	if t == nil || (t.IsPtr() && t.Elem() == nil) || t.IsUntyped() {
		target.Diagnostics.Fatalf("TypeSym %v", t)
	}
	if t.Kind() == types.TFUNC && t.Recv() != nil {
		target.Diagnostics.Fatalf("misuse of method type: %v", t)
	}
	s := typeSymLookup(target, types.TypeSymName(t))
	NeedRuntimeType(target, t)
	return s
}

func TypeLinksymPrefix(target *ir.Package, prefix string, t *types.Type) *obj.LSym {
	return TypeSymPrefix(target, prefix, t).Linksym()
}

func TypeLinksymLookup(target *ir.Package, name string) *obj.LSym {
	return typeSymLookup(target, name).Linksym()
}

func typeSymLookup(target *ir.Package, name string) *types.Sym {
	target.TypeSymMu.Lock()
	defer target.TypeSymMu.Unlock()
	if target.TypeSymPkg == nil {
		target.TypeSymPkg = target.Pkg("type", "type")
	}
	return target.TypeSymPkg.Lookup(name)
}

func TypeLinksym(target *ir.Package, t *types.Type) *obj.LSym {
	lsym := TypeSym(target, t).Linksym()
	setTypeInfo(target, lsym, t)
	return lsym
}

func setTypeInfo(target *ir.Package, lsym *obj.LSym, t *types.Type) {
	target.RuntimeTypeMu.Lock()
	if lsym.Extra == nil {
		ti := lsym.NewTypeInfo()
		ti.Type = t
	}
	target.RuntimeTypeMu.Unlock()
}

// TypePtrAt returns an expression that evaluates to the
// *runtime._type value for t.
func TypePtrAt(curfunc *ir.Func, pos src.XPos, t *types.Type) *ir.AddrExpr {
	return typecheck.LinksymAddr(curfunc, pos, TypeLinksym(curfunc.Target, t), curfunc.Target.Type(types.TUINT8))
}

// ITabLsym returns the LSym representing the itab for concrete type typ implementing
// interface iface. A dummy tab will be created in the unusual case where typ doesn't
// implement iface. Normally, this wouldn't happen, because the typechecker would
// have reported a compile-time error. This situation can only happen when the
// destination type of a type assert or a type in a type switch is parameterized, so
// it may sometimes, but not always, be a type that can't implement the specified
// interface.
func ITabLsym(target *ir.Package, typ, iface *types.Type) *obj.LSym {
	return itabLsym(target, typ, iface, true)
}

func itabLsym(target *ir.Package, typ, iface *types.Type, allowNonImplement bool) *obj.LSym {
	s, existed := target.ItabPkg.LookupOK(typ.LinkString() + "," + iface.LinkString())
	lsym := s.Linksym()
	target.RuntimeTypeMu.Lock()
	if lsym.Extra == nil {
		ii := lsym.NewItabInfo()
		ii.Type = typ
	}
	target.RuntimeTypeMu.Unlock()

	if !existed {
		writeITab(target, lsym, typ, iface, allowNonImplement)
	}
	return lsym
}

// ITabAddrAt returns an expression that evaluates to the
// *runtime.itab value for concrete type typ implementing interface
// iface.
func ITabAddrAt(curfunc *ir.Func, pos src.XPos, typ, iface *types.Type) *ir.AddrExpr {
	lsym := itabLsym(curfunc.Target, typ, iface, false)
	return typecheck.LinksymAddr(curfunc, pos, lsym, curfunc.Target.Type(types.TUINT8))
}

// needkeyupdate reports whether map updates with t as a key
// need the key to be updated.
func needkeyupdate(t *types.Type) bool {
	switch t.Kind() {
	case types.TBOOL, types.TINT, types.TUINT, types.TINT8, types.TUINT8, types.TINT16, types.TUINT16, types.TINT32, types.TUINT32,
		types.TINT64, types.TUINT64, types.TUINTPTR, types.TPTR, types.TUNSAFEPTR, types.TCHAN:
		return false

	case types.TFLOAT32, types.TFLOAT64, types.TCOMPLEX64, types.TCOMPLEX128, // floats and complex can be +0/-0
		types.TINTER,
		types.TSTRING: // strings might have smaller backing stores
		return true

	case types.TARRAY:
		return needkeyupdate(t.Elem())

	case types.TSTRUCT:
		for _, t1 := range t.Fields() {
			if needkeyupdate(t1.Type) {
				return true
			}
		}
		return false

	default:
		t.Context().Diagnostics.Fatalf("bad type for map key: %v", t)
		return true
	}
}

// hashMightPanic reports whether the hash of a map key of type t might panic.
func hashMightPanic(t *types.Type) bool {
	switch t.Kind() {
	case types.TINTER:
		return true

	case types.TARRAY:
		return hashMightPanic(t.Elem())

	case types.TSTRUCT:
		for _, t1 := range t.Fields() {
			if hashMightPanic(t1.Type) {
				return true
			}
		}
		return false

	default:
		return false
	}
}

// formalType replaces predeclared aliases with real types.
// They've been separate internally to make error messages
// better, but we have to merge them in the reflect tables.
func formalType(target *ir.Package, t *types.Type) *types.Type {
	switch t {
	case target.AnyType, target.ByteType, target.RuneType:
		return target.Type(t.Kind())
	}
	return t
}

func writeType(target *ir.Package, t *types.Type) *obj.LSym {
	t = formalType(target, t)
	if t.IsUntyped() {
		target.Diagnostics.Fatalf("writeType %v", t)
	}

	s := TypeSym(target, t)
	lsym := s.Linksym()

	// special case (look for runtime below):
	// when compiling package runtime,
	// emit the type structures for int, float, etc.
	tbase := t
	if t.IsPtr() && t.Sym() == nil && t.Elem().Sym() != nil {
		tbase = t.Elem()
	}
	if tbase.Kind() == types.TFORW {
		target.Diagnostics.Fatalf("unresolved defined type: %v", tbase)
	}

	// This is a fake type we generated for our builtin pseudo-runtime
	// package. We'll emit a description for the real type while
	// compiling package runtime, so we don't need or want to emit one
	// from this fake type.
	if sym := tbase.Sym(); sym != nil && sym.Pkg == target.RuntimePkg {
		return lsym
	}

	if s.Siggen() {
		// The descriptor has already been written. If it was written for a
		// noalg instance of the same type, it left out the algorithms this
		// instance needs, so fill them in, including TFlagRegularMemory.
		if lsym.WeakDef() && !types.TypeHasNoAlg(t) {
			lsym.Set(obj.AttrWeakDef, false)
			if eqfunc := geneq(target, t); eqfunc != nil {
				objw.SymPtr(target.Ctxt, lsym, int(rttype.For(target).Type.OffsetOf("Equal")), eqfunc, 0)
			}
			if types.AlgType(t) == types.AMEM {
				off := int(rttype.For(target).Type.OffsetOf("TFlag"))
				objw.Uint8(target.Ctxt, lsym, off, lsym.P[off]|uint8(abi.TFlagRegularMemory))
			}
		}
		return lsym
	}
	s.SetSiggen(true)

	if !tbase.HasShape() {
		setTypeInfo(target, lsym, t) // ensure lsym.Extra is set
	}

	if !NeedEmit(target, tbase) {
		u := t
		for u.IsPtr() {
			u = u.Elem()
		}
		typecheck.CalcMethods(types.ReceiverBaseType(u))

		if i := typecheck.BaseTypeIndex(target, t); i >= 0 {
			lsym.Pkg = tbase.Sym().Pkg.Prefix
			lsym.SymIdx = int32(i)
			lsym.Set(obj.AttrIndexed, true)
		}

		// TODO(mdempsky): Investigate whether this still happens.
		// If we know we don't need to emit code for a type,
		// we should have a link-symbol index for it.
		// See also TODO in NeedEmit.
		return lsym
	}

	// Type layout                          Written by               Marker
	// +--------------------------------+                            - 0
	// | abi/publicinternal.Type              |   dcommontype
	// +--------------------------------+                            - A
	// | additional type-dependent      |   code in the switch below
	// | fields, e.g.                   |
	// | abi/publicinternal.ArrayType.Len     |
	// +--------------------------------+                            - B
	// | publicinternal/abi.UncommonType      |   dextratype
	// | This section is optional,      |
	// | if type has a name or methods  |
	// +--------------------------------+                            - C
	// | variable-length data           |   code in the switch below
	// | referenced by                  |
	// | type-dependent fields, e.g.    |
	// | abi/publicinternal.StructType.Fields |
	// | dataAdd = size of this section |
	// +--------------------------------+                            - D
	// | method list, if any            |   dextratype
	// +--------------------------------+                            - E

	// publicinternal/abi.Type.DescriptorSize is aware of this type layout,
	// and must be changed if the layout change.

	// UncommonType section is included if we have a name or a method.
	extra := t.Sym() != nil || len(methods(target, t)) != 0

	// Decide the underlying type of the descriptor, and remember
	// the size we need for variable-length data.
	var rt *types.Type
	dataAdd := 0
	switch t.Kind() {
	default:
		rt = rttype.For(target).Type
	case types.TARRAY:
		rt = rttype.For(target).ArrayType
	case types.TSLICE:
		rt = rttype.For(target).SliceType
	case types.TCHAN:
		rt = rttype.For(target).ChanType
	case types.TFUNC:
		rt = rttype.For(target).FuncType
		dataAdd = (t.NumRecvs() + t.NumParams() + t.NumResults()) * target.PtrSize
	case types.TINTER:
		rt = rttype.For(target).InterfaceType
		dataAdd = len(imethods(target, t)) * int(rttype.For(target).IMethod.Size())
	case types.TMAP:
		rt = rttype.For(target).MapType
	case types.TPTR:
		rt = rttype.For(target).PtrType
		// TODO: use rttype.For(target).Type for Elem() is ANY?
	case types.TSTRUCT:
		rt = rttype.For(target).StructType
		dataAdd = t.NumFields() * int(rttype.For(target).StructField.Size())
	}

	// Compute offsets of each section.
	B := rt.Size()
	C := B
	if extra {
		C = B + rttype.For(target).UncommonType.Size()
	}
	D := C + int64(dataAdd)
	E := D + int64(len(methods(target, t)))*rttype.For(target).Method.Size()

	// Write the runtime._type
	c := rttype.NewCursor(target.Ctxt, lsym, 0, rt)
	if rt == rttype.For(target).Type {
		dcommontype(target, c, t)
	} else {
		dcommontype(target, c.Field("Type"), t)
	}

	// Write additional type-specific data
	// (Both the fixed size and variable-sized sections.)
	switch t.Kind() {
	case types.TARRAY:
		// publicinternal/abi.ArrayType
		s1 := writeType(target, t.Elem())
		t2 := types.NewSlice(t.Elem())
		s2 := writeType(target, t2)
		c.Field("Elem").WritePtr(s1)
		c.Field("Slice").WritePtr(s2)
		c.Field("Len").WriteUintptr(uint64(t.NumElem()))

	case types.TSLICE:
		// publicinternal/abi.SliceType
		s1 := writeType(target, t.Elem())
		c.Field("Elem").WritePtr(s1)

	case types.TCHAN:
		// publicinternal/abi.ChanType
		s1 := writeType(target, t.Elem())
		c.Field("Elem").WritePtr(s1)
		c.Field("Dir").WriteInt(int64(t.ChanDir()))

	case types.TFUNC:
		// publicinternal/abi.FuncType
		for _, t1 := range t.RecvParamsResults() {
			writeType(target, t1.Type)
		}
		inCount := t.NumRecvs() + t.NumParams()
		outCount := t.NumResults()
		if t.IsVariadic() {
			outCount |= 1 << 15
		}

		c.Field("InCount").WriteUint16(uint16(inCount))
		c.Field("OutCount").WriteUint16(uint16(outCount))

		// Array of rtype pointers follows funcType.
		typs := t.RecvParamsResults()
		array := rttype.NewArrayCursor(target.Ctxt, lsym, C, target.Type(types.TUNSAFEPTR), len(typs))
		for i, t1 := range typs {
			array.Elem(i).WritePtr(writeType(target, t1.Type))
		}

	case types.TINTER:
		// publicinternal/abi.InterfaceType
		m := imethods(target, t)
		n := len(m)
		for _, a := range m {
			writeType(target, a.type_)
		}

		var tpkg *types.Pkg
		if t.Sym() != nil && t != target.Type(t.Kind()) && t != target.ErrorType {
			tpkg = t.Sym().Pkg
		}
		dgopkgpath(target, c.Field("PkgPath"), tpkg)
		c.Field("Methods").WriteSlice(lsym, C, int64(n), int64(n))

		array := rttype.NewArrayCursor(target.Ctxt, lsym, C, rttype.For(target).IMethod, n)
		for i, a := range m {
			exported := types.IsExported(a.name.Name)
			var pkg *types.Pkg
			if !exported && a.name.Pkg != tpkg {
				pkg = a.name.Pkg
			}
			nsym := dname(target, a.name.Name, "", pkg, exported, false)

			e := array.Elem(i)
			e.Field("Name").WriteSymPtrOff(nsym, false)
			e.Field("Typ").WriteSymPtrOff(writeType(target, a.type_), false)
		}

	case types.TMAP:
		writeMapType(target, t, lsym, c)

	case types.TPTR:
		// publicinternal/abi.PtrType
		if t.Elem().Kind() == types.TANY {
			target.Diagnostics.Fatalf("bad pointer base type")
		}

		s1 := writeType(target, t.Elem())
		c.Field("Elem").WritePtr(s1)

	case types.TSTRUCT:
		// publicinternal/abi.StructType
		fields := t.Fields()
		for _, t1 := range fields {
			writeType(target, t1.Type)
		}

		// All non-exported struct field names within a struct
		// type must originate from a single package. By
		// identifying and recording that package within the
		// struct type descriptor, we can omit that
		// information from the field descriptors.
		var spkg *types.Pkg
		for _, f := range fields {
			if !types.IsExported(f.Sym.Name) {
				spkg = f.Sym.Pkg
				break
			}
		}

		dgopkgpath(target, c.Field("PkgPath"), spkg)
		c.Field("Fields").WriteSlice(lsym, C, int64(len(fields)), int64(len(fields)))

		array := rttype.NewArrayCursor(target.Ctxt, lsym, C, rttype.For(target).StructField, len(fields))
		for i, f := range fields {
			e := array.Elem(i)
			dnameField(target, e.Field("Name"), spkg, f)
			e.Field("Typ").WritePtr(writeType(target, f.Type))
			e.Field("Offset").WriteUintptr(uint64(f.Offset))
		}
	}

	// Write the extra info, if any.
	if extra {
		dextratype(target, lsym, B, t, dataAdd)
	}

	dupok := 0
	if TypeCanBeDupok(tbase) {
		dupok = obj.DUPOK
	}

	objw.Global(target.Ctxt, lsym, int32(E), int16(dupok|obj.RODATA))

	// A noalg type gets a descriptor without the type's hash and equality
	// algorithms, because this package has no use for them. Another package
	// may well have one, and emit a descriptor for the same type that does
	// link them. Both describe the same type and carry the same name, so
	// mark this one weak: the descriptor with the algorithms is usable
	// everywhere this one is, and has to be the one the linker keeps.
	// See cmd/publicinternal/obj.AttrWeakDef.
	if types.TypeHasNoAlg(t) {
		lsym.Set(obj.AttrWeakDef, true)
	}

	// The linker will leave a table of all the typelinks for
	// types in the binary, so the runtime can find them.
	//
	// When buildmode=shared, all types are in typelinks so the
	// runtime can deduplicate type pointers.
	keep := target.Ctxt.Flag_dynlink
	if !keep && t.Sym() == nil {
		// For an unnamed type, we only need the link if the type can
		// be created at run time by reflect.PointerTo and similar
		// functions. If the type exists in the program, those
		// functions must return the existing type structure rather
		// than creating a new one.
		switch t.Kind() {
		case types.TPTR, types.TARRAY, types.TCHAN, types.TFUNC, types.TMAP, types.TSLICE, types.TSTRUCT:
			keep = true
		}
	}
	// Do not put Noalg types in typelinks.  See issue #22605.
	if types.TypeHasNoAlg(t) {
		keep = false
	}
	lsym.Set(obj.AttrMakeTypelink, keep)
	lsym.Align = int16(target.PtrSize)

	return lsym
}

// InterfaceMethodOffset returns the offset of the i-th method in the interface
// type descriptor, ityp.
func InterfaceMethodOffset(target *ir.Package, ityp *types.Type, i int64) int64 {
	// interface type descriptor layout is struct {
	//   _type        // commonSize
	//   pkgpath      // 1 word
	//   []imethod    // 3 words (pointing to [...]imethod below)
	//   uncommontype // uncommonSize
	//   [...]imethod
	// }
	// The size of imethod is 8.
	return int64(commonSize(target)+4*target.PtrSize+uncommonSize(target, ityp)) + i*8
}

// NeedRuntimeType ensures that a runtime type descriptor is emitted for t.
func NeedRuntimeType(target *ir.Package, t *types.Type) {
	target.RuntimeTypeMu.Lock()
	if target.RuntimeTypes == nil {
		target.RuntimeTypes = make(map[*types.Type]struct{})
	}
	if _, ok := target.RuntimeTypes[t]; !ok {
		target.RuntimeTypes[t] = struct{}{}
		target.RuntimeTypeQueue = append(target.RuntimeTypeQueue, t)
	}
	target.RuntimeTypeMu.Unlock()
}

// ForEachRuntimeType calls fn for each type for which a runtime type descriptor
// has been requested. This is used by the code that indexes symbols early, so
// we can recursively index the symbols WriteRuntimeTypes numbers using the
// recursive writeType function.
func ForEachRuntimeType(target *ir.Package, fn func(*types.Type)) {
	target.RuntimeTypeMu.Lock()
	queue := append([]*types.Type(nil), target.RuntimeTypeQueue...)
	target.RuntimeTypeMu.Unlock()
	for _, t := range queue {
		fn(t)
	}
}

func WriteRuntimeTypes(target *ir.Package) {
	// Process the runtime type queue. Use a loop, as writeType adds
	// entries to the queue while it is being processed.
	for {
		target.RuntimeTypeMu.Lock()
		if len(target.RuntimeTypeQueue) == 0 {
			target.RuntimeTypeMu.Unlock()
			break
		}
		signats := append([]*types.Type(nil), target.RuntimeTypeQueue...)
		target.RuntimeTypeMu.Unlock()
		entries := make([]typeAndStr, len(signats))
		for i, t := range signats {
			entries[i] = typeAndStr{t: t, short: types.TypeSymName(t), regular: t.String()}
		}
		// Sort for reproducible builds.
		slices.SortFunc(entries, typesStrCmp)
		for _, ts := range entries {
			t := ts.t
			writeType(target, t)
			if t.Sym() != nil {
				writeType(target, types.NewPtr(t))
			}
		}
		target.RuntimeTypeMu.Lock()
		target.RuntimeTypeQueue = target.RuntimeTypeQueue[len(signats):]
		target.RuntimeTypeMu.Unlock()
	}
}

func WriteGCSymbols(target *ir.Package) {
	// Emit GC data symbols.
	target.RuntimeTypeMu.Lock()
	gcsyms := make([]typeAndStr, 0, len(target.GCSymTypes))
	for t := range target.GCSymTypes {
		gcsyms = append(gcsyms, typeAndStr{t: t, short: types.TypeSymName(t), regular: t.String()})
	}
	target.RuntimeTypeMu.Unlock()
	slices.SortFunc(gcsyms, typesStrCmp)
	for _, ts := range gcsyms {
		dgcsym(target, ts.t, true, false)
	}
}

// writeITab writes the itab for concrete type typ implementing interface iface. If
// allowNonImplement is true, allow the case where typ does not implement iface, and just
// create a dummy itab with zeroed-out method entries.
func writeITab(target *ir.Package, lsym *obj.LSym, typ, iface *types.Type, allowNonImplement bool) {
	// TODO(mdempsky): Fix geneq and genhash (and maybe others) to stop
	// clobbering base.Pos.
	oldpos := target.Pos
	defer ir.SetPosValue(target, oldpos)

	if typ == nil || (typ.IsPtr() && typ.Elem() == nil) || typ.IsUntyped() || iface == nil || !iface.IsInterface() || iface.IsEmptyInterface() {
		target.Diagnostics.Fatalf("writeITab(%v, %v)", typ, iface)
	}

	sigs := iface.AllMethods()
	entries := make([]*obj.LSym, 0, len(sigs))

	// both sigs and methods are sorted by name,
	// so we can find the intersection in a single pass
	for _, m := range methods(target, typ) {
		if types.CompareSyms(m.name, sigs[0].Sym) == 0 {
			entries = append(entries, m.isym)
			if m.isym == nil {
				panic("NO ISYM")
			}
			sigs = sigs[1:]
			if len(sigs) == 0 {
				break
			}
		}
	}
	completeItab := len(sigs) == 0
	if !allowNonImplement && !completeItab {
		target.Diagnostics.Fatalf("incomplete itab")
	}

	// dump empty itab symbol into i.sym
	// type itab struct {
	//   inter  *interfacetype
	//   _type  *_type
	//   hash   uint32 // copy of _type.hash. Used for type switches.
	//   _      [4]byte
	//   fun    [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
	// }
	c := rttype.NewCursor(target.Ctxt, lsym, 0, rttype.For(target).ITab)
	c.Field("Inter").WritePtr(writeType(target, iface))
	c.Field("Type").WritePtr(writeType(target, typ))
	c.Field("Hash").WriteUint32(types.TypeHash(typ)) // copy of type hash

	var delta int64
	c = c.Field("Fun")
	if !completeItab {
		// If typ doesn't implement iface, make method entries be zero.
		c.Elem(0).WriteUintptr(0)
	} else {
		var a rttype.ArrayCursor
		a, delta = c.ModifyArray(len(entries))
		for i, fn := range entries {
			a.Elem(i).WritePtrWeak(fn) // method pointer for each method
		}
	}
	// Nothing writes static itabs, so they are read only.
	objw.Global(target.Ctxt, lsym, int32(rttype.For(target).ITab.Size()+delta), int16(obj.DUPOK|obj.RODATA))
	lsym.Set(obj.AttrContentAddressable, true)
	lsym.Align = int16(target.PtrSize)
}

func WritePluginTable(target *ir.Package) {
	ptabs := target.PluginExports
	if len(ptabs) == 0 {
		return
	}

	lsym := target.Ctxt.Lookup("go:plugin.tabs")
	ot := 0
	for _, p := range ptabs {
		// Dump ptab symbol into go.pluginsym package.
		//
		// type ptab struct {
		//	name nameOff
		//	typ  typeOff // pointer to symbol
		// }
		nsym := dname(target, p.Sym().Name, "", nil, true, false)
		t := p.Type()
		if p.Class != ir.PFUNC {
			t = types.NewPtr(t)
		}
		tsym := writeType(target, t)
		ot = objw.SymPtrOff(target.Ctxt, lsym, ot, nsym)
		ot = objw.SymPtrOff(target.Ctxt, lsym, ot, tsym)
		// Plugin exports symbols as interfaces. Mark their types
		// as UsedInIface.
		tsym.Set(obj.AttrUsedInIface, true)
	}
	objw.Global(target.Ctxt, lsym, int32(ot), int16(obj.RODATA))

	lsym = target.Ctxt.Lookup("go:plugin.exports")
	ot = 0
	for _, p := range ptabs {
		ot = objw.SymPtr(target.Ctxt, lsym, ot, p.Linksym(), 0)
	}
	objw.Global(target.Ctxt, lsym, int32(ot), int16(obj.RODATA))
}

// writtenByWriteBasicTypes reports whether typ is written by WriteBasicTypes.
// WriteBasicTypes always writes pointer types; any pointer has been stripped off typ already.
func writtenByWriteBasicTypes(target *ir.Package, typ *types.Type) bool {
	if typ.Sym() == nil && typ.Kind() == types.TFUNC {
		// func(error) string
		if typ.NumRecvs() == 0 &&
			typ.NumParams() == 1 && typ.NumResults() == 1 &&
			typ.Param(0).Type == target.ErrorType &&
			typ.Result(0).Type == target.Type(types.TSTRING) {
			return true
		}
	}

	// Now we have left the basic types plus any and error, plus slices of them.
	// Strip the slice.
	if typ.Sym() == nil && typ.IsSlice() {
		typ = typ.Elem()
	}

	// Basic types.
	sym := typ.Sym()
	if sym != nil && sym.Pkg != nil && (sym.Pkg.Prefix == "go:builtin" || sym.Pkg.Path == "unsafe") {
		return true
	}
	// any or error
	return (sym == nil && typ.IsEmptyInterface()) || typ == target.ErrorType
}

func WriteBasicTypes(target *ir.Package) {
	// do basic types if compiling package runtime.
	// they have to be in at least one package,
	// and runtime is always loaded implicitly,
	// so this is as good as any.
	// another possible choice would be package main,
	// but using runtime means fewer copies in object files.
	// The code here needs to be in sync with writtenByWriteBasicTypes above.
	if target.Ctxt.Pkgpath != "runtime" {
		return
	}

	// Note: always write NewPtr(t) because NeedEmit's caller strips the pointer.
	var list []*types.Type
	for i := types.Kind(1); i <= types.TBOOL; i++ {
		list = append(list, target.Type(i))
	}
	list = append(list,
		target.Type(types.TSTRING),
		target.Type(types.TUNSAFEPTR),
		target.AnyType,
		target.ErrorType)
	for _, t := range list {
		writeType(target, types.NewPtr(t))
		writeType(target, types.NewPtr(types.NewSlice(t)))
	}

	// emit type for func(error) string,
	// which is the type of an auto-generated wrapper.
	writeType(target, types.NewPtr(types.NewSignature(nil, []*types.Field{
		types.NewField(target.AutoPos, nil, target.ErrorType),
	}, []*types.Field{
		types.NewField(target.AutoPos, nil, target.Type(types.TSTRING)),
	})))
}

type typeAndStr struct {
	t       *types.Type
	short   string // "short" here means TypeSymName
	regular string
}

func typesStrCmp(a, b typeAndStr) int {
	// put named types before unnamed types
	if a.t.Sym() != nil && b.t.Sym() == nil {
		return -1
	}
	if a.t.Sym() == nil && b.t.Sym() != nil {
		return +1
	}

	if r := strings.Compare(a.short, b.short); r != 0 {
		return r
	}
	// When the only difference between the types is whether
	// they refer to byte or uint8, such as **byte vs **uint8,
	// the types' NameStrings can be identical.
	// To preserve deterministic sort ordering, sort these by String().
	//
	// TODO(mdempsky): This all seems suspect. Using LinkString would
	// avoid naming collisions, and there shouldn't be a reason to care
	// about "byte" vs "uint8": they share the same runtime type
	// descriptor anyway.
	if r := strings.Compare(a.regular, b.regular); r != 0 {
		return r
	}
	// A type and the noalg version of it share a name, so nothing above
	// tells them apart. Order the one that has the algorithms first: its
	// descriptor is the one that has to survive. See #82043.
	if an, bn := types.TypeHasNoAlg(a.t), types.TypeHasNoAlg(b.t); an != bn {
		if bn {
			return -1
		}
		return +1
	}
	// Identical anonymous interfaces defined in different locations
	// will be equal for the above checks, but different in DWARF output.
	// Sort by source position to ensure deterministic order.
	// See issues 27013 and 30202.
	if a.t.Kind() == types.TINTER && len(a.t.AllMethods()) > 0 {
		if a.t.AllMethods()[0].Pos.Before(b.t.AllMethods()[0].Pos) {
			return -1
		}
		return +1
	}
	return 0
}

// GCSym returns a data symbol containing GC information for type t.
// GC information is always a bitmask, never a gc program.
// GCSym may be called in concurrent backend, so it does not emit the symbol
// content.
func GCSym(target *ir.Package, t *types.Type, onDemandAllowed bool) (lsym *obj.LSym, ptrdata int64) {
	// Record that we need to emit the GC symbol.
	target.RuntimeTypeMu.Lock()
	if target.GCSymTypes == nil {
		target.GCSymTypes = make(map[*types.Type]struct{})
	}
	target.GCSymTypes[t] = struct{}{}
	target.RuntimeTypeMu.Unlock()

	lsym, _, ptrdata = dgcsym(target, t, false, onDemandAllowed)
	return
}

// dgcsym returns a data symbol containing GC information for type t, along
// with a boolean reporting whether the gc mask should be computed on demand
// at runtime, and the ptrdata field to record in the reflect type information.
// When write is true, it writes the symbol data.
func dgcsym(target *ir.Package, t *types.Type, write, onDemandAllowed bool) (lsym *obj.LSym, onDemand bool, ptrdata int64) {
	ptrdata = types.PtrDataSize(t)
	if !onDemandAllowed || t.TFlag()&abi.TFlagGCMaskOnDemand == 0 {
		lsym = dgcptrmask(target, t, write)
		return
	}

	onDemand = true
	lsym = dgcptrmaskOnDemand(target, t, write)
	return
}

// dgcptrmask emits and returns the symbol containing a pointer mask for type t.
func dgcptrmask(target *ir.Package, t *types.Type, write bool) *obj.LSym {
	// Bytes we need for the ptrmask.
	n := (types.PtrDataSize(t)/int64(target.PtrSize) + 7) / 8
	// Runtime wants ptrmasks padded to a multiple of uintptr in size.
	n = (n + int64(target.PtrSize) - 1) &^ (int64(target.PtrSize) - 1)
	ptrmask := make([]byte, n)
	fillptrmask(target, t, ptrmask)
	p := fmt.Sprintf("runtime.gcbits.%x", ptrmask)

	lsym := target.Ctxt.Lookup(p)
	if write && !lsym.OnList() {
		for i, x := range ptrmask {
			objw.Uint8(target.Ctxt, lsym, i, x)
		}
		objw.Global(target.Ctxt, lsym, int32(len(ptrmask)), obj.DUPOK|obj.RODATA|obj.LOCAL)
		lsym.Set(obj.AttrContentAddressable, true)
		// The runtime expects ptrmasks to be aligned
		// as a uintptr.
		lsym.Align = int16(target.PtrSize)
	}
	return lsym
}

// fillptrmask fills in ptrmask with 1s corresponding to the
// word offsets in t that hold pointers.
// ptrmask is assumed to fit at least types.PtrDataSize(t)/PtrSize bits.
func fillptrmask(target *ir.Package, t *types.Type, ptrmask []byte) {
	if !t.HasPointers() {
		return
	}

	vec := bitvec.New(8 * int32(len(ptrmask)))
	typebits.Set(target.PtrSize, t, 0, vec)

	nptr := types.PtrDataSize(t) / int64(target.PtrSize)
	for i := int64(0); i < nptr; i++ {
		if vec.Get(int32(i)) {
			ptrmask[i/8] |= 1 << (uint(i) % 8)
		}
	}
}

// dgcptrmaskOnDemand emits and returns the symbol that should be referenced by
// the GCData field of a type, for large types.
func dgcptrmaskOnDemand(target *ir.Package, t *types.Type, write bool) *obj.LSym {
	lsym := TypeLinksymPrefix(target, ".gcmask", t)
	if write && !lsym.OnList() {
		// Note: contains a pointer, but a pointer to a
		// persistentalloc allocation. Starts with nil.
		// Allocated in BSS.
		objw.Global(target.Ctxt, lsym, int32(target.PtrSize), obj.DUPOK|obj.NOPTR|obj.LOCAL)
	}
	return lsym
}

// ZeroAddr returns the address of a symbol with at least
// size bytes of zeros.
func ZeroAddr(curfunc *ir.Func, size int64) ir.Node {
	if size >= 1<<31 {
		curfunc.Target.Diagnostics.Fatalf("map elem too big %d", size)
	}
	if curfunc.Target.ZeroSize < size {
		curfunc.Target.ZeroSize = size
	}
	lsym := curfunc.Target.TypeContext.LinkSym("go:map", "zero", obj.ABI0)
	x := ir.NewLinksymExpr(curfunc.Target.Pos, lsym, curfunc.Target.Type(types.TUINT8))
	return typecheck.Expr(curfunc, typecheck.NodAddr(curfunc, x))
}

// TypeCanBeDupok reports whether the type descriptor can be defined in multiple packages:
// that is, unnamed types, instantiated types and shape types.
func TypeCanBeDupok(t *types.Type) bool {
	return t.Sym() == nil || t.IsFullyInstantiated() || t.HasShape()
}

// NeedEmit reports whether typ is a type that we need to emit code
// for (e.g., runtime type descriptors, method wrappers).
func NeedEmit(target *ir.Package, typ *types.Type) bool {
	// TODO(mdempsky): Export data should keep track of which anonymous
	// and instantiated types were emitted, so at least downstream
	// packages can skip re-emitting them.
	//
	// Perhaps we can just generalize the linker-symbol indexing to
	// track the index of arbitrary types, not just defined types, and
	// use its presence to detect this. The same idea would work for
	// instantiated generic functions too.

	switch sym := typ.Sym(); {
	case writtenByWriteBasicTypes(target, typ):
		return target.Ctxt.Pkgpath == "runtime"

	case sym == nil:
		// Anonymous type; possibly never seen before or ever again.
		// Need to emit to be safe (however, see TODO above).
		return true

	case sym.Pkg == target.LocalPkg:
		// Local defined type; our responsibility.
		return true

	case typ.IsFullyInstantiated():
		// Instantiated type; possibly instantiated with unique type arguments.
		// Need to emit to be safe (however, see TODO above).
		return true

	case typ.HasShape():
		// Shape type; need to emit even though it lives in the .shape package.
		// TODO: make sure the linker deduplicates them (see dupok in writeType above).
		return true

	default:
		// Should have been emitted by an imported package.
		return false
	}
}

// Generate a wrapper function to convert from
// a receiver of type T to a receiver of type U.
// That is,
//
//	func (t T) M() {
//		...
//	}
//
// already exists; this function generates
//
//	func (u U) M() {
//		u.M()
//	}
//
// where the types T and U are such that u.M() is valid
// and calls the T.M method.
// The resulting function is for use in method tables.
//
//	rcvr - U
//	method - M func (t T)(), a TFIELD type struct
//
// Also wraps methods on instantiated generic types for use in itab entries.
// For an instantiated generic type G[int], we generate wrappers like:
// G[int] pointer shaped:
//
//	func (x G[int]) f(arg) {
//		.inst.G[int].f(dictionary, x, arg)
//	}
//
// G[int] not pointer shaped:
//
//	func (x *G[int]) f(arg) {
//		.inst.G[int].f(dictionary, *x, arg)
//	}
//
// These wrappers are always fully stenciled.
func methodWrapper(target *ir.Package, rcvr *types.Type, method *types.Field, forItab bool) *obj.LSym {
	if forItab && !types.IsDirectIface(rcvr) {
		rcvr = rcvr.PtrTo()
	}

	sym, _ := ir.MethodSym(target, rcvr, method)
	lsym := sym.Linksym()

	// Unified IR creates its own wrappers.
	return lsym
}

// MarkTypeUsedInInterface marks that type t is converted to an interface.
// This information is used in the linker in dead method elimination.
func MarkTypeUsedInInterface(target *ir.Package, t *types.Type, from *obj.LSym) {
	if t.HasShape() {
		// Shape types shouldn't be put in interfaces, so we shouldn't ever get here.
		target.Diagnostics.Fatalf("shape types have no methods %+v", t)
	}
	MarkTypeSymUsedInInterface(target, TypeLinksym(target, t), from)
}
func MarkTypeSymUsedInInterface(target *ir.Package, tsym *obj.LSym, from *obj.LSym) {
	// Emit a marker relocation. The linker will know the type is converted
	// to an interface if "from" is reachable.
	from.AddRel(target.Ctxt, obj.Reloc{Type: objabi.R_USEIFACE, Sym: tsym})
}

// MarkUsedIfaceMethod marks that an interface method is used in the current
// function. n is OCALLINTER node.
func MarkUsedIfaceMethod(curfunc *ir.Func, n *ir.CallExpr) {
	// skip unnamed functions (func _())
	if curfunc.LSym == nil {
		return
	}
	dot := n.Fun.(*ir.SelectorExpr)
	ityp := dot.X.Type()
	if ityp.HasShape() {
		// Here we're calling a method on a generic interface. Something like:
		//
		// type I[T any] interface { foo() T }
		// func f[T any](x I[T]) {
		//     ... = x.foo()
		// }
		// f[int](...)
		// f[string](...)
		//
		// In this case, in f we're calling foo on a generic interface.
		// Which method could that be? Normally we could match the method
		// both by name and by type. But in this case we don't really know
		// the type of the method we're calling. It could be func()int
		// or func()string. So we match on just the function name, instead
		// of both the name and the type used for the non-generic case below.
		// TODO: instantiations at least know the shape of the instantiated
		// type, and the linker could do more complicated matching using
		// some sort of fuzzy shape matching. For now, only use the name
		// of the method for matching.
		curfunc.LSym.AddRel(curfunc.Target.Ctxt, obj.Reloc{
			Type: objabi.R_USENAMEDMETHOD,
			Sym:  staticdata.StringSymNoCommon(curfunc.Target, dot.Sel.Name),
		})
		return
	}

	// dot.Offset() is the method index * PtrSize (the offset of code pointer in itab).
	midx := dot.Offset() / int64(curfunc.Target.PtrSize)
	curfunc.LSym.AddRel(curfunc.Target.Ctxt, obj.Reloc{
		Type: objabi.R_USEIFACEMETHOD,
		Sym:  TypeLinksym(curfunc.Target, ityp),
		Add:  InterfaceMethodOffset(curfunc.Target, ityp, midx),
	})
}
