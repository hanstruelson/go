// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"encoding/base64"
	"fmt"
	"go/constant"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/objw"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/hash"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"cmd/publicinternal/src"
)

// InitAddrOffset writes the static name symbol lsym to n, it does not modify n.
// It's the caller responsibility to make sure lsym is from ONAME/PEXTERN node.
func InitAddrOffset(target *ir.Package, n *ir.Name, noff int64, lsym *obj.LSym, off int64) {
	if n.Op() != ir.ONAME {
		target.Diagnostics.Fatalf("InitAddr n op %v", n.Op())
	}
	if n.Sym() == nil {
		target.Diagnostics.Fatalf("InitAddr nil n sym")
	}
	s := n.Linksym()
	s.WriteAddr(target.Ctxt, noff, target.PtrSize, lsym, off)
}

// InitAddr is InitAddrOffset, with offset fixed to 0.
func InitAddr(target *ir.Package, n *ir.Name, noff int64, lsym *obj.LSym) {
	InitAddrOffset(target, n, noff, lsym, 0)
}

// InitSlice writes a static slice symbol {lsym, lencap, lencap} to n+noff, it does not modify n.
// It's the caller responsibility to make sure lsym is from ONAME node.
func InitSlice(target *ir.Package, n *ir.Name, noff int64, lsym *obj.LSym, lencap int64) {
	s := n.Linksym()
	s.WriteAddr(target.Ctxt, noff, target.PtrSize, lsym, 0)
	s.WriteInt(target.Ctxt, noff+int64(target.PtrSize), target.PtrSize, lencap)
	s.WriteInt(target.Ctxt, noff+2*int64(target.PtrSize), target.PtrSize, lencap)
}

func InitSliceBytes(target *ir.Package, nam *ir.Name, off int64, s string) {
	if nam.Op() != ir.ONAME {
		target.Diagnostics.Fatalf("InitSliceBytes %v", nam)
	}
	InitSlice(target, nam, off, slicedata(target, nam.Sym().Pkg, nam.Pos(), s), int64(len(s)))
}

const (
	stringSymPrefix  = "go:string."
	stringSymPattern = ".gostring.%d.%s"
)

// shortHashString converts the hash to a string for use with stringSymPattern.
// We cut it to 16 bytes and then base64-encode to make it even smaller.
func shortHashString(hash []byte) string {
	return base64.StdEncoding.EncodeToString(hash[:16])
}

// StringSym returns a symbol containing the string s.
// The symbol contains the string data, not a string header.
func StringSym(target *ir.Package, pos src.XPos, s string) (data *obj.LSym) {
	var symname string
	if len(s) > 100 {
		// Huge strings are hashed to avoid long names in object files.
		// Indulge in some paranoia by writing the length of s, too,
		// as protection against length extension attacks.
		// Same pattern is known to fileStringSym below.
		h := hash.New32()
		io.WriteString(h, s)
		symname = fmt.Sprintf(stringSymPattern, len(s), shortHashString(h.Sum(nil)))
	} else {
		// Small strings get named directly by their contents.
		symname = strconv.Quote(s)
	}

	symdata := target.Ctxt.Lookup(stringSymPrefix + symname)
	if !symdata.OnList() {
		off := dstringdata(target, symdata, 0, s, pos, "string")
		objw.Global(target.Ctxt, symdata, int32(off), obj.DUPOK|obj.RODATA|obj.LOCAL)
		symdata.Set(obj.AttrContentAddressable, true)
		symdata.Align = 1
	}

	return symdata
}

// StringSymNoCommon is like StringSym, but produces a symbol that is not content-
// addressable. This symbol is not supposed to appear in the final binary, it is
// only used to pass string arguments to the linker like R_USENAMEDMETHOD does.
func StringSymNoCommon(target *ir.Package, s string) (data *obj.LSym) {
	var nameSym obj.LSym
	nameSym.WriteString(target.Ctxt, 0, len(s), s)
	objw.Global(target.Ctxt, &nameSym, int32(len(s)), obj.RODATA)
	return &nameSym
}

// maxFileSize is the maximum file size permitted by the linker
// (see issue #9862).
const maxFileSize = obj.MaxSymSize

// fileStringSym returns a symbol for the contents and the size of file.
// If readonly is true, the symbol shares storage with any literal string
// or other file with the same content and is placed in a read-only section.
// If readonly is false, the symbol is a read-write copy separate from any other,
// for use as the backing store of a []byte.
// The content hash of file is copied into hashBytes. (If hash is nil, nothing is copied.)
// The returned symbol contains the data itself, not a string header.
func fileStringSym(target *ir.Package, pkg *types.Pkg, pos src.XPos, file string, readonly bool, hashBytes []byte) (*obj.LSym, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	size := info.Size()
	if size <= 1*1024 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, 0, err
		}
		if int64(len(data)) != size {
			return nil, 0, fmt.Errorf("file changed between reads")
		}
		var sym *obj.LSym
		if readonly {
			sym = StringSym(target, pos, string(data))
		} else {
			sym = slicedata(target, pkg, pos, string(data))
		}
		if len(hashBytes) > 0 {
			sum := hash.Sum32(data)
			copy(hashBytes, sum[:])
		}
		return sym, size, nil
	}
	if size > maxFileSize {
		// ggloblsym takes an int32,
		// and probably the rest of the toolchain
		// can't handle such big symbols either.
		// See golang.org/issue/9862.
		return nil, 0, fmt.Errorf("file too large (%d bytes > %d bytes)", size, maxFileSize)
	}

	// File is too big to read and keep in memory.
	// Compute hashBytes if needed for read-only content hashing or if the caller wants it.
	var sum []byte
	if readonly || len(hashBytes) > 0 {
		h := hash.New32()
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, 0, err
		}
		if n != size {
			return nil, 0, fmt.Errorf("file changed between reads")
		}
		sum = h.Sum(nil)
		copy(hashBytes, sum)
	}

	var symdata *obj.LSym
	if readonly {
		symname := fmt.Sprintf(stringSymPattern, size, shortHashString(sum))
		symdata = target.Ctxt.Lookup(stringSymPrefix + symname)
		if !symdata.OnList() {
			info := symdata.NewFileInfo()
			info.Name = file
			info.Size = size
			objw.Global(target.Ctxt, symdata, int32(size), obj.DUPOK|obj.RODATA|obj.LOCAL)
			symdata.Align = 1
			// Note: AttrContentAddressable cannot be set here,
			// because the content-addressable-handling code
			// does not know about file symbols.
		}
	} else {
		// Emit a zero-length data symbol
		// and then fix up length and content to use file.
		symdata = slicedata(target, pkg, pos, "")
		symdata.Size = size
		symdata.Type = objabi.SNOPTRDATA
		info := symdata.NewFileInfo()
		info.Name = file
		info.Size = size
	}

	return symdata, size, nil
}

func slicedata(target *ir.Package, pkg *types.Pkg, pos src.XPos, s string) *obj.LSym {
	pkg.SlicedataGen++
	symname := fmt.Sprintf(".gobytes.%d", pkg.SlicedataGen)
	lsym := pkg.Lookup(symname).LinksymABI(obj.ABI0)
	off := dstringdata(target, lsym, 0, s, pos, "slice")
	objw.Global(target.Ctxt, lsym, int32(off), obj.NOPTR|obj.LOCAL)
	lsym.Align = 1

	return lsym
}

func dstringdata(target *ir.Package, s *obj.LSym, off int, t string, pos src.XPos, what string) int {
	// Objects that are too large will cause the data section to overflow right away,
	// causing a cryptic error message by the linker. Check for oversize objects here
	// and provide a useful error message instead.
	if int64(len(t)) > 2e9 {
		target.Diagnostics.ErrorfAt(pos, 0, "%v with length %v is too big", what, len(t))
		return 0
	}

	s.WriteString(target.Ctxt, int64(off), len(t), t)
	return off + len(t)
}

// FuncLinksym returns n·f, the function value symbol for n.
func FuncLinksym(target *ir.Package, n *ir.Name) *obj.LSym {
	if n.Op() != ir.ONAME || n.Class != ir.PFUNC {
		target.Diagnostics.Fatalf("expected func name: %v", n)
	}
	s := n.Sym()

	// FuncSymsMu here serves to protect not just mutations of FuncSyms (below),
	// but also the package lookup of the func sym name,
	// since this function gets called concurrently from the backend.
	// There are no other concurrent package lookups in the backend,
	// except for the types package, which is protected separately.
	// Reusing the per-target mutex to also cover this package lookup
	// avoids a general, broader, expensive package lookup mutex.
	target.FuncSymsMu.Lock()
	sf, existed := s.Pkg.LookupOK(ir.FuncSymName(s))
	if !existed {
		target.FuncSyms = append(target.FuncSyms, n)
	}
	target.FuncSymsMu.Unlock()

	return sf.Linksym()
}

func GlobalLinksym(n *ir.Name) *obj.LSym {
	if n.Op() != ir.ONAME || n.Class != ir.PEXTERN {
		panic(fmt.Sprintf("expected global variable: %v", n))
	}
	return n.Linksym()
}

func WriteFuncSyms(target *ir.Package) {
	slices.SortFunc(target.FuncSyms, func(a, b *ir.Name) int {
		return strings.Compare(a.Linksym().Name, b.Linksym().Name)
	})
	for _, nam := range target.FuncSyms {
		s := nam.Sym()
		sf := s.Pkg.Lookup(ir.FuncSymName(s)).Linksym()

		// While compiling package runtime, we might try to create
		// funcsyms for functions from both the local package and
		// the compiler's pseudo-runtime package.
		if target.Flags.CompilingRuntime && sf.OnList() {
			continue
		}

		// Function values must always reference ABIInternal
		// entry points.
		targetLSym := s.Linksym()
		if targetLSym.ABI() != obj.ABIInternal {
			target.Diagnostics.Fatalf("expected ABIInternal: %v has %v", targetLSym, targetLSym.ABI())
		}
		// A function value of reflect.Value.Method or MethodByName may be
		// called with a method name the compiler cannot see; see usemethod.
		if s.Pkg.Path == "reflect" && (s.Name == "Value.Method" || s.Name == "Value.MethodByName") {
			sf.Set(obj.AttrReflectMethod, true)
		}
		objw.SymPtr(target.Ctxt, sf, 0, targetLSym, 0)
		objw.Global(target.Ctxt, sf, int32(target.PtrSize), obj.DUPOK|obj.RODATA)
	}
}

// InitConst writes the static literal c to n.
// Neither n nor c is modified.
func InitConst(target *ir.Package, n *ir.Name, noff int64, c ir.Node, wid int) {
	if n.Op() != ir.ONAME {
		target.Diagnostics.Fatalf("InitConst n op %v", n.Op())
	}
	if n.Sym() == nil {
		target.Diagnostics.Fatalf("InitConst nil n sym")
	}
	if c.Op() == ir.ONIL {
		return
	}
	if c.Op() != ir.OLITERAL {
		target.Diagnostics.Fatalf("InitConst c op %v", c.Op())
	}
	s := n.Linksym()
	switch u := c.Val(); u.Kind() {
	case constant.Bool:
		i := int64(obj.Bool2int(constant.BoolVal(u)))
		s.WriteInt(target.Ctxt, noff, wid, i)

	case constant.Int:
		s.WriteInt(target.Ctxt, noff, wid, ir.IntVal(c.Type(), u))

	case constant.Float:
		f, _ := constant.Float64Val(u)
		switch c.Type().Kind() {
		case types.TFLOAT32:
			s.WriteFloat32(target.Ctxt, noff, float32(f))
		case types.TFLOAT64:
			s.WriteFloat64(target.Ctxt, noff, f)
		}

	case constant.Complex:
		re, _ := constant.Float64Val(constant.Real(u))
		im, _ := constant.Float64Val(constant.Imag(u))
		switch c.Type().Kind() {
		case types.TCOMPLEX64:
			s.WriteFloat32(target.Ctxt, noff, float32(re))
			s.WriteFloat32(target.Ctxt, noff+4, float32(im))
		case types.TCOMPLEX128:
			s.WriteFloat64(target.Ctxt, noff, re)
			s.WriteFloat64(target.Ctxt, noff+8, im)
		}

	case constant.String:
		i := constant.StringVal(u)
		symdata := StringSym(target, n.Pos(), i)
		s.WriteAddr(target.Ctxt, noff, target.PtrSize, symdata, 0)
		s.WriteInt(target.Ctxt, noff+int64(target.PtrSize), target.PtrSize, int64(len(i)))

	default:
		target.Diagnostics.Fatalf("InitConst unhandled OLITERAL %v", c)
	}
}
