// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"publicinternal/runtime/atomic"
	"unsafe"
)

// jitModuleIndex is a pointer-free immutable snapshot. It is allocated outside
// the Go heap so the index does not keep module owners alive. Owner entries use
// runtime weak handles; lookup paths that retain a PC convert them to strong
// pointers through the GC's weak-to-strong operation. Readers hold a grace
// period while consulting module metadata; writers publish a replacement and
// wait for readers before freeing the old snapshot.
type jitModuleIndex struct {
	count         uintptr
	addressOffset uintptr
	gofuncOffset  uintptr
}

type jitModuleIndexEntry struct {
	minpc, maxpc uintptr
	mapStart     uintptr
	mapEnd       uintptr
	pclnStart    uintptr
	pclnEnd      uintptr
	gofuncStart  uintptr
	gofuncEnd    uintptr
	generation   uint64
	module       uintptr
	owner        uintptr
	weakHandle   uintptr
}

type jitModuleAddressEntry struct {
	start, end uintptr
	generation uint64
	module     uintptr
	owner      uintptr
	weakHandle uintptr
}

var (
	jitModuleIndexPtr uintptr
	jitIndexEpoch     uint32
	jitIndexReaders   [2]int32
	jitIndexLock      mutex
	// jitMappingMin and jitMappingMax form a coarse, monotonically widened
	// address interval. GC pointer scanning sees many non-heap pointers; this
	// lets it reject the common case without acquiring an index reader lease.
	jitMappingMin     uintptr
	jitMappingMax     uintptr
	jitOwnerRootLock  mutex
	jitOwnerPinSlots  []*jitOwnerRootSlot
	jitOwnerWeakRoots []unsafe.Pointer
	jitRawPCPins      int32
)

// Pin slots are allocated during registration so runtime profiler paths can
// retain an owner without allocating.
type jitOwnerRootSlot struct {
	owner *JITModuleOwner
}

//go:nosplit
func jitIndexReadEnter() uintptr {
	for {
		epoch := atomic.Load(&jitIndexEpoch) & 1
		readers := &jitIndexReaders[epoch]
		for {
			count := atomic.Loadint32(readers)
			if count < 0 {
				throw("runtime: negative JIT index reader count")
			}
			if count == int32(^uint32(0)>>1) {
				throw("runtime: JIT index reader count exhausted")
			}
			if atomic.Casint32(readers, count, count+1) {
				break
			}
		}
		if atomic.Load(&jitIndexEpoch)&1 != epoch {
			if atomic.Xaddint32(readers, -1) < 0 {
				throw("runtime: JIT index reader count underflow")
			}
			continue
		}
		index := atomic.Loaduintptr(&jitModuleIndexPtr)
		if index&3 != 0 {
			throw("runtime: unaligned JIT module index")
		}
		// The low two bits encode the epoch and a lease marker. Module index
		// snapshots are page-aligned, including the nil snapshot.
		return index | uintptr(epoch<<1) | 1
	}
}

//go:nosplit
func jitIndexPointer(lease uintptr) uintptr {
	return lease &^ 3
}

//go:nosplit
func jitIndexReadLeave(lease uintptr) {
	if lease == 0 || lease&1 == 0 {
		throw("runtime: invalid JIT index reader lease")
	}
	epoch := (lease >> 1) & 1
	if atomic.Xaddint32(&jitIndexReaders[epoch], -1) < 0 {
		throw("runtime: JIT index reader count underflow")
	}
}

// jitPublishIndex publishes an immutable snapshot and starts a new reader
// epoch. The caller serializes writers with jitIndexLock, then waits for the
// returned old epoch before freeing the previous snapshot.
func jitPublishIndex(index uintptr) uint32 {
	if index&3 != 0 {
		throw("runtime: unaligned JIT module index publication")
	}
	oldEpoch := atomic.Load(&jitIndexEpoch) & 1
	atomic.Storeuintptr(&jitModuleIndexPtr, index)
	atomic.Store(&jitIndexEpoch, oldEpoch^1)
	return oldEpoch
}

func jitWaitIndexEpoch(epoch uint32) {
	for atomic.Loadint32(&jitIndexReaders[epoch&1]) != 0 {
		osyield()
	}
}

//go:nosplit
func jitIndexEntries(h *jitModuleIndex) []jitModuleIndexEntry {
	if h.count == 0 {
		return nil
	}
	ptr := unsafe.Pointer(uintptr(unsafe.Pointer(h)) + unsafe.Sizeof(*h))
	return unsafe.Slice((*jitModuleIndexEntry)(ptr), int(h.count))
}

//go:nosplit
func jitIndexAddressEntries(h *jitModuleIndex) []jitModuleAddressEntry {
	if h.count == 0 {
		return nil
	}
	ptr := unsafe.Pointer(uintptr(unsafe.Pointer(h)) + h.addressOffset)
	return unsafe.Slice((*jitModuleAddressEntry)(ptr), int(h.count))
}

//go:nosplit
func jitIndexGofuncEntries(h *jitModuleIndex) []jitModuleAddressEntry {
	if h.count == 0 {
		return nil
	}
	ptr := unsafe.Pointer(uintptr(unsafe.Pointer(h)) + h.gofuncOffset)
	return unsafe.Slice((*jitModuleAddressEntry)(ptr), int(h.count))
}

func jitModuleIndexSize(count uintptr) uintptr {
	return unsafe.Sizeof(jitModuleIndex{}) + count*unsafe.Sizeof(jitModuleIndexEntry{}) +
		2*count*unsafe.Sizeof(jitModuleAddressEntry{})
}

// jitExpandMappingBounds publishes conservative bounds before a module index
// can expose the new mapping. Bounds never shrink, so a stale value can only
// cause an extra index lookup; it cannot hide a currently published mapping.
//
//go:nosplit
func jitExpandMappingBounds(start, end uintptr) {
	if start == 0 || end <= start {
		throw("runtime: invalid JIT mapping bounds")
	}
	for old := atomic.Loaduintptr(&jitMappingMin); old == 0 || start < old; old = atomic.Loaduintptr(&jitMappingMin) {
		if atomic.Casuintptr(&jitMappingMin, old, start) {
			break
		}
	}
	for old := atomic.Loaduintptr(&jitMappingMax); end > old; old = atomic.Loaduintptr(&jitMappingMax) {
		if atomic.Casuintptr(&jitMappingMax, old, end) {
			break
		}
	}
}

// jitSortPCEntries sorts a module index by entry PC. Heap sort keeps module
// publication O(n log n) without allocating a temporary slice in the runtime.
func jitSortPCEntries(entries []jitModuleIndexEntry) {
	sift := func(root, end int) {
		for {
			child := root*2 + 1
			if child >= end {
				return
			}
			if child+1 < end && entries[child].minpc < entries[child+1].minpc {
				child++
			}
			if entries[root].minpc >= entries[child].minpc {
				return
			}
			entries[root], entries[child] = entries[child], entries[root]
			root = child
		}
	}
	for root := len(entries)/2 - 1; root >= 0; root-- {
		sift(root, len(entries))
	}
	for end := len(entries) - 1; end > 0; end-- {
		entries[0], entries[end] = entries[end], entries[0]
		sift(0, end)
	}
}

// jitSortAddressEntries sorts mapping and funcdata indexes by start address.
// Empty ranges sort first and are ignored by the interval lookup.
func jitSortAddressEntries(entries []jitModuleAddressEntry) {
	sift := func(root, end int) {
		for {
			child := root*2 + 1
			if child >= end {
				return
			}
			if child+1 < end && entries[child].start < entries[child+1].start {
				child++
			}
			if entries[root].start >= entries[child].start {
				return
			}
			entries[root], entries[child] = entries[child], entries[root]
			root = child
		}
	}
	for root := len(entries)/2 - 1; root >= 0; root-- {
		sift(root, len(entries))
	}
	for end := len(entries) - 1; end > 0; end-- {
		entries[0], entries[end] = entries[end], entries[0]
		sift(0, end)
	}
}

//go:nosplit
func jitPCEntry(entries []jitModuleIndexEntry, pc uintptr) *jitModuleIndexEntry {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if pc < entries[mid].minpc {
			hi = mid
		} else if pc >= entries[mid].maxpc {
			lo = mid + 1
		} else {
			return &entries[mid]
		}
	}
	return nil
}

//go:nosplit
func jitAddressEntry(entries []jitModuleAddressEntry, address uintptr) *jitModuleAddressEntry {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if address < entries[mid].start {
			hi = mid
		} else if address >= entries[mid].end {
			lo = mid + 1
		} else {
			return &entries[mid]
		}
	}
	return nil
}

//go:nosplit
func jitLookupPC(pc uintptr) (*moduledata, *JITModuleOwner) {
	md, owner, lease := jitLookupPCLease(pc)
	if lease != 0 {
		jitIndexReadLeave(lease)
	}
	return md, owner
}

//go:nosplit
func jitLookupPCLease(pc uintptr) (*moduledata, *JITModuleOwner, uintptr) {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, nil, 0
	}
	h := (*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))
	entries := jitIndexEntries(h)
	entry := jitPCEntry(entries, pc)
	if entry != nil {
		if atomic.Loaduintptr(&entry.owner) == 0 {
			jitIndexReadLeave(index)
			return nil, nil, 0
		}
		md := (*moduledata)(unsafe.Pointer(entry.module))
		return md, nil, index
	}
	jitIndexReadLeave(index)
	return nil, nil, 0
}

func jitLookupPCLN(ptr uintptr) (*moduledata, *JITModuleOwner) {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, nil
	}
	h := (*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))
	entries := jitIndexEntries(h)
	for i := range entries {
		entry := &entries[i]
		if entry.pclnStart <= ptr && ptr < entry.pclnEnd {
			if atomic.Loaduintptr(&entry.owner) == 0 {
				jitIndexReadLeave(index)
				return nil, nil
			}
			md := (*moduledata)(unsafe.Pointer(entry.module))
			owner := (*JITModuleOwner)(internal_weak_runtime_makeStrongFromWeak(unsafe.Pointer(entry.weakHandle)))
			jitIndexReadLeave(index)
			KeepAlive(owner)
			if owner == nil {
				return nil, nil
			}
			return md, owner
		}
	}
	jitIndexReadLeave(index)
	return nil, nil
}

// jitLookupGofunc finds dynamic module metadata for a stackObjectRecord. The
// record's gcdataoff is relative to moduledata.rodata, so stack copying and GC
// stack-object scanning must recover the owning module from its gofunc range.
func jitLookupGofunc(ptr uintptr) *moduledata {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil
	}
	h := (*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))
	entry := jitAddressEntry(jitIndexGofuncEntries(h), ptr)
	if entry != nil && atomic.Loaduintptr(&entry.owner) != 0 {
		md := (*moduledata)(unsafe.Pointer(entry.module))
		jitIndexReadLeave(index)
		return md
	}
	jitIndexReadLeave(index)
	return nil
}

func publishJITModule(md *moduledata, owner *JITModuleOwner) error {
	if md == nil || owner == nil || owner.generation == 0 {
		return JITModuleError("runtime: invalid JIT module publication identity")
	}
	lock(&jitIndexLock)
	defer unlock(&jitIndexLock)
	oldPtr := atomic.Loaduintptr(&jitModuleIndexPtr)
	oldCount := uintptr(0)
	var oldEntries []jitModuleIndexEntry
	if oldPtr != 0 {
		oldEntries = jitIndexEntries((*jitModuleIndex)(unsafe.Pointer(oldPtr)))
		for i := range oldEntries {
			if atomic.Loaduintptr(&oldEntries[i].owner) != 0 {
				oldCount++
			}
		}
	}
	if oldCount >= 1<<20 {
		return JITModuleError("runtime: too many JIT modules")
	}
	count := oldCount + 1
	size := jitModuleIndexSize(count)
	if err := reserveJITMemory(size); err != nil {
		return err
	}
	base := sysAlloc(size, &memstats.other_sys, "jit module index")
	if base == nil {
		releaseJITMemory(size)
		return JITModuleError("runtime: cannot allocate JIT module index")
	}
	memclrNoHeapPointers(base, size)
	next := (*jitModuleIndex)(base)
	next.count = count
	next.addressOffset = unsafe.Sizeof(jitModuleIndex{}) + count*unsafe.Sizeof(jitModuleIndexEntry{})
	next.gofuncOffset = next.addressOffset + count*unsafe.Sizeof(jitModuleAddressEntry{})
	entries := jitIndexEntries(next)
	addressEntries := jitIndexAddressEntries(next)
	gofuncEntries := jitIndexGofuncEntries(next)
	copyAt := 0
	for i := range oldEntries {
		if atomic.Loaduintptr(&oldEntries[i].owner) != 0 {
			entries[copyAt] = oldEntries[i]
			copyAt++
		}
	}
	entries[copyAt] = jitModuleIndexEntry{
		minpc: md.minpc, maxpc: md.maxpc,
		mapStart: owner.mapping.Base(), mapEnd: owner.mapping.Base() + owner.mapping.Size(),
		pclnStart:   uintptr(unsafe.Pointer(unsafe.SliceData(md.pclntable))),
		pclnEnd:     uintptr(unsafe.Pointer(unsafe.SliceData(md.pclntable))) + uintptr(len(md.pclntable)),
		gofuncStart: md.gofunc,
		gofuncEnd:   md.gofunc + md.jitGofuncSize,
		generation:  owner.generation,
		module:      uintptr(unsafe.Pointer(md)), owner: uintptr(unsafe.Pointer(owner)), weakHandle: uintptr(owner.weakHandle),
	}
	if md.jitGofuncSize == 0 {
		entries[copyAt].gofuncStart, entries[copyAt].gofuncEnd = 0, 0
	}
	jitSortPCEntries(entries)
	for i, entry := range entries {
		addressEntries[i] = jitModuleAddressEntry{
			start: entry.mapStart, end: entry.mapEnd, generation: entry.generation,
			module: entry.module, owner: entry.owner, weakHandle: entry.weakHandle,
		}
		gofuncEntries[i] = jitModuleAddressEntry{
			start: entry.gofuncStart, end: entry.gofuncEnd, generation: entry.generation,
			module: entry.module, owner: entry.owner, weakHandle: entry.weakHandle,
		}
	}
	jitSortAddressEntries(addressEntries)
	jitSortAddressEntries(gofuncEntries)
	for i := 1; i < len(entries); i++ {
		if entries[i-1].maxpc > entries[i].minpc {
			sysFree(base, size, &memstats.other_sys)
			releaseJITMemory(size)
			return JITModuleError("runtime: overlapping JIT text ranges")
		}
		if addressEntries[i-1].end > addressEntries[i].start {
			sysFree(base, size, &memstats.other_sys)
			releaseJITMemory(size)
			return JITModuleError("runtime: overlapping JIT mappings")
		}
		if gofuncEntries[i-1].end > gofuncEntries[i].start {
			sysFree(base, size, &memstats.other_sys)
			releaseJITMemory(size)
			return JITModuleError("runtime: overlapping JIT funcdata ranges")
		}
	}
	jitExpandMappingBounds(owner.mapping.Base(), owner.mapping.Base()+owner.mapping.Size())
	oldEpoch := jitPublishIndex(uintptr(base))
	jitWaitIndexEpoch(oldEpoch)
	if oldPtr != 0 {
		old := (*jitModuleIndex)(unsafe.Pointer(oldPtr))
		oldSize := jitModuleIndexSize(old.count)
		sysFree(unsafe.Pointer(oldPtr), oldSize, &memstats.other_sys)
		releaseJITMemory(oldSize)
	}
	return nil
}

func unpublishJITModule(md *moduledata, generation uint64) error {
	if md == nil || generation == 0 {
		return JITModuleError("runtime: invalid JIT module retirement identity")
	}
	lock(&jitIndexLock)
	defer unlock(&jitIndexLock)
	oldPtr := atomic.Loaduintptr(&jitModuleIndexPtr)
	if oldPtr == 0 {
		return JITModuleError("runtime: unknown JIT module")
	}
	old := (*jitModuleIndex)(unsafe.Pointer(oldPtr))
	oldEntries := jitIndexEntries(old)
	oldAddressEntries := jitIndexAddressEntries(old)
	oldGofuncEntries := jitIndexGofuncEntries(old)
	remove := -1
	for i := range oldEntries {
		if oldEntries[i].module == uintptr(unsafe.Pointer(md)) && oldEntries[i].generation == generation && atomic.Loaduintptr(&oldEntries[i].owner) != 0 {
			remove = i
			break
		}
	}
	if remove < 0 {
		return JITModuleError("runtime: unknown JIT module")
	}
	count := uintptr(0)
	for i := range oldEntries {
		if i != remove && atomic.Loaduintptr(&oldEntries[i].owner) != 0 {
			count++
		}
	}
	var newPtr uintptr
	var newBase unsafe.Pointer
	if count != 0 {
		newSize := jitModuleIndexSize(count)
		// Retirement must not keep an owner alive or block forever because a
		// compact replacement exceeds the budget. If it cannot reserve a new
		// snapshot, tombstone this entry in place and leave the old allocation
		// charged until a later publication can compact it or the last module
		// retires.
		if reserveErr := reserveJITMemory(newSize); reserveErr == nil {
			newBase = sysAlloc(newSize, &memstats.other_sys, "jit module index")
			if newBase == nil {
				releaseJITMemory(newSize)
			}
		}
		if newBase != nil {
			memclrNoHeapPointers(newBase, newSize)
			next := (*jitModuleIndex)(newBase)
			next.count = count
			next.addressOffset = unsafe.Sizeof(jitModuleIndex{}) + count*unsafe.Sizeof(jitModuleIndexEntry{})
			next.gofuncOffset = next.addressOffset + count*unsafe.Sizeof(jitModuleAddressEntry{})
			entries := jitIndexEntries(next)
			addressEntries := jitIndexAddressEntries(next)
			gofuncEntries := jitIndexGofuncEntries(next)
			copyAt := 0
			for i := range oldEntries {
				if i != remove && atomic.Loaduintptr(&oldEntries[i].owner) != 0 {
					entries[copyAt] = oldEntries[i]
					copyAt++
				}
			}
			for i, entry := range entries {
				addressEntries[i] = jitModuleAddressEntry{
					start: entry.mapStart, end: entry.mapEnd, generation: entry.generation,
					module: entry.module, owner: entry.owner, weakHandle: entry.weakHandle,
				}
				gofuncEntries[i] = jitModuleAddressEntry{
					start: entry.gofuncStart, end: entry.gofuncEnd, generation: entry.generation,
					module: entry.module, owner: entry.owner, weakHandle: entry.weakHandle,
				}
			}
			jitSortAddressEntries(addressEntries)
			jitSortAddressEntries(gofuncEntries)
			newPtr = uintptr(newBase)
		}
	}
	var oldEpoch uint32
	if count != 0 && newBase == nil {
		// Allocation failure falls back to a tombstone. Readers check owner
		// before dereferencing the module pointer, so the old snapshot remains
		// valid without retaining this generation's metadata.
		atomic.Storeuintptr(&oldEntries[remove].owner, 0)
		for i := range oldAddressEntries {
			if oldAddressEntries[i].module == uintptr(unsafe.Pointer(md)) && oldAddressEntries[i].generation == generation {
				atomic.Storeuintptr(&oldAddressEntries[i].owner, 0)
				break
			}
		}
		for i := range oldGofuncEntries {
			if oldGofuncEntries[i].module == uintptr(unsafe.Pointer(md)) && oldGofuncEntries[i].generation == generation {
				atomic.Storeuintptr(&oldGofuncEntries[i].owner, 0)
				break
			}
		}
		oldEpoch = jitPublishIndex(oldPtr)
	} else {
		oldEpoch = jitPublishIndex(newPtr)
	}
	jitWaitIndexEpoch(oldEpoch)
	if newPtr != 0 || count == 0 {
		oldSize := jitModuleIndexSize(old.count)
		sysFree(unsafe.Pointer(oldPtr), oldSize, &memstats.other_sys)
		releaseJITMemory(oldSize)
	}
	return nil
}

// PinJITModule keeps a module alive for process lifetime. It is used by APIs
// that return legacy raw PCs or metadata objects with no owner-bearing handle.
//
//go:nowritebarrier
func PinJITModule(owner *JITModuleOwner) {
	if owner == nil {
		return
	}
	lock(&jitOwnerRootLock)
	if owner.pinSlot == nil {
		unlock(&jitOwnerRootLock)
		throw("runtime: JIT owner has no preallocated root slot")
	}
	if owner.pinSlot.owner != owner {
		atomicstorep(unsafe.Pointer(&owner.pinSlot.owner), unsafe.Pointer(owner))
	}
	unlock(&jitOwnerRootLock)
	KeepAlive(owner)
}

// registerJITOwnerRoots makes a weak handle available to the pointer-free PC
// index and preallocates the strong-root slot used by legacy raw-PC APIs.
func registerJITOwnerRoots(owner *JITModuleOwner) bool {
	if owner == nil {
		return false
	}
	handle := internal_weak_runtime_registerWeakPointer(unsafe.Pointer(owner))
	if handle == nil {
		return false
	}
	slot := new(jitOwnerRootSlot)
	lock(&jitOwnerRootLock)
	jitOwnerWeakRoots = append(jitOwnerWeakRoots, handle)
	jitOwnerPinSlots = append(jitOwnerPinSlots, slot)
	owner.weakHandle = handle
	owner.pinSlot = slot
	unlock(&jitOwnerRootLock)
	KeepAlive(owner)
	return true
}

// removeJITOwnerRoots drops the runtime's weak-handle root and the unused
// strong pin slot after the PC index has been unpublished and its readers have
// drained. A non-nil slot owner would make the owner reachable, so reaching
// retirement in that state is an invariant violation.
func removeJITOwnerRoots(weakHandle, pinSlot uintptr) {
	lock(&jitOwnerRootLock)
	var foundWeak, foundSlot bool
	for i, handle := range jitOwnerWeakRoots {
		if uintptr(handle) == weakHandle {
			last := len(jitOwnerWeakRoots) - 1
			jitOwnerWeakRoots[i] = jitOwnerWeakRoots[last]
			jitOwnerWeakRoots[last] = nil
			jitOwnerWeakRoots = jitOwnerWeakRoots[:last]
			foundWeak = true
			break
		}
	}
	for i, slot := range jitOwnerPinSlots {
		if uintptr(unsafe.Pointer(slot)) == pinSlot {
			if slot.owner != nil {
				unlock(&jitOwnerRootLock)
				throw("runtime: retired JIT owner remains pinned")
			}
			last := len(jitOwnerPinSlots) - 1
			jitOwnerPinSlots[i] = jitOwnerPinSlots[last]
			jitOwnerPinSlots[last] = nil
			jitOwnerPinSlots = jitOwnerPinSlots[:last]
			foundSlot = true
			break
		}
	}
	unlock(&jitOwnerRootLock)
	if !foundWeak || !foundSlot {
		throw("runtime: missing JIT owner roots")
	}
}

// jitOwnerForPC safely converts the index's weak handle to a strong pointer.
// The reader lease keeps the handle available while the weak-pointer operation
// synchronizes with GC.
func jitOwnerForPC(pc uintptr) (*JITModuleOwner, bool) {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, false
	}
	h := (*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))
	entries := jitIndexEntries(h)
	entry := jitPCEntry(entries, pc)
	if entry != nil {
		if atomic.Loaduintptr(&entry.owner) == 0 {
			jitIndexReadLeave(index)
			return nil, true
		}
		owner := (*JITModuleOwner)(internal_weak_runtime_makeStrongFromWeak(unsafe.Pointer(entry.weakHandle)))
		jitIndexReadLeave(index)
		KeepAlive(owner)
		return owner, true
	}
	jitIndexReadLeave(index)
	return nil, false
}

// JITModuleRegistered reports whether pc belongs to a currently published JIT
// module. It is diagnostic only and does not retain the module or make a PC
// safe to call.
func JITModuleRegistered(pc uintptr) bool {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return false
	}
	entry := jitPCEntry(jitIndexEntries((*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))), pc)
	registered := entry != nil && atomic.Loaduintptr(&entry.owner) != 0
	jitIndexReadLeave(index)
	return registered
}

// jitOwnerForLivePC is only for a PC captured from a currently executing Go
// stack. Such a stack must already hold the owner through the generated entry
// parameter or closure environment. The caller pins it before that stack
// reference can disappear.
func jitOwnerForLivePC(pc uintptr) (*JITModuleOwner, bool) {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, false
	}
	h := (*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))
	entries := jitIndexEntries(h)
	entry := jitPCEntry(entries, pc)
	if entry != nil {
		if atomic.Loaduintptr(&entry.owner) == 0 {
			jitIndexReadLeave(index)
			return nil, true
		}
		owner := (*JITModuleOwner)(unsafe.Pointer(atomic.Loaduintptr(&entry.owner)))
		jitIndexReadLeave(index)
		return owner, true
	}
	jitIndexReadLeave(index)
	return nil, false
}

// jitScanGlobals scans a shard of pointer-bearing package data outside the Go
// heap. It runs at the GC mark-done ragged barrier, after ordinary roots have
// reached a fixed point. Mutators may resume after their P's callback, so
// writes after a module's shard is scanned rely on the normal write barrier
// and final termination flush. The owner is scanned only when ordinary Go
// roots already marked it in this GC cycle; otherwise scanning its globals
// would make unreachable modules live forever. The object's GC bitmap was
// validated and captured before the module was published. Any newly marked
// objects restart concurrent marking, which runs this scan again after those
// objects have been traced.
//
//go:nowritebarrier
func jitScanGlobals(gcw *gcWork, shard, nshards int) int64 {
	if nshards <= 0 || shard < 0 || shard >= nshards {
		return 0
	}
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return 0
	}
	entries := jitIndexEntries((*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index))))
	var workDone int64
	for i := shard; i < len(entries); i += nshards {
		entry := &entries[i]
		if atomic.Loaduintptr(&entry.owner) == 0 || entry.weakHandle == 0 {
			continue
		}
		ownerAddress := (*atomic.Uintptr)(unsafe.Pointer(entry.weakHandle)).Load()
		if ownerAddress == 0 {
			continue
		}
		obj, span, objIndex := findObject(ownerAddress, 0, 0)
		if obj == 0 || !span.markBitsForIndex(objIndex).isMarked() {
			continue
		}
		owner := (*JITModuleOwner)(unsafe.Pointer(ownerAddress))
		for _, root := range owner.globals {
			scanblock(root.address, root.ptrBytes, (*byte)(root.mask), gcw, nil)
			workDone += int64(root.ptrBytes)
		}
		KeepAlive(owner)
	}
	jitIndexReadLeave(index)
	return workDone
}

//go:nosplit
func jitOwnerForMappedAddressLease(address uintptr) (*JITModuleOwner, bool, uintptr) {
	min := atomic.Loaduintptr(&jitMappingMin)
	max := atomic.Loaduintptr(&jitMappingMax)
	if min == 0 || address < min || address >= max || atomic.Loaduintptr(&jitModuleIndexPtr) == 0 {
		return nil, false, 0
	}
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, false, 0
	}
	entries := jitIndexAddressEntries((*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index))))
	entry := jitAddressEntry(entries, address)
	if entry == nil || atomic.Loaduintptr(&entry.owner) == 0 || entry.weakHandle == 0 {
		jitIndexReadLeave(index)
		return nil, false, 0
	}
	// The raw owner address in the immutable index may outlive the heap object
	// by one GC cycle while the reaper waits to unpublish the module. Read the
	// weak handle itself: it is cleared before the object's storage is freed.
	ownerAddr := (*atomic.Uintptr)(unsafe.Pointer(entry.weakHandle)).Load()
	if ownerAddr == 0 {
		// The mapping remains published until the retirement worker removes
		// this index generation. Report it as mapped while holding the lease so
		// GC scanners never mistake its address for an arena heap pointer.
		return nil, true, index
	}
	owner := (*JITModuleOwner)(unsafe.Pointer(ownerAddr))
	return owner, true, index
}

// jitOwnerForMappedAddress converts an address in a published JIT mapping to
// a strong owner reference. Unlike jitOwnerForMappedAddressLease, this is for
// ordinary runtime paths that may allocate or return an owner-bearing value;
// GC scan paths must use the lease form and mark the owner directly.
func jitOwnerForMappedAddress(address uintptr) (*JITModuleOwner, bool) {
	min := atomic.Loaduintptr(&jitMappingMin)
	max := atomic.Loaduintptr(&jitMappingMax)
	if min == 0 || address < min || address >= max || atomic.Loaduintptr(&jitModuleIndexPtr) == 0 {
		return nil, false
	}
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, false
	}
	entries := jitIndexAddressEntries((*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index))))
	entry := jitAddressEntry(entries, address)
	if entry == nil || atomic.Loaduintptr(&entry.owner) == 0 || entry.weakHandle == 0 {
		jitIndexReadLeave(index)
		return nil, false
	}
	owner := (*JITModuleOwner)(internal_weak_runtime_makeStrongFromWeak(unsafe.Pointer(entry.weakHandle)))
	jitIndexReadLeave(index)
	KeepAlive(owner)
	return owner, true
}

// jitAddressIsMapped reports whether address belongs to a currently
// published JIT mapping. It deliberately does not promote the weak owner.
func jitAddressIsMapped(address uintptr) bool {
	_, found, lease := jitOwnerForMappedAddressLease(address)
	if found {
		jitIndexReadLeave(lease)
	}
	return found
}

// jitLookupTypeModule returns the dynamic module whose type-offset base
// contains address. The caller must keep the source type pointer live until it
// has finished reading the returned module metadata. That typed pointer is a
// GC edge into the mapping, so stack/root scanning marks the owner without
// promoting a weak handle or taking a write barrier in this no-write-barrier
// lookup path.
func jitLookupTypeModule(address uintptr) (*moduledata, bool) {
	min := atomic.Loaduintptr(&jitMappingMin)
	max := atomic.Loaduintptr(&jitMappingMax)
	if min == 0 || address < min || address >= max || atomic.Loaduintptr(&jitModuleIndexPtr) == 0 {
		return nil, false
	}
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return nil, false
	}
	entries := jitIndexAddressEntries((*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index))))
	entry := jitAddressEntry(entries, address)
	if entry == nil || atomic.Loaduintptr(&entry.owner) == 0 || entry.weakHandle == 0 {
		jitIndexReadLeave(index)
		return nil, false
	}
	md := (*moduledata)(unsafe.Pointer(entry.module))
	found := md.jitModule && md.types != 0 && md.types <= address && address < md.etypes
	jitIndexReadLeave(index)
	if !found {
		return nil, false
	}
	return md, true
}

// pinAllJITModules permanently roots every currently live module and enables
// the registration-side policy that roots later modules too. This is used
// where the runtime stores raw PCs asynchronously (CPU profiles and traces).
func pinAllJITModules() {
	atomic.Storeint32(&jitRawPCPins, 1)
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		return
	}
	h := (*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index)))
	entries := jitIndexEntries(h)
	for i := range entries {
		if atomic.Loaduintptr(&entries[i].owner) == 0 {
			continue
		}
		owner := (*JITModuleOwner)(internal_weak_runtime_makeStrongFromWeak(unsafe.Pointer(entries[i].weakHandle)))
		PinJITModule(owner)
	}
	jitIndexReadLeave(index)
}

func pinJITStack(stk []uintptr) {
	for _, pc := range stk {
		owner, found := jitOwnerForLivePC(pc)
		if found {
			if owner == nil {
				throw("runtime: live JIT PC has no owner")
			}
			PinJITModule(owner)
		}
	}
}
