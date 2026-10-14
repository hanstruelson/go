// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package objw

import (
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/bitvec"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
	"encoding/binary"
)

// Uint8 writes an unsigned byte v into s at offset off,
// and returns the next unused offset (i.e., off+1).
func Uint8(ctxt *obj.Link, s *obj.LSym, off int, v uint8) int {
	return UintN(ctxt, s, off, uint64(v), 1)
}

func Uint16(ctxt *obj.Link, s *obj.LSym, off int, v uint16) int {
	return UintN(ctxt, s, off, uint64(v), 2)
}

func Uint32(ctxt *obj.Link, s *obj.LSym, off int, v uint32) int {
	return UintN(ctxt, s, off, uint64(v), 4)
}

func Uintptr(ctxt *obj.Link, s *obj.LSym, off int, v uint64) int {
	return UintN(ctxt, s, off, v, ctxt.Arch.PtrSize)
}

// Uvarint writes a varint v into s at offset off,
// and returns the next unused offset.
func Uvarint(ctxt *obj.Link, s *obj.LSym, off int, v uint64) int {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return int(s.WriteBytes(ctxt, int64(off), buf[:n]))
}

func Bool(ctxt *obj.Link, s *obj.LSym, off int, v bool) int {
	w := 0
	if v {
		w = 1
	}
	return UintN(ctxt, s, off, uint64(w), 1)
}

// UintN writes an unsigned integer v of size wid bytes into s at offset off,
// and returns the next unused offset.
func UintN(ctxt *obj.Link, s *obj.LSym, off int, v uint64, wid int) int {
	if off&(wid-1) != 0 {
		base.Fatalf("duintxxLSym: misaligned: v=%d wid=%d off=%d", v, wid, off)
	}
	s.WriteInt(ctxt, int64(off), wid, int64(v))
	return off + wid
}

func SymPtr(ctxt *obj.Link, s *obj.LSym, off int, x *obj.LSym, xoff int) int {
	off = int(types.RoundUp(int64(off), int64(ctxt.Arch.PtrSize)))
	s.WriteAddr(ctxt, int64(off), ctxt.Arch.PtrSize, x, int64(xoff))
	off += ctxt.Arch.PtrSize
	return off
}

func SymPtrWeak(ctxt *obj.Link, s *obj.LSym, off int, x *obj.LSym, xoff int) int {
	off = int(types.RoundUp(int64(off), int64(ctxt.Arch.PtrSize)))
	s.WriteWeakAddr(ctxt, int64(off), ctxt.Arch.PtrSize, x, int64(xoff))
	off += ctxt.Arch.PtrSize
	return off
}

func SymPtrOff(ctxt *obj.Link, s *obj.LSym, off int, x *obj.LSym) int {
	s.WriteOff(ctxt, int64(off), x, 0)
	off += 4
	return off
}

func SymPtrWeakOff(ctxt *obj.Link, s *obj.LSym, off int, x *obj.LSym) int {
	s.WriteWeakOff(ctxt, int64(off), x, 0)
	off += 4
	return off
}

func Global(ctxt *obj.Link, s *obj.LSym, width int32, flags int16) {
	if flags&obj.LOCAL != 0 {
		s.Set(obj.AttrLocal, true)
		flags &^= obj.LOCAL
	}
	ctxt.Globl(s, int64(width), int(flags))
}

// BitVec writes the contents of bv into s as sequence of bytes
// in little-endian order, and returns the next unused offset.
func BitVec(ctxt *obj.Link, s *obj.LSym, off int, bv bitvec.BitVec) int {
	// Runtime reads the bitmaps as byte arrays. Oblige.
	for j := 0; int32(j) < bv.N; j += 8 {
		word := bv.B[j/32]
		off = Uint8(ctxt, s, off, uint8(word>>(uint(j)%32)))
	}
	return off
}
