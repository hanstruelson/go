// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"publicinternal/abi"
	"publicinternal/goarch"
	"publicinternal/runtime/atomic"
	"unsafe"
)

var jitOwnerSlotSpanCount int32

type jitPinnedTypeRoot struct {
	typ      *_type
	metadata *jitHeapObjectMetadata
	next     *jitPinnedTypeRoot
}

// jitHeapObjectMetadata is reached through a pointer-free span side table.
// The GC shades this ordinary heap object only when the associated language
// object is live, then scans its fields normally. It carries the descriptor,
// all module owners needed by a reflected type, and that type's local
// reflection-offset targets.
type jitHeapObjectMetadata struct {
	typ      *_type
	identity uint64
	owners   []*JITModuleOwner
	offsets  []JITReflectOffset
}

func (m *jitHeapObjectMetadata) addOwner(owner *JITModuleOwner) {
	if owner == nil {
		return
	}
	for _, existing := range m.owners {
		if existing == owner {
			return
		}
	}
	m.owners = append(m.owners, owner)
}

var (
	jitReflectTypeIdentity uint64
	jitPinnedTypeRootLock  mutex
	jitPinnedTypeRoots     *jitPinnedTypeRoot
)

// jitOwnerForType finds the owner of a type descriptor in a JIT mapping.
// The weak index lookup promotes the owner before returning it, so the
// allocation path can safely attach that owner to the resulting heap object.
func jitOwnerForType(typ *_type) (*JITModuleOwner, bool) {
	if typ == nil {
		return nil, false
	}
	owner, found := jitOwnerForMappedAddress(uintptr(unsafe.Pointer(typ)))
	if found && owner == nil {
		throw("runtime: allocating an object with a retired JIT type")
	}
	dependent := found || typ.TFlag&abi.TFlagJITDependent != 0
	if owner == nil && dependent {
		owner = jitOwnerFromTypeEdge(typ)
	}
	return owner, dependent
}

// jitOwnersForType returns the complete owner set recorded for a
// reflect-created descriptor, or the direct owner of an image descriptor.
// Structural reflection caches may combine dependencies from several modules
// and must keep the complete set live while publishing a canonical descriptor.
func jitOwnersForType(typ *_type) ([]*JITModuleOwner, bool) {
	if typ == nil {
		return nil, false
	}
	address := uintptr(unsafe.Pointer(typ))
	owner, found := jitOwnerForMappedAddress(address)
	if found && owner == nil {
		throw("runtime: JIT-dependent type has no live owner")
	}
	dependent := found || typ.TFlag&abi.TFlagJITDependent != 0
	if owner != nil {
		return []*JITModuleOwner{owner}, true
	}
	if !dependent {
		return nil, false
	}
	if metadata := jitMetadataForHeapObject(address); metadata != nil {
		if len(metadata.owners) != 0 {
			KeepAlive(metadata)
			return metadata.owners, true
		}
		KeepAlive(metadata)
		return nil, true
	}
	if owner := jitOwnerFromTypeEdge(typ); owner != nil {
		return []*JITModuleOwner{owner}, true
	}
	return nil, true
}

// jitOwnerFromTypeEdge resolves a reflect-created descriptor's non-GC side
// edge to one of its originating module owners. Dependencies form a directed
// acyclic chain because reflect constructors only refer to existing types.
func jitOwnerFromTypeEdge(typ *_type) *JITModuleOwner {
	address := uintptr(unsafe.Pointer(typ))
	for depth := 0; depth < 128; depth++ {
		if owner, found := jitOwnerForMappedAddress(address); found {
			if owner == nil {
				throw("runtime: JIT-dependent type has no live owner")
			}
			return owner
		}
		_, span, index := findObject(address, 0, 0)
		if span == nil || index >= uintptr(span.nelems) {
			return nil
		}
		slots := (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
		if slots == nil {
			return nil
		}
		edge := atomic.Loaduintptr((*uintptr)(unsafe.Add(unsafe.Pointer(slots), index*goarch.PtrSize)))
		if edge == 0 {
			return nil
		}
		switch edge & 3 {
		case 0:
			return (*JITModuleOwner)(unsafe.Pointer(edge))
		case 1:
			address = edge &^ 3
		case 2:
			metadata := (*jitHeapObjectMetadata)(unsafe.Pointer(edge &^ 3))
			if len(metadata.owners) != 0 {
				return metadata.owners[0]
			}
			return nil
		default:
			throw("runtime: invalid JIT object owner edge")
		}
	}
	return nil
}

// jitTypeNeedsOwner reports whether typ itself or a reflect-created type
// reachable from it depends on a reclaimable JIT module.
func jitTypeNeedsOwner(typ *_type) bool {
	if typ == nil {
		return false
	}
	return typ.TFlag&abi.TFlagJITDependent != 0 || jitAddressIsMapped(uintptr(unsafe.Pointer(typ)))
}

// jitAddReflectTypeOwners copies every source owner reachable from a type into
// metadata for a newly reflected descriptor. A reflected type can combine
// types from multiple independently compiled modules, so retaining only the
// first owner would let another dependency unload while the descriptor still
// contains its raw type or method PCs.
func jitAddReflectTypeOwners(metadata *jitHeapObjectMetadata, typ *_type) bool {
	if typ == nil {
		return false
	}
	address := uintptr(unsafe.Pointer(typ))
	if owner, found := jitOwnerForMappedAddress(address); found {
		if owner == nil {
			throw("runtime: JIT-dependent type has no live owner")
		}
		metadata.addOwner(owner)
		KeepAlive(owner)
		return false
	}
	if dependency := jitMetadataForHeapObject(address); dependency != nil {
		if len(dependency.owners) == 0 {
			return true
		}
		for _, owner := range dependency.owners {
			metadata.addOwner(owner)
		}
		KeepAlive(dependency)
		return false
	}
	if typ.TFlag&abi.TFlagJITDependent != 0 {
		if owner, _ := jitOwnerForType(typ); owner != nil {
			metadata.addOwner(owner)
			return false
		}
		return true
	}
	return false
}

// jitAssociateHeapObject records a traced lifetime edge from one heap object
// to owner without changing the object's language-visible layout. The owner
// address is stored in non-GC span metadata and is shaded whenever the object
// is marked. A span slot is cleared when a non-JIT allocation reuses it.
func jitAssociateHeapObject(x unsafe.Pointer, owner *JITModuleOwner) bool {
	if x == nil || owner == nil {
		return true
	}
	return jitSetHeapOwnerEdge(x, uintptr(unsafe.Pointer(owner)))
}

// jitAssociateHeapTypeObject records a descriptor edge for an allocation
// whose reflect-created type depends on a JIT type. The descriptor itself is
// a Go heap object; tracing it keeps its descriptor storage and all referenced
// JIT types alive. Tag value one distinguishes this form from a direct owner
// pointer in the low alignment bits of the side-table word.
func jitAssociateHeapTypeObject(x unsafe.Pointer, typ *_type) bool {
	if x == nil || typ == nil {
		return true
	}
	address := uintptr(unsafe.Pointer(typ))
	if address&3 != 0 {
		throw("runtime: unaligned JIT-dependent type descriptor")
	}
	if !jitAddressIsMapped(address) {
		_, span, _ := findObject(address, 0, 0)
		if span == nil {
			throw("runtime: JIT-dependent type descriptor is not heap allocated")
		}
	}
	if !jitSetHeapOwnerEdge(x, address|1) {
		// Without the side-table edge, the Go allocation could outlive its
		// reflect-generated descriptor. Keep that descriptor explicitly rooted
		// as the fail-closed path for system-memory or budget exhaustion.
		jitPinTypeDescriptor(typ)
		return false
	}
	return true
}

// jitAssociateHeapMetadataObject attaches a typed metadata object to a heap
// object without changing its language-visible layout.
func jitAssociateHeapMetadataObject(x unsafe.Pointer, metadata *jitHeapObjectMetadata) bool {
	if x == nil || metadata == nil {
		return true
	}
	address := uintptr(unsafe.Pointer(metadata))
	if address&3 != 0 {
		throw("runtime: unaligned JIT metadata")
	}
	if _, span, _ := findObject(address, 0, 0); span == nil {
		throw("runtime: JIT reflect metadata is not heap allocated")
	}
	if _, span, _ := findObject(uintptr(x), 0, 0); span == nil {
		throw("runtime: JIT metadata target is not heap allocated")
	}
	return jitSetHeapOwnerEdge(x, address|2)
}

func jitPinTypeDescriptor(typ *_type) {
	if typ == nil {
		return
	}
	root := new(jitPinnedTypeRoot)
	root.typ = typ
	lock(&jitPinnedTypeRootLock)
	root.next = jitPinnedTypeRoots
	jitPinnedTypeRoots = root
	unlock(&jitPinnedTypeRootLock)
	KeepAlive(typ)
}

func jitPinReflectMetadata(metadata *jitHeapObjectMetadata) {
	if metadata == nil {
		return
	}
	root := new(jitPinnedTypeRoot)
	root.typ = metadata.typ
	root.metadata = metadata
	for _, owner := range metadata.owners {
		PinJITModule(owner)
	}
	lock(&jitPinnedTypeRootLock)
	root.next = jitPinnedTypeRoots
	jitPinnedTypeRoots = root
	unlock(&jitPinnedTypeRootLock)
	KeepAlive(metadata)
}

func jitPinOwnerEdge(edge uintptr) {
	switch edge & 3 {
	case 0:
		PinJITModule((*JITModuleOwner)(unsafe.Pointer(edge)))
	case 1:
		jitPinTypeOwners(typFromJITOwnerEdge(edge))
	case 2:
		metadata := (*jitHeapObjectMetadata)(unsafe.Pointer(edge &^ 3))
		if len(metadata.owners) == 0 {
			pinAllJITModules()
			return
		}
		for _, owner := range metadata.owners {
			PinJITModule(owner)
		}
	default:
		throw("runtime: invalid JIT object owner edge")
	}
}

func jitPinTypeOwners(typ *_type) {
	if typ == nil {
		return
	}
	if owner, found := jitOwnerForMappedAddress(uintptr(unsafe.Pointer(typ))); found {
		if owner == nil {
			throw("runtime: cannot pin a retired JIT type")
		}
		PinJITModule(owner)
		return
	}
	if metadata := jitMetadataForHeapObject(uintptr(unsafe.Pointer(typ))); metadata != nil {
		if len(metadata.owners) == 0 {
			pinAllJITModules()
			return
		}
		for _, owner := range metadata.owners {
			PinJITModule(owner)
		}
		KeepAlive(metadata)
		return
	}
	if owner, _ := jitOwnerForType(typ); owner != nil {
		PinJITModule(owner)
		return
	}
	pinAllJITModules()
}

// JITPinAddress permanently retains the JIT module associated with a raw
// address. Reflection uses this before returning uintptr or unsafe.Pointer
// views, which cannot carry an owner edge by themselves. Ordinary addresses
// have no effect.
func JITPinAddress(address uintptr) bool {
	if address == 0 {
		return false
	}
	if owner, found := jitOwnerForMappedAddress(address); found {
		if owner == nil {
			throw("runtime: cannot pin a retired JIT address")
		}
		PinJITModule(owner)
		KeepAlive(owner)
		return true
	}
	_, span, index := findObject(address, 0, 0)
	if span == nil || index >= uintptr(span.nelems) {
		return false
	}
	slots := (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
	if slots == nil {
		return false
	}
	edge := atomic.Loaduintptr((*uintptr)(unsafe.Add(unsafe.Pointer(slots), index*goarch.PtrSize)))
	if edge == 0 {
		return false
	}
	jitPinOwnerEdge(edge)
	return true
}

// jitOwnerForFuncval returns a strong owner for a function value whose code
// resides in a JIT mapping. Runtime records use this while moving a callback
// from a non-GC queue onto a goroutine stack; GC root scanners use
// jitMarkFuncvalOwner instead.
func jitOwnerForFuncval(fn *funcval) *JITModuleOwner {
	if fn == nil {
		return nil
	}
	owner, found := jitOwnerForMappedAddress(fn.fn)
	if found && owner == nil {
		throw("runtime: JIT function value has no live owner")
	}
	return owner
}

// jitMarkFuncvalOwner follows the raw code PC in a runtime-owned function
// value. The funcval itself is a normal GC pointer, but its first word is a
// uintptr and does not keep executable text alive.
//
//go:nowritebarrier
//go:nosplit
func jitMarkFuncvalOwner(fn *funcval, gcw *gcWork) {
	if fn != nil {
		jitMarkOwnerAddr(fn.fn, gcw)
	}
}

// jitMarkTypeOwner follows a type descriptor held in runtime-owned metadata.
// Descriptor pointers are either direct addresses in a JIT image or pointers
// to reflected heap objects whose side-table edge carries the source owners.
// Ordinary static descriptors take the fast no-op path.
//
//go:nowritebarrier
//go:nosplit
func jitMarkTypeOwner(typ *_type, gcw *gcWork) {
	if typ == nil {
		return
	}
	address := uintptr(unsafe.Pointer(typ))
	if jitMarkOwnerAddr(address, gcw) {
		return
	}
	if obj, span, index := findObject(address, 0, 0); obj != 0 {
		jitMarkSpanOwner(span, index, gcw)
	}
}

// jitMarkCleanupOwners follows both code addresses stored in a runtime cleanup
// record. The closure and argument pointers are scanned by the ordinary
// cleanup root mask; these function values' code words are uintptrs.
//
//go:nowritebarrier
func jitMarkCleanupOwners(cleanup *cleanupFn, gcw *gcWork) {
	if cleanup == nil {
		return
	}
	call := *(**funcval)(unsafe.Pointer(&cleanup.call))
	jitMarkFuncvalOwner(call, gcw)
	jitMarkFuncvalOwner(cleanup.fn, gcw)
}

// jitSetHeapOwnerEdge stores a tagged owner or descriptor address in the
// non-GC span side table.
func jitSetHeapOwnerEdge(x unsafe.Pointer, edge uintptr) bool {
	if edge == 0 {
		return true
	}
	obj, span, index := findObject(uintptr(x), 0, 0)
	if obj == 0 || span == nil || index >= uintptr(span.nelems) {
		jitPinOwnerEdge(edge)
		return false
	}

	slots := (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
	if slots == nil {
		size := uintptr(span.nelems) * goarch.PtrSize
		if size == 0 || size/goarch.PtrSize != uintptr(span.nelems) || reserveJITMemory(size) != nil {
			jitPinOwnerEdge(edge)
			return false
		}
		candidate := sysAlloc(size, &memstats.other_sys, "jit object owner slots")
		if candidate == nil {
			releaseJITMemory(size)
			jitPinOwnerEdge(edge)
			return false
		}
		memclrNoHeapPointers(candidate, size)
		if atomic.Casp1((*unsafe.Pointer)(unsafe.Pointer(&span.jitOwnerSlots)), nil, candidate) {
			slots = (*unsafe.Pointer)(candidate)
			atomic.Xaddint32(&jitOwnerSlotSpanCount, 1)
		} else {
			sysFree(candidate, size, &memstats.other_sys)
			releaseJITMemory(size)
			slots = (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
		}
	}
	if slots == nil {
		jitPinOwnerEdge(edge)
		return false
	}
	atomic.Storeuintptr((*uintptr)(unsafe.Add(unsafe.Pointer(slots), index*goarch.PtrSize)), edge)
	if writeBarrier.enabled {
		jitMarkSpanOwner(span, index, &getg().m.p.ptr().gcw)
	}
	switch edge & 3 {
	case 0:
		KeepAlive((*JITModuleOwner)(unsafe.Pointer(edge)))
	case 1:
		KeepAlive(typFromJITOwnerEdge(edge))
	case 2:
		KeepAlive((*jitHeapObjectMetadata)(unsafe.Pointer(edge &^ 3)))
	default:
		throw("runtime: invalid JIT object owner edge")
	}
	return true
}

func typFromJITOwnerEdge(edge uintptr) *_type {
	return (*_type)(unsafe.Pointer(edge &^ 3))
}

// jitMetadataForHeapObject returns the reflected-type metadata edge associated
// with address, if present. The side table is unscanned; callers use the
// returned metadata only while the containing object is live on their stack.
func jitMetadataForHeapObject(address uintptr) *jitHeapObjectMetadata {
	_, span, index := findObject(address, 0, 0)
	if span == nil || index >= uintptr(span.nelems) {
		return nil
	}
	slots := (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
	if slots == nil {
		return nil
	}
	edge := atomic.Loaduintptr((*uintptr)(unsafe.Add(unsafe.Pointer(slots), index*goarch.PtrSize)))
	if edge&3 != 2 {
		return nil
	}
	return (*jitHeapObjectMetadata)(unsafe.Pointer(edge &^ 3))
}

// jitReflectOffsetForType resolves a local reflection offset from a
// reflect-created descriptor. Local IDs are negative values starting at -2;
// -1 remains reserved for the runtime's unreachable sentinel.
func jitReflectOffsetForType(base uintptr, off int32, kind uint8) (unsafe.Pointer, bool) {
	if off >= -1 {
		return nil, false
	}
	metadata := jitMetadataForHeapObject(base)
	if metadata == nil {
		return nil, false
	}
	index := uintptr(-int64(off) - 2)
	if index >= uintptr(len(metadata.offsets)) {
		return nil, false
	}
	entry := metadata.offsets[index]
	if entry.Kind != kind || entry.Target == nil {
		return nil, false
	}
	return entry.Target, true
}

func jitFinishHeapAllocation(x unsafe.Pointer, owner *JITModuleOwner, typ *_type) unsafe.Pointer {
	if typ != nil && typ.TFlag&abi.TFlagJITDependent != 0 {
		jitAssociateHeapTypeObject(x, typ)
	} else if owner != nil {
		jitAssociateHeapObject(x, owner)
	} else {
		jitClearHeapObjectOwner(x)
	}
	return x
}

// jitClearHeapObjectOwner removes a stale owner slot when a span slot is
// reused by an ordinary allocation. It does not allocate and is safe on the
// allocator's non-preemptible path.
func jitClearHeapObjectOwner(x unsafe.Pointer) {
	if x == nil || atomic.Loadint32(&jitOwnerSlotSpanCount) == 0 {
		return
	}
	obj, span, index := findObject(uintptr(x), 0, 0)
	if obj == 0 || span == nil || index >= uintptr(span.nelems) {
		return
	}
	slots := (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
	if slots != nil {
		atomic.Storeuintptr((*uintptr)(unsafe.Add(unsafe.Pointer(slots), index*goarch.PtrSize)), 0)
	}
}

// jitFreeSpanOwnerSlots releases the pointer-free auxiliary owner table when
// a whole span returns to the heap. The caller holds mheap_.lock, and a span
// is returned only after its allocations have completed the current sweep.
func jitFreeSpanOwnerSlots(span *mspan) {
	if span.jitOwnerSlots == nil {
		return
	}
	slots := span.jitOwnerSlots
	atomic.StorepNoWB(unsafe.Pointer(&span.jitOwnerSlots), nil)
	if atomic.Xaddint32(&jitOwnerSlotSpanCount, -1) < 0 {
		throw("runtime: JIT owner slot span count underflow")
	}
	size := uintptr(span.nelems) * goarch.PtrSize
	if size == 0 || size/goarch.PtrSize != uintptr(span.nelems) {
		throw("runtime: invalid JIT owner slot allocation size")
	}
	sysFree(slots, size, &memstats.other_sys)
	releaseJITMemory(size)
}

// jitMarkSpanOwner shades the owner or typed metadata attached to one marked
// heap allocation. The side table is deliberately not a GC root: only a live
// object can create the edge, so an unreachable allocation cannot keep its
// module alive.
//
//go:nowritebarrier
//go:nosplit
func jitMarkSpanOwner(span *mspan, index uintptr, gcw *gcWork) {
	if span == nil || index >= uintptr(span.nelems) {
		return
	}
	slots := (*unsafe.Pointer)(atomic.Loadp(unsafe.Pointer(&span.jitOwnerSlots)))
	if slots == nil {
		return
	}
	ownerAddr := atomic.Loaduintptr((*uintptr)(unsafe.Add(unsafe.Pointer(slots), index*goarch.PtrSize)))
	if ownerAddr == 0 {
		return
	}
	switch ownerAddr & 3 {
	case 1:
		address := ownerAddr &^ 3
		if !tryDeferToSpanScan(address, gcw) {
			if obj, ownerSpan, objIndex := findObject(address, 0, 0); obj != 0 {
				greyobject(obj, 0, 0, ownerSpan, gcw, objIndex)
			} else {
				jitMarkOwnerAddr(address, gcw)
			}
		}
	case 2:
		address := ownerAddr &^ 3
		if !tryDeferToSpanScan(address, gcw) {
			if obj, metadataSpan, objIndex := findObject(address, 0, 0); obj != 0 {
				greyobject(obj, 0, 0, metadataSpan, gcw, objIndex)
			} else {
				throw("runtime: JIT reflect metadata edge is not a heap object")
			}
		}
	case 0:
		jitMarkOwnerObject((*JITModuleOwner)(unsafe.Pointer(ownerAddr)), gcw)
	default:
		throw("runtime: invalid JIT object owner edge")
	}
}

// jitMarkOwnerObject marks a JIT owner from a pointer held in runtime-owned
// metadata. It mirrors the ordinary pointer path without treating the address
// as a GC root by itself.
//
//go:nowritebarrier
//go:nosplit
func jitMarkOwnerObject(owner *JITModuleOwner, gcw *gcWork) {
	if owner == nil {
		return
	}
	ownerAddr := uintptr(unsafe.Pointer(owner))
	if !tryDeferToSpanScan(ownerAddr, gcw) {
		if obj, span, objIndex := findObject(ownerAddr, 0, 0); obj != 0 {
			greyobject(obj, 0, 0, span, gcw, objIndex)
		}
	}
}
