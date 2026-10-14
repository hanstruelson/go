// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflectdata

import (
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/rttype"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"cmd/publicinternal/src"
	"publicinternal/abi"
	"publicinternal/buildcfg"
)

// MapGroupType makes the map slot group type given the type of the map.
func MapGroupType(target *ir.Package, t *types.Type) *types.Type {
	if t.MapType().Group != nil {
		return t.MapType().Group
	}

	// Builds a type representing a group structure for the given map type.
	// This type is not visible to users, we include it so we can generate
	// a correct GC program for it.
	//
	// Make sure this stays in sync with publicinternal/runtime/maps/group.go.

	keytype := t.Key()
	elemtype := t.Elem()
	types.CalcSize(keytype)
	types.CalcSize(elemtype)
	if keytype.Size() > abi.MapMaxKeyBytes {
		keytype = types.NewPtr(keytype)
	}
	if elemtype.Size() > abi.MapMaxElemBytes {
		elemtype = types.NewPtr(elemtype)
	}

	var fields []*types.Field
	if buildcfg.Experiment.MapSplitGroup {
		// Split layout (KKKKVVVV):
		// type group struct {
		//     ctrl  uint64
		//     keys  [abi.MapGroupSlots]keyType
		//     elems [abi.MapGroupSlots]elemType
		// }
		keyArr := types.NewArray(keytype, abi.MapGroupSlots)
		keyArr.SetNoalg(true)

		elemArr := types.NewArray(elemtype, abi.MapGroupSlots)
		elemArr.SetNoalg(true)

		fields = []*types.Field{
			makefield("ctrl", target.Type(types.TUINT64)),
			makefield("keys", keyArr),
			makefield("elems", elemArr),
		}
	} else {
		// Interleaved slot layout (KVKVKVKV):
		// type group struct {
		//     ctrl  uint64
		//     slots [abi.MapGroupSlots]struct {
		//         key  keyType
		//         elem elemType
		//     }
		// }
		slotFields := []*types.Field{
			makefield("key", keytype),
			makefield("elem", elemtype),
		}
		slot := types.NewStruct(slotFields)
		slot.SetNoalg(true)

		slotArr := types.NewArray(slot, abi.MapGroupSlots)
		slotArr.SetNoalg(true)

		fields = []*types.Field{
			makefield("ctrl", target.Type(types.TUINT64)),
			makefield("slots", slotArr),
		}
	}

	group := types.NewStruct(fields)
	group.SetNoalg(true)
	types.CalcSize(group)

	// Check invariants that map code depends on.
	if !types.IsComparable(t.Key()) {
		base.Fatalf("unsupported map key type for %v", t)
	}
	if group.Size() <= 8 {
		// publicinternal/runtime/maps creates pointers to slots, even if
		// both key and elem are size zero. In this case, each slot is
		// size 0, but group should still reserve a word of padding at
		// the end to ensure pointers are valid.
		base.Fatalf("bad group size for %v", t)
	}
	if t.Key().Size() > abi.MapMaxKeyBytes && !keytype.IsPtr() {
		base.Fatalf("key indirect incorrect for %v", t)
	}
	if t.Elem().Size() > abi.MapMaxElemBytes && !elemtype.IsPtr() {
		base.Fatalf("elem indirect incorrect for %v", t)
	}

	t.MapType().Group = group
	group.StructType().Map = t
	return group
}

// mapTableType returns a type interchangeable with publicinternal/runtime/maps.table.
// Make sure this stays in sync with publicinternal/runtime/maps/table.go.
func mapTableType(target *ir.Package) *types.Type {
	if target.MapTableType != nil {
		return target.MapTableType
	}

	// type table struct {
	//     used       uint16
	//     capacity   uint16
	//     growthLeft uint16
	//     localDepth uint8
	//     // N.B Padding
	//
	//     index int
	//
	//     // From groups.
	//     groups_data       unsafe.Pointer
	//     groups_lengthMask uint64
	// }
	// must match publicinternal/runtime/maps/table.go:table.
	fields := []*types.Field{
		makefield("used", target.Type(types.TUINT16)),
		makefield("capacity", target.Type(types.TUINT16)),
		makefield("growthLeft", target.Type(types.TUINT16)),
		makefield("localDepth", target.Type(types.TUINT8)),
		makefield("index", target.Type(types.TINT)),
		makefield("groups_data", target.Type(types.TUNSAFEPTR)),
		makefield("groups_lengthMask", target.Type(types.TUINT64)),
	}

	n := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, target.InternalMapsPkg.Lookup("table"))
	table := types.NewNamed(n)
	n.SetType(table)
	n.SetTypecheck(1)

	table.SetUnderlying(types.NewStruct(fields))
	types.CalcSize(table)

	// The size of table should be 32 bytes on 64 bit
	// and 24 bytes on 32 bit platforms.
	if size := int64(3*2 + 2*1 /* one extra for padding */ + 1*8 + 2*target.PtrSize); table.Size() != size {
		base.Fatalf("publicinternal/runtime/maps.table size not correct: got %d, want %d", table.Size(), size)
	}

	target.MapTableType = table
	return table
}

// MapType returns a type interchangeable with publicinternal/runtime/maps.Map.
// Make sure this stays in sync with publicinternal/runtime/maps/map.go.
func MapType(target *ir.Package) *types.Type {
	if target.MapType != nil {
		return target.MapType
	}

	// type Map struct {
	//     used uint64
	//     seed uintptr
	//
	//     dirPtr unsafe.Pointer
	//     dirLen int
	//
	//     globalDepth uint8
	//     globalShift uint8
	//
	//     writing uint8
	//     tombstonePossible bool
	//     // N.B Padding
	//
	//     clearSeq uint64
	// }
	// must match publicinternal/runtime/maps/map.go:Map.
	fields := []*types.Field{
		makefield("used", target.Type(types.TUINT64)),
		makefield("seed", target.Type(types.TUINTPTR)),
		makefield("dirPtr", target.Type(types.TUNSAFEPTR)),
		makefield("dirLen", target.Type(types.TINT)),
		makefield("globalDepth", target.Type(types.TUINT8)),
		makefield("globalShift", target.Type(types.TUINT8)),
		makefield("writing", target.Type(types.TUINT8)),
		makefield("tombstonePossible", target.Type(types.TBOOL)),
		makefield("clearSeq", target.Type(types.TUINT64)),
	}

	n := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, target.InternalMapsPkg.Lookup("Map"))
	m := types.NewNamed(n)
	n.SetType(m)
	n.SetTypecheck(1)

	m.SetUnderlying(types.NewStruct(fields))
	types.CalcSize(m)

	// The size of Map should be 48 bytes on 64 bit
	// and 32 bytes on 32 bit platforms.
	if size := int64(2*8 + 4*target.PtrSize /* one extra for globalDepth/globalShift/writing + padding */); m.Size() != size {
		base.Fatalf("publicinternal/runtime/maps.Map size not correct: got %d, want %d", m.Size(), size)
	}

	target.MapType = m
	return m
}

// MapIterType returns a type interchangeable with publicinternal/runtime/maps.Iter.
// Make sure this stays in sync with publicinternal/runtime/maps/table.go.
func MapIterType(target *ir.Package) *types.Type {
	if target.MapIterType != nil {
		return target.MapIterType
	}

	// type Iter struct {
	//    key  unsafe.Pointer // *Key
	//    elem unsafe.Pointer // *Elem
	//    typ  unsafe.Pointer // *MapType
	//    m    *Map
	//
	//    groupSlotOffset uint64
	//    dirOffset       uint64
	//
	//    clearSeq uint64
	//
	//    globalDepth uint8
	//    // N.B. padding
	//
	//    dirIdx int
	//
	//    tab *table
	//
	//    group unsafe.Pointer // actually groupReference.data
	//
	//    entryIdx uint64
	// }
	// must match publicinternal/runtime/maps/table.go:Iter.
	fields := []*types.Field{
		makefield("key", target.Type(types.TUNSAFEPTR)),  // Used in range.go for TMAP.
		makefield("elem", target.Type(types.TUNSAFEPTR)), // Used in range.go for TMAP.
		makefield("typ", target.Type(types.TUNSAFEPTR)),
		makefield("m", types.NewPtr(MapType(target))),
		makefield("groupSlotOffset", target.Type(types.TUINT64)),
		makefield("dirOffset", target.Type(types.TUINT64)),
		makefield("clearSeq", target.Type(types.TUINT64)),
		makefield("globalDepth", target.Type(types.TUINT8)),
		makefield("dirIdx", target.Type(types.TINT)),
		makefield("tab", types.NewPtr(mapTableType(target))),
		makefield("group", target.Type(types.TUNSAFEPTR)),
		makefield("entryIdx", target.Type(types.TUINT64)),
	}

	// build iterator struct holding the above fields
	n := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, target.InternalMapsPkg.Lookup("Iter"))
	iter := types.NewNamed(n)
	n.SetType(iter)
	n.SetTypecheck(1)

	iter.SetUnderlying(types.NewStruct(fields))
	types.CalcSize(iter)

	// The size of Iter should be 96 bytes on 64 bit
	// and 64 bytes on 32 bit platforms.
	if size := 8*target.PtrSize /* one extra for globalDepth + padding */ + 4*8; iter.Size() != int64(size) {
		base.Fatalf("publicinternal/runtime/maps.Iter size not correct: got %d, want %d", iter.Size(), size)
	}

	target.MapIterType = iter
	return iter
}

func writeMapType(target *ir.Package, t *types.Type, lsym *obj.LSym, c rttype.Cursor) {
	// publicinternal/abi.MapType
	gtyp := MapGroupType(target, t)
	s1 := writeType(target, t.Key())
	s2 := writeType(target, t.Elem())
	s3 := writeType(target, gtyp)
	hasher := genhash(target, t.Key())

	var keysOff int64
	var keyStride int64
	var elemsOff int64
	var elemStride int64
	var elemOff int64
	if buildcfg.Experiment.MapSplitGroup {
		// Split layout: field 1 is keys array, field 2 is elems array.
		keysOff = gtyp.Field(1).Offset
		keyStride = gtyp.Field(1).Type.Elem().Size()
		elemsOff = gtyp.Field(2).Offset
		elemStride = gtyp.Field(2).Type.Elem().Size()
	} else {
		// Interleaved layout: field 1 is slots array.
		// KeysOff = offset of slots array (first key).
		// KeyStride = ElemStride = slot stride.
		// ElemsOff = offset of slots + offset of elem within slot.
		keysOff = gtyp.Field(1).Offset
		slotTyp := gtyp.Field(1).Type.Elem()
		slotSize := slotTyp.Size()
		elemOffInSlot := slotTyp.Field(1).Offset
		keyStride = slotSize
		elemsOff = keysOff + elemOffInSlot
		elemStride = slotSize
		elemOff = slotTyp.Field(1).Offset
	}

	c.Field("Key").WritePtr(s1)
	c.Field("Elem").WritePtr(s2)
	c.Field("Group").WritePtr(s3)
	c.Field("Hasher").WritePtr(hasher)
	c.Field("GroupSize").WriteUintptr(uint64(gtyp.Size()))
	c.Field("KeysOff").WriteUintptr(uint64(keysOff))
	c.Field("KeyStride").WriteUintptr(uint64(keyStride))
	c.Field("ElemsOff").WriteUintptr(uint64(elemsOff))
	c.Field("ElemStride").WriteUintptr(uint64(elemStride))
	c.Field("ElemOff").WriteUintptr(uint64(elemOff))
	var flags uint32
	if needkeyupdate(t.Key()) {
		flags |= abi.MapNeedKeyUpdate
	}
	if hashMightPanic(t.Key()) {
		flags |= abi.MapHashMightPanic
	}
	if t.Key().Size() > abi.MapMaxKeyBytes {
		flags |= abi.MapIndirectKey
	}
	if t.Elem().Size() > abi.MapMaxElemBytes {
		flags |= abi.MapIndirectElem
	}
	c.Field("Flags").WriteUint32(flags)

	if u := t.Underlying(); u != t {
		// If t is a named map type, also keep the underlying map
		// type live in the binary. This is important to make sure that
		// a named map and that same map cast to its underlying type via
		// reflection, use the same hash function. See issue 37716.
		lsym.AddRel(target.Ctxt, obj.Reloc{Type: objabi.R_KEEP, Sym: writeType(target, u)})
	}
}
