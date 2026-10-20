// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typebits

import (
	"cmd/compile/publicinternal/bitvec"
	"cmd/compile/publicinternal/types"
	"fmt"
)

// NOTE: The bitmap for a specific type t could be cached in t after
// the first run and then simply copied into bv at the correct offset
// on future calls with the same type t.
func Set(ptrSize int, t *types.Type, off int64, bv bitvec.BitVec) {
	set(ptrSize, t, off, bv, false)
}

// SetNoCheck is like Set, but do not check for alignment.
func SetNoCheck(ptrSize int, t *types.Type, off int64, bv bitvec.BitVec) {
	set(ptrSize, t, off, bv, true)
}

func set(ptrSize int, t *types.Type, off int64, bv bitvec.BitVec, skip bool) {
	if !skip && uint8(t.Alignment()) > 0 && off&int64(uint8(t.Alignment())-1) != 0 {
		panic(fmt.Sprintf("typebits.Set: invalid initial alignment: type %v has alignment %d, but offset is %v", t, uint8(t.Alignment()), off))
	}
	if !t.HasPointers() {
		// Note: this case ensures that pointers to not-in-heap types
		// are not considered pointers by garbage collection and stack copying.
		return
	}

	switch t.Kind() {
	case types.TPTR, types.TUNSAFEPTR, types.TFUNC, types.TCHAN, types.TMAP:
		if off&int64(ptrSize-1) != 0 {
			panic(fmt.Sprintf("typebits.Set: invalid alignment, %v", t))
		}
		bv.Set(int32(off / int64(ptrSize))) // pointer

	case types.TSTRING:
		// struct { byte *str; intgo len; }
		if off&int64(ptrSize-1) != 0 {
			panic(fmt.Sprintf("typebits.Set: invalid alignment, %v", t))
		}
		bv.Set(int32(off / int64(ptrSize))) //pointer in first slot

	case types.TINTER:
		// struct { Itab *tab;	void *data; }
		// or, when isnilinter(t)==true:
		// struct { Type *type; void *data; }
		if off&int64(ptrSize-1) != 0 {
			panic(fmt.Sprintf("typebits.Set: invalid alignment, %v", t))
		}
		// Trace both words. Static itabs and type descriptors are outside
		// the Go heap and are ignored by the heap scanner. JIT itabs and type
		// descriptors can belong to reclaimable modules, so their interface
		// word must retain the module owner through the ordinary GC path.
		bv.Set(int32(off/int64(ptrSize) + 1)) // pointer in second slot
		bv.Set(int32(off / int64(ptrSize)))   // pointer to itab or type in first slot

	case types.TSLICE:
		// struct { byte *array; uintgo len; uintgo cap; }
		if off&int64(ptrSize-1) != 0 {
			panic(fmt.Sprintf("typebits.Set: invalid TARRAY alignment, %v", t))
		}
		bv.Set(int32(off / int64(ptrSize))) // pointer in first slot (BitsPointer)

	case types.TARRAY:
		elt := t.Elem()
		if elt.Size() == 0 {
			// Short-circuit for #20739.
			break
		}
		for i := int64(0); i < t.NumElem(); i++ {
			set(ptrSize, elt, off, bv, skip)
			off += elt.Size()
		}

	case types.TSTRUCT:
		for _, f := range t.Fields() {
			set(ptrSize, f.Type, off+f.Offset, bv, skip)
		}

	default:
		panic(fmt.Sprintf("typebits.Set: unexpected type, %v", t))
	}
}
