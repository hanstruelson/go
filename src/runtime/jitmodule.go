// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"publicinternal/abi"
	"publicinternal/goarch"
	"publicinternal/runtime/atomic"
	"publicinternal/runtime/sys"
	"unsafe"
)

var (
	jitTypeIdentityLock        mutex
	jitActiveTypeIdentityGuard *JITTypeIdentityGuard // protected by jitTypeIdentityLock
	jitReflectTypeLock         mutex
	jitReflectTypes            map[string]unsafe.Pointer // weak handles; keys contain no descriptor addresses
	jitReflectTypeBytes        uint64
)

const (
	jitReflectTypeCachePruneBytes = 1 << 20
	jitReflectTypeCacheEntryBytes = 32 // approximate map entry and bucket overhead
)

// JITFunctionMetadata is the function-table input for one function in an
// in-memory Go module. The compiler/linker owns the PC tables, source file
// mapping, and stack maps; the runtime copies them into non-Go memory before
// publishing the module to stack walkers and the garbage collector.
//
// This interface is intended for the in-process JIT implementation. Callers
// must only provide metadata produced for the current Go toolchain and target.
type JITFunctionMetadata struct {
	Offset    uint32
	Size      uint32
	Name      string
	ArgsSize  uint32
	StartLine int32
	FuncID    uint8
	FuncFlag  uint8
	PCSP      []byte
	PCFile    []byte
	PCLine    []byte
	// Files is the complete CU-indexed file table used by PCFile. Preserve
	// empty slots if an object format contains an unused CU file index.
	Files    []string
	PCData   [][]byte
	FuncData [][]byte
}

// JITCaptureBinding couples a call-site-local binding identity with the
// address and exact static type of its caller cell. Runtime owners retain
// these records as a scanned slice and retain module owners referenced by the
// type graph; generated code resolves a binding by ID rather than position.
type JITCaptureBinding struct {
	BindingID uint64
	Address   unsafe.Pointer
	Type      unsafe.Pointer // exact static capture type descriptor
}

// JITGlobalMetadata describes one writable package global in a JIT image.
// Type points at the Go runtime type descriptor for the exact symbol extent.
// The runtime uses its pointer map to scan this external storage while the
// owning module is live.
type JITGlobalMetadata struct {
	Address uintptr
	Size    uintptr
	Type    unsafe.Pointer
}

// JITTypeMapping associates a local, module-relative descriptor with its
// canonical descriptor. The canonical descriptor can belong to the host
// executable, to an already published JIT module, or to a live reflect-created
// type descriptor. The private roots keep weakly cataloged candidates live
// until registration takes over their lifetime dependency.
type JITTypeMapping struct {
	Dynamic   uintptr
	Canonical uintptr
	owner     *JITModuleOwner // temporary strong edge held from discovery through registration
	heapType  *_type          // temporary root for a reflect-created canonical descriptor
}

// JITTypeIdentityGuard serializes image canonicalization, reflect-created type
// interning, and plugin typelink publication. Keep it from descriptor
// discovery through module publish so a reflect constructor or plugin load
// cannot change the canonical descriptor catalog in the middle of an update.
type JITTypeIdentityGuard struct {
	locked bool
}

// JITBeginTypeIdentityUpdate starts one serialized runtime type-catalog update.
func JITBeginTypeIdentityUpdate() *JITTypeIdentityGuard {
	guard := new(JITTypeIdentityGuard)
	lock(&jitTypeIdentityLock)
	guard.locked = true
	if jitActiveTypeIdentityGuard != nil {
		throw("runtime: JIT type identity guard already active")
	}
	jitActiveTypeIdentityGuard = guard
	return guard
}

// Release ends the serialized type-catalog update. It is safe to call after a
// failed mapping or registration, but each guard must be released once.
func (guard *JITTypeIdentityGuard) Release() {
	if guard == nil || !guard.locked || jitActiveTypeIdentityGuard != guard {
		throw("runtime: invalid JIT type identity guard release")
	}
	jitActiveTypeIdentityGuard = nil
	guard.locked = false
	unlock(&jitTypeIdentityLock)
}

func jitTypeIdentityGuardHeld(guard *JITTypeIdentityGuard) bool {
	return guard != nil && guard.locked && jitActiveTypeIdentityGuard == guard
}

// JITReflectOffset is one name, type, or text target referenced by a
// reflect-created type descriptor. Offsets are local to that descriptor;
// they are not process-wide IDs. The containing descriptor's GC edge owns
// these targets for exactly as long as the descriptor remains live.
type JITReflectOffset struct {
	Target unsafe.Pointer
	Kind   uint8
}

const (
	JITReflectNameOffset uint8 = iota + 1
	JITReflectTypeOffset
	JITReflectTextOffset
)

// JITTypeNeedsOwner reports whether typ is stored in a reclaimable JIT image
// or is a reflect-created descriptor that depends on one. Reflection uses
// this to avoid placing such descriptors in process-global caches.
func JITTypeNeedsOwner(typ unsafe.Pointer) bool {
	if typ == nil {
		return false
	}
	return jitTypeNeedsOwner((*_type)(typ))
}

// JITTypeUsesGCMaskOnDemand reports whether a compiler-emitted type descriptor
// requires the runtime to mutate its GC mask cache. Image descriptors are
// immutable after publication, so the loader rejects this form; reflect-made
// descriptors live in the Go heap and can use the ordinary runtime path.
func JITTypeUsesGCMaskOnDemand(typ unsafe.Pointer) bool {
	return typ != nil && (*_type)(typ).TFlag&abi.TFlagGCMaskOnDemand != 0
}

// JITTypeDescriptorSize is the common ABI descriptor size expected by the
// runtime for a compiler-emitted JIT type symbol.
func JITTypeDescriptorSize() uintptr { return unsafe.Sizeof(_type{}) }

// JITCanonicalizeTypes finds existing identities for unnamed descriptors in
// a staged image. The returned mappings are only candidates: registration
// validates them again and retains every JIT module that owns a canonical
// descriptor before publishing the new module. It checks static typelinks,
// live JIT descriptors, and reflect-created descriptors already present in
// the weak structural cache; candidate roots are held until registration.
func JITCanonicalizeTypes(guard *JITTypeIdentityGuard, typesStart, typesSize uintptr, descriptors []unsafe.Pointer, mappings []JITTypeMapping) ([]JITTypeMapping, error) {
	if !jitTypeIdentityGuardHeld(guard) {
		return nil, JITModuleError("runtime: JIT type canonicalization requires an identity guard")
	}
	if typesStart == 0 || typesSize == 0 || typesStart+typesSize < typesStart {
		return nil, JITModuleError("runtime: invalid staged JIT type range")
	}
	typesEnd := typesStart + typesSize
	descriptorSize := unsafe.Sizeof(_type{})
	typeMap := make(map[*_type]*_type, len(mappings)+len(descriptors))
	staged := &moduledata{types: typesStart, etypes: typesEnd, jitModule: true, typemap: typeMap}
	seen := make(map[_typePair]struct{})
	var result []JITTypeMapping
	for _, pair := range mappings {
		if pair.Dynamic%goarch.PtrSize != 0 || pair.Dynamic < typesStart || pair.Dynamic > typesEnd ||
			typesEnd-pair.Dynamic < descriptorSize || pair.Canonical == 0 || pair.Canonical%goarch.PtrSize != 0 {
			return nil, JITModuleError("runtime: invalid explicit JIT type mapping")
		}
		dynamicType := (*_type)(unsafe.Pointer(pair.Dynamic))
		if owner, found := jitOwnerForMappedAddress(pair.Canonical); found {
			if owner == nil {
				return nil, JITModuleError("runtime: canonical JIT type module is retiring")
			}
			pair.owner = owner
		}
		if pair.heapType == nil {
			obj, span, _ := findObject(pair.Canonical, 0, 0)
			if obj == pair.Canonical && span != nil && span.elemsize >= descriptorSize {
				candidateType := (*_type)(unsafe.Pointer(pair.Canonical))
				if metadata := jitMetadataForHeapObject(pair.Canonical); metadata != nil && metadata.typ == candidateType {
					pair.heapType = candidateType
					KeepAlive(metadata)
				}
			}
		}
		canonicalValid := pair.heapType != nil
		for _, md := range activeModules() {
			if pair.Canonical >= md.types && pair.Canonical <= md.etypes && md.etypes-pair.Canonical >= descriptorSize {
				canonicalValid = true
				break
			}
		}
		if pair.owner != nil {
			md := (*moduledata)(unsafe.Pointer(pair.owner.module))
			if md != nil && md.jitModule && pair.Canonical >= md.types && pair.Canonical <= md.etypes && md.etypes-pair.Canonical >= descriptorSize {
				canonicalValid = true
			}
		}
		if !canonicalValid {
			return nil, JITModuleError("runtime: explicit canonical JIT type is not in a live type descriptor range")
		}
		canonicalType := (*_type)(unsafe.Pointer(pair.Canonical))
		equal := dynamicType.Hash == canonicalType.Hash && jitTypesEqual(dynamicType, canonicalType, seen, staged)
		clear(seen)
		KeepAlive(pair.owner)
		KeepAlive(pair.heapType)
		if !equal {
			// The object linker can only use a display string to propose a host
			// alias. If the actual descriptors differ, discard the hint and let
			// structural interning choose the right identity from the catalogs.
			continue
		}
		if previous, exists := typeMap[dynamicType]; exists {
			if previous == canonicalType {
				continue
			}
			continue
		}
		typeMap[dynamicType] = canonicalType
		result = append(result, pair)
	}
	descriptorTypes := make([]*_type, 0, len(descriptors))
	seenDescriptors := make(map[*_type]struct{}, len(descriptors))
	wantedHashes := make(map[uint32]struct{})
	for _, descriptor := range descriptors {
		address := uintptr(descriptor)
		if address%goarch.PtrSize != 0 || address < typesStart || address > typesEnd || typesEnd-address < descriptorSize {
			return nil, JITModuleError("runtime: staged JIT type descriptor is outside its type range")
		}
		typ := (*_type)(descriptor)
		if _, duplicate := seenDescriptors[typ]; duplicate {
			continue
		}
		seenDescriptors[typ] = struct{}{}
		descriptorTypes = append(descriptorTypes, typ)
		if typ.TFlag&abi.TFlagNamed == 0 {
			wantedHashes[typ.Hash] = struct{}{}
		}
	}

	type candidate struct {
		typ   *_type
		owner *JITModuleOwner
		heap  *_type
	}
	byHash := make(map[uint32][]candidate)
	seenCandidates := make(map[*_type]struct{})
	addCandidate := func(typ *_type, owner *JITModuleOwner, heapType *_type) {
		if typ == nil || typ.TFlag&abi.TFlagNamed != 0 {
			return
		}
		if _, wanted := wantedHashes[typ.Hash]; !wanted {
			return
		}
		if _, ok := seenCandidates[typ]; ok {
			return
		}
		seenCandidates[typ] = struct{}{}
		byHash[typ.Hash] = append(byHash[typ.Hash], candidate{typ: typ, owner: owner, heap: heapType})
	}
	// The static module order defines the process-lifetime canonical identity.
	for _, md := range activeModules() {
		for _, typ := range moduleTypelinks(md) {
			if canonical := md.typemap[typ]; canonical != nil {
				typ = canonical
			}
			addCandidate(typ, nil, nil)
		}
	}
	owners := jitLiveModuleOwners()
	for _, owner := range owners {
		md := (*moduledata)(unsafe.Pointer(owner.module))
		if md == nil || !md.jitModule {
			continue
		}
		for _, address := range owner.typeDescriptors {
			typ := (*_type)(unsafe.Pointer(address))
			if canonical := md.typemap[typ]; canonical != nil {
				typ = canonical
			}
			addCandidate(typ, owner, nil)
		}
	}
	reflectTypes := jitLiveReflectTypeDescriptors()
	for _, typ := range reflectTypes {
		addCandidate(typ, nil, typ)
	}

	for _, typ := range descriptorTypes {
		address := uintptr(unsafe.Pointer(typ))
		if _, explicit := typeMap[typ]; explicit || typ.TFlag&abi.TFlagNamed != 0 {
			canonical := typeMap[typ]
			if canonical == nil {
				canonical = typ
			}
			addCandidate(canonical, nil, nil)
			continue
		}
		for _, candidate := range byHash[typ.Hash] {
			if typ == candidate.typ {
				continue
			}
			if jitTypesEqual(typ, candidate.typ, seen, staged) {
				typeMap[typ] = candidate.typ
				result = append(result, JITTypeMapping{
					Dynamic: address, Canonical: uintptr(unsafe.Pointer(candidate.typ)),
					owner: candidate.owner, heapType: candidate.heap,
				})
				break
			}
			clear(seen)
		}
		canonical := typeMap[typ]
		if canonical == nil {
			canonical = typ
		}
		addCandidate(canonical, nil, nil)
	}
	KeepAlive(owners)
	KeepAlive(reflectTypes)
	KeepAlive(descriptorTypes)
	return result, nil
}

func jitLiveReflectTypeDescriptors() []*_type {
	lock(&jitReflectTypeLock)
	handles := make([]unsafe.Pointer, 0, len(jitReflectTypes))
	for _, weak := range jitReflectTypes {
		handles = append(handles, weak)
	}
	unlock(&jitReflectTypeLock)
	types := make([]*_type, 0, len(handles))
	for _, handle := range handles {
		if typ := (*_type)(internal_weak_runtime_makeStrongFromWeak(handle)); typ != nil {
			types = append(types, typ)
		}
	}
	KeepAlive(handles)
	return types
}

// JITTypeIdentity returns a process-unique identity for a named dynamic type.
// Image types use their module generation and module-relative address. A
// reflect-created descriptor uses an identity stored in its GC-owned metadata.
// Unnamed types should be identified structurally by reflect instead.
func JITTypeIdentity(typ unsafe.Pointer) (generation, identity uint64, ok bool) {
	if typ == nil {
		return 0, 0, false
	}
	address := uintptr(typ)
	if owner, found := jitOwnerForMappedAddress(address); found {
		if owner == nil {
			throw("runtime: named JIT type has no live owner")
		}
		base := owner.mapping.Base()
		if address < base {
			throw("runtime: JIT type address precedes its mapping")
		}
		generation, identity = owner.generation, uint64(address-base)
		KeepAlive(owner)
		return generation, identity, true
	}
	if metadata := jitMetadataForHeapObject(address); metadata != nil && metadata.identity != 0 {
		identity = metadata.identity
		KeepAlive(metadata)
		return 0, identity, true
	}
	return 0, 0, false
}

type jitGlobalRoot struct {
	address  uintptr
	ptrBytes uintptr
	mask     unsafe.Pointer
}

// JITModuleError describes invalid runtime metadata or a rejected JIT lifetime
// transition.
type JITModuleError string

func (e JITModuleError) Error() string { return string(e) }

// JITMapping is an unforgeable runtime-accounted external mapping. Target
// implementations provide the mapping and protection operations; the runtime
// JIT owner keeps its token reachable until retirement has completed.
type JITMapping struct {
	mu    mutex
	base  uintptr
	size  uintptr
	owner *JITModuleOwner
}

// Base returns the current mapping address, or zero after unmapping.
func (m *JITMapping) Base() uintptr {
	if m == nil {
		return 0
	}
	lock(&m.mu)
	base := m.base
	unlock(&m.mu)
	return base
}

// Size returns the current mapping size, or zero after unmapping.
func (m *JITMapping) Size() uintptr {
	if m == nil {
		return 0
	}
	lock(&m.mu)
	size := m.size
	unlock(&m.mu)
	return size
}

// JITModuleOwner is the GC-visible lifetime token for one generated module.
// Generated entry points and function literals retain this value in typed
// stack or closure slots. Its fields are private because only the runtime may
// retire the associated mapping and metadata.
type JITModuleOwner struct {
	mapping         *JITMapping
	module          uintptr
	control         uintptr
	generation      uint64
	captures        []JITCaptureBinding
	globals         []jitGlobalRoot
	typeMap         map[*_type]*_type
	typeOwners      []*JITModuleOwner // dependencies for canonical descriptors and direct type references
	typeDescriptors []uintptr         // descriptors considered by future JIT interning
	weakHandle      unsafe.Pointer
	pinSlot         *jitOwnerRootSlot
	itabLock        mutex
	itabs           *jitDynamicItab
	itabCaches      uint32
}

// JITRegisterReflectType installs GC-owned metadata for a reflect-created
// descriptor. Its local offset table and all source module owners are reached
// only while the descriptor is live.
func JITRegisterReflectType(typ unsafe.Pointer, dependencies []unsafe.Pointer, offsets []JITReflectOffset) {
	if typ == nil {
		return
	}
	metadata := abi.Escape(&jitHeapObjectMetadata{typ: (*_type)(typ)})
	metadata.identity = atomic.Xadd64(&jitReflectTypeIdentity, 1)
	if metadata.identity == 0 {
		throw("runtime: JIT reflection type identity exhausted")
	}
	metadata.offsets = append(metadata.offsets, offsets...)
	missingOwner := false
	for _, candidate := range dependencies {
		if candidate == nil || !jitTypeNeedsOwner((*_type)(candidate)) {
			continue
		}
		missingOwner = jitAddReflectTypeOwners(metadata, (*_type)(candidate)) || missingOwner
	}
	for _, offset := range offsets {
		if offset.Target == nil {
			continue
		}
		if owner, found := jitOwnerForMappedAddress(uintptr(offset.Target)); found {
			if owner == nil {
				throw("runtime: JIT reflection offset refers to a retired module")
			}
			metadata.addOwner(owner)
		}
	}
	if len(metadata.owners) != 0 {
		(*_type)(typ).TFlag |= abi.TFlagJITDependent
	}
	if len(metadata.owners) == 0 {
		if owner, _ := jitOwnerForType((*_type)(typ)); owner != nil {
			metadata.addOwner(owner)
		}
	}
	if _, dependent := jitOwnerForType((*_type)(typ)); dependent && (len(metadata.owners) == 0 || missingOwner) {
		// Corrupt or incomplete dependency metadata must not leave raw PCs
		// pointing into a reclaimable mapping. Pin every module as a safe,
		// intentionally permanent fallback.
		pinAllJITModules()
	}
	if !jitAssociateHeapMetadataObject(typ, metadata) {
		// The side table is a correctness boundary. If it cannot be installed,
		// retain the descriptor and its metadata permanently and pin every
		// referenced module rather than return a descriptor with dangling PCs.
		jitPinReflectMetadata(metadata)
	}
	KeepAlive(dependencies)
	KeepAlive(offsets)
}

// jitCanonicalReflectType finds an already published unnamed type with the
// same runtime descriptor shape as typ. The caller holds jitTypeIdentityLock,
// so no JIT image can be published while this candidate is being checked.
func jitCanonicalReflectType(typ *_type) *_type {
	if typ == nil || typ.TFlag&abi.TFlagNamed != 0 {
		return nil
	}
	seen := make(map[_typePair]struct{})
	for _, md := range activeModules() {
		for _, candidate := range moduleTypelinks(md) {
			if canonical := md.typemap[candidate]; canonical != nil {
				candidate = canonical
			}
			if typ.Hash == candidate.Hash && jitTypesEqual(typ, candidate, seen, nil) {
				return candidate
			}
			clear(seen)
		}
	}
	owners := jitLiveModuleOwners()
	for _, owner := range owners {
		md := (*moduledata)(unsafe.Pointer(owner.module))
		if md == nil || !md.jitModule {
			continue
		}
		for _, address := range owner.typeDescriptors {
			candidate := (*_type)(unsafe.Pointer(address))
			if canonical := md.typemap[candidate]; canonical != nil {
				candidate = canonical
			}
			if typ.Hash == candidate.Hash && jitTypesEqual(typ, candidate, seen, nil) {
				KeepAlive(owners)
				return candidate
			}
			clear(seen)
		}
	}
	KeepAlive(owners)
	return nil
}

// JITReflectTypeCache looks up or stores a reflect-created type descriptor in
// a process-wide weak-value cache. keys contains the complete structural
// identity chosen by reflect; it does not keep any input descriptors alive.
// For a new candidate, the identity lock coordinates it with JIT publication
// and an already published static or JIT descriptor takes precedence.
// Passing candidate == nil does
// a lookup. Passing a candidate stores it unless another goroutine has already
// stored the same key, in which case the existing canonical descriptor wins.
// The weak value does not keep the descriptor's module owners alive.
func JITReflectTypeCache(kind uint8, keys []byte, dependencies []unsafe.Pointer, candidate unsafe.Pointer) unsafe.Pointer {
	owners := make([]*JITModuleOwner, 0, len(dependencies))
	for _, dependency := range dependencies {
		if dependency == nil || !jitTypeNeedsOwner((*_type)(dependency)) {
			continue
		}
		dependencyOwners, _ := jitOwnersForType((*_type)(dependency))
		if len(dependencyOwners) == 0 {
			// An incomplete descriptor edge cannot be represented by this
			// canonicalizer. Preserve safety by pinning before returning it.
			pinAllJITModules()
			continue
		}
		for _, owner := range dependencyOwners {
			found := false
			for _, existing := range owners {
				if existing == owner {
					found = true
					break
				}
			}
			if !found {
				owners = append(owners, owner)
			}
		}
	}
	lock(&jitTypeIdentityLock)
	defer unlock(&jitTypeIdentityLock)

	keyBytes := make([]byte, len(keys)+1)
	keyBytes[0] = kind
	copy(keyBytes[1:], keys)
	key := string(keyBytes)
	for {
		lock(&jitReflectTypeLock)
		handle := jitReflectTypes[key]
		unlock(&jitReflectTypeLock)
		if handle != nil {
			if typ := internal_weak_runtime_makeStrongFromWeak(handle); typ != nil {
				KeepAlive(owners)
				KeepAlive(dependencies)
				return typ
			}
			jitDeleteReflectTypeCacheEntry(key, handle)
			if candidate == nil {
				KeepAlive(owners)
				KeepAlive(dependencies)
				return nil
			}
			continue
		}
		if candidate == nil {
			KeepAlive(owners)
			KeepAlive(dependencies)
			return nil
		}
		if canonical := jitCanonicalReflectType((*_type)(candidate)); canonical != nil {
			KeepAlive(candidate)
			KeepAlive(owners)
			KeepAlive(dependencies)
			return unsafe.Pointer(canonical)
		}

		// The weak handle does not root the descriptor or any of its module
		// owners. A live descriptor carries its own owner metadata.
		weak := internal_weak_runtime_registerWeakPointer(candidate)
		if weak == nil {
			KeepAlive(candidate)
			KeepAlive(owners)
			KeepAlive(dependencies)
			return candidate
		}
		lock(&jitReflectTypeLock)
		if jitReflectTypes == nil {
			jitReflectTypes = make(map[string]unsafe.Pointer)
		}
		if jitReflectTypes[key] == nil {
			jitReflectTypes[key] = weak
			entryBytes := uint64(len(key)) + jitReflectTypeCacheEntryBytes
			previousBytes := jitReflectTypeBytes
			if jitReflectTypeBytes > ^uint64(0)-entryBytes {
				unlock(&jitReflectTypeLock)
				throw("runtime: JIT reflect type cache size overflow")
			}
			jitReflectTypeBytes += entryBytes
			prune := len(jitReflectTypes)%64 == 0 ||
				previousBytes/jitReflectTypeCachePruneBytes < jitReflectTypeBytes/jitReflectTypeCachePruneBytes
			unlock(&jitReflectTypeLock)
			if prune {
				jitPruneReflectTypeCache(false)
			}
			KeepAlive(candidate)
			KeepAlive(owners)
			KeepAlive(dependencies)
			return candidate
		}
		unlock(&jitReflectTypeLock)
	}
}

// JITPruneCaches removes weak cache keys whose reflected type or itab has been
// collected. Reclaim forces a complete pass; background sweep pruning checks
// the global structural cache after its entry-count or estimated-byte
// threshold is reached and rebuilds its map after removing dead keys.
func JITPruneCaches() {
	jitPruneModuleCaches(true)
	jitPruneReflectTypeCache(true)
}

func jitPruneReflectTypeCache(force bool) {
	lock(&jitReflectTypeLock)
	if !force && len(jitReflectTypes) < 64 && jitReflectTypeBytes < jitReflectTypeCachePruneBytes {
		unlock(&jitReflectTypeLock)
		return
	}
	entries := make([]jitReflectTypeCacheEntry, 0, len(jitReflectTypes))
	for key, weak := range jitReflectTypes {
		entries = append(entries, jitReflectTypeCacheEntry{key: key, weak: weak})
	}
	unlock(&jitReflectTypeLock)
	var dead []jitReflectTypeCacheEntry
	for _, entry := range entries {
		typ := internal_weak_runtime_makeStrongFromWeak(entry.weak)
		if typ == nil {
			dead = append(dead, entry)
		}
		KeepAlive(typ)
	}
	if len(dead) == 0 {
		return
	}
	// Allocate the replacement table before taking the runtime mutex. Size it
	// for the snapshot's live entries, not the pre-prune peak. Concurrent
	// insertions are checked below; if they exceed this hint, keep the current
	// table and recompute its accounting instead.
	compactHint := len(entries) - len(dead)
	if compactHint < 0 {
		compactHint = 0
	}
	compacted := make(map[string]unsafe.Pointer, compactHint)
	lock(&jitReflectTypeLock)
	removed := false
	for _, entry := range dead {
		if jitReflectTypes[entry.key] == entry.weak {
			delete(jitReflectTypes, entry.key)
			removed = true
		}
	}
	if removed {
		// Go maps retain their peak bucket array after deletion. Rebuild the
		// weak cache after pruning so a burst of short-lived JIT type shapes
		// does not leave that peak table resident for process life.
		if len(jitReflectTypes) == 0 {
			jitReflectTypes = nil
			jitReflectTypeBytes = 0
		} else {
			var bytes uint64
			for key := range jitReflectTypes {
				entryBytes := uint64(len(key)) + jitReflectTypeCacheEntryBytes
				if bytes > ^uint64(0)-entryBytes {
					unlock(&jitReflectTypeLock)
					throw("runtime: JIT reflect type cache size overflow")
				}
				bytes += entryBytes
			}
			jitReflectTypeBytes = bytes
			if len(jitReflectTypes) <= compactHint {
				for key, weak := range jitReflectTypes {
					compacted[key] = weak
				}
				jitReflectTypes = compacted
			}
		}
	}
	unlock(&jitReflectTypeLock)
}

func jitDeleteReflectTypeCacheEntry(key string, handle unsafe.Pointer) {
	lock(&jitReflectTypeLock)
	if jitReflectTypes[key] == handle {
		delete(jitReflectTypes, key)
		entryBytes := uint64(len(key)) + jitReflectTypeCacheEntryBytes
		if jitReflectTypeBytes < entryBytes {
			throw("runtime: JIT reflect type cache accounting underflow")
		}
		jitReflectTypeBytes -= entryBytes
		if len(jitReflectTypes) == 0 {
			jitReflectTypes = nil
			jitReflectTypeBytes = 0
		}
	}
	unlock(&jitReflectTypeLock)
}

type jitReflectTypeCacheEntry struct {
	key  string
	weak unsafe.Pointer
}

func jitPruneModuleCaches(all bool) {
	index := jitIndexReadEnter()
	if jitIndexPointer(index) == 0 {
		jitIndexReadLeave(index)
		if !all {
			jitPruneReflectTypeCache(false)
		}
		return
	}
	entries := jitIndexEntries((*jitModuleIndex)(unsafe.Pointer(jitIndexPointer(index))))
	owners := make([]*JITModuleOwner, 0, len(entries))
	for i := range entries {
		entry := &entries[i]
		if atomic.Loaduintptr(&entry.owner) == 0 || entry.weakHandle == 0 {
			continue
		}
		owner := (*JITModuleOwner)(internal_weak_runtime_makeStrongFromWeak(unsafe.Pointer(entry.weakHandle)))
		if owner != nil {
			owners = append(owners, owner)
		}
	}
	jitIndexReadLeave(index)
	for _, owner := range owners {
		jitPruneOwnerCaches(owner, all)
	}
	KeepAlive(owners)
	if !all {
		jitPruneReflectTypeCache(false)
	}
}

func jitPruneOwnerCaches(owner *JITModuleOwner, all bool) {
	if owner == nil {
		return
	}
	lock(&owner.itabLock)
	if all || owner.itabCaches >= 64 {
		itabEntries := make([]*jitDynamicItab, 0, owner.itabCaches)
		for entry := owner.itabs; entry != nil; entry = entry.next {
			itabEntries = append(itabEntries, entry)
		}
		owner.itabCaches = 0
		unlock(&owner.itabLock)
		for _, entry := range itabEntries {
			tab := (*itab)(internal_weak_runtime_makeStrongFromWeak(entry.weak))
			if tab == nil {
				lock(&owner.itabLock)
				owner.itabs = removeJITDynamicItab(owner.itabs, entry)
				unlock(&owner.itabLock)
			}
			KeepAlive(tab)
		}
		lock(&owner.itabLock)
		owner.itabCaches = 0
		for entry := owner.itabs; entry != nil; entry = entry.next {
			owner.itabCaches++
		}
		unlock(&owner.itabLock)
		KeepAlive(itabEntries)
		KeepAlive(owner)
		return
	}
	unlock(&owner.itabLock)
	KeepAlive(owner)
}

// jitManifestBytes is initialized by cmd/link from the symbols retained for
// JIT-visible packages. Its backing storage is immutable read-only linker
// data; the linker fills this slice descriptor with ordinary relocations.
var jitManifestBytes []byte

// JITManifest returns the immutable package and symbol manifest emitted by
// cmd/link for executables that import jit. The returned bytes are backed by
// the executable's read-only data segment and remain valid for process life.
func JITManifest() []byte {
	// The linker places the backing bytes in read-only data. Returning the
	// linker-owned slice directly would make a caller's ordinary []byte write
	// fault the process, and would expose global metadata to accidental
	// mutation. The manifest is small and normally parsed once at startup, so
	// return a caller-owned copy.
	return append([]byte(nil), jitManifestBytes...)
}

// RegisterJITModule publishes generated Go function metadata in the separate
// JIT PC index. The executable reservation must start with a writable,
// non-executable control page. The returned owner is the GC-visible lifetime
// edge for generated frames and closures; dropping the last edge schedules
// asynchronous retirement. Each capture couples a call-site binding ID with
// its address of a typed Go variable; these records are copied into a scanned
// owner field before the module becomes visible.
// typeDescriptors is a pointer-free list of emitted descriptors for later
// unnamed-type comparisons; it does not keep this module alive.
func RegisterJITModule(guard *JITTypeIdentityGuard, textStart, textSize, rodataStart, typesStart, typesSize uintptr, mapping *JITMapping, controlPage uintptr, functions []JITFunctionMetadata, globals []JITGlobalMetadata, typeMappings []JITTypeMapping, typeDescriptors []uintptr, captures []JITCaptureBinding) (*JITModuleOwner, error) {
	if !jitTypeIdentityGuardHeld(guard) {
		return nil, JITModuleError("runtime: JIT module registration requires a type identity guard")
	}
	if !jitPlatformSupported() {
		return nil, JITModuleError("runtime: JIT module registration is unavailable on this platform")
	}
	if textStart == 0 || textSize == 0 || textStart+textSize < textStart || textSize > uintptr(^uint32(0)) {
		return nil, JITModuleError("runtime: invalid JIT text range")
	}
	if mapping == nil || controlPage == 0 || controlPage != mapping.Base() || controlPage%physPageSize != 0 || mapping.Size() < physPageSize {
		return nil, JITModuleError("runtime: invalid JIT owner mapping or control page")
	}
	mapBase, mapSize := mapping.Base(), mapping.Size()
	if mapBase+mapSize < mapBase || textStart < mapBase+physPageSize || textStart+textSize < textStart || textStart+textSize > mapBase+mapSize {
		return nil, JITModuleError("runtime: JIT text is outside its owner mapping")
	}
	if rodataStart != 0 && (rodataStart < mapBase+physPageSize || rodataStart >= mapBase+mapSize) {
		return nil, JITModuleError("runtime: JIT read-only data is outside its owner mapping")
	}
	if (typesStart == 0) != (typesSize == 0) || typesStart+typesSize < typesStart ||
		(typesSize != 0 && (typesStart < mapBase+physPageSize || typesStart+typesSize > mapBase+mapSize)) {
		return nil, JITModuleError("runtime: JIT type metadata is outside its owner mapping")
	}
	if typesSize != 0 && rodataStart != typesStart {
		return nil, JITModuleError("runtime: JIT type metadata must begin at the read-only data base")
	}
	seenTypeDescriptors := make(map[uintptr]struct{}, len(typeDescriptors))
	for _, descriptor := range typeDescriptors {
		if descriptor%goarch.PtrSize != 0 || descriptor < typesStart || descriptor > typesStart+typesSize ||
			typesStart+typesSize-descriptor < unsafe.Sizeof(_type{}) {
			return nil, JITModuleError("runtime: JIT type descriptor is outside the module type range")
		}
		if _, duplicate := seenTypeDescriptors[descriptor]; duplicate {
			return nil, JITModuleError("runtime: duplicate JIT type descriptor")
		}
		seenTypeDescriptors[descriptor] = struct{}{}
	}
	if len(functions) == 0 || len(functions) > 1<<20 {
		return nil, JITModuleError("runtime: JIT module function count is invalid")
	}
	if len(globals) > 1<<20 {
		return nil, JITModuleError("runtime: too many JIT global roots")
	}
	seenCaptureIDs := make(map[uint64]struct{}, len(captures))
	for _, capture := range captures {
		if capture.BindingID == 0 {
			return nil, JITModuleError("runtime: JIT capture has no binding identity")
		}
		if capture.Address == nil {
			return nil, JITModuleError("runtime: JIT capture has no storage address")
		}
		if capture.Type == nil || uintptr(capture.Type)%goarch.PtrSize != 0 {
			return nil, JITModuleError("runtime: JIT capture has no aligned type descriptor")
		}
		if JITCaptureAddressOnCurrentStack(capture.Address) {
			return nil, JITModuleError("runtime: JIT capture points into the current goroutine stack")
		}
		if _, duplicate := seenCaptureIDs[capture.BindingID]; duplicate {
			return nil, JITModuleError("runtime: duplicate JIT capture binding identity")
		}
		seenCaptureIDs[capture.BindingID] = struct{}{}
	}
	for i, fn := range functions {
		if fn.Size == 0 || uint64(fn.Offset)+uint64(fn.Size) > uint64(textSize) || fn.Name == "" || len(fn.PCSP) == 0 {
			return nil, JITModuleError("runtime: invalid JIT function metadata")
		}
		if i != 0 {
			previous := functions[i-1]
			if previous.Offset >= fn.Offset || uint64(previous.Offset)+uint64(previous.Size) > uint64(fn.Offset) {
				return nil, JITModuleError("runtime: JIT function ranges overlap or are out of order")
			}
		}
		if len(fn.PCData) < 2 || len(fn.FuncData) < 2 {
			return nil, JITModuleError("runtime: JIT function is missing GC metadata")
		}
		if len(fn.PCFile) != 0 && !jitPCFileTableValid(fn.PCFile, fn.Size, fn.Files) {
			return nil, JITModuleError("runtime: JIT source table contains an invalid CU file index")
		}
	}
	if functions[0].Offset != 0 {
		return nil, JITModuleError("runtime: JIT function table does not start at text offset zero")
	}
	if !jitFindFuncTableFits(functions, textSize) {
		return nil, JITModuleError("runtime: too many functions in a JIT findfunc bucket")
	}
	globalRoots := make([]jitGlobalRoot, 0, len(globals))
	for _, global := range globals {
		if global.Address == 0 || global.Address%goarch.PtrSize != 0 || global.Size == 0 || global.Address+global.Size < global.Address ||
			global.Address < mapBase+physPageSize || global.Address+global.Size > mapBase+mapSize || global.Type == nil {
			return nil, JITModuleError("runtime: invalid JIT global root metadata")
		}
		typ := (*_type)(global.Type)
		if uintptr(typ.Size_) != global.Size || uintptr(typ.PtrBytes) > global.Size ||
			typ.PtrBytes == 0 || uintptr(typ.PtrBytes)%goarch.PtrSize != 0 {
			return nil, JITModuleError("runtime: JIT global type does not match its storage")
		}
		if typ.TFlag&abi.TFlagGCMaskOnDemand != 0 {
			return nil, JITModuleError("runtime: JIT global uses an on-demand GC type map")
		}
		mask := typ.GCData
		if mask == nil {
			return nil, JITModuleError("runtime: JIT global type has no GC pointer map")
		}
		globalRoots = append(globalRoots, jitGlobalRoot{address: global.Address, ptrBytes: uintptr(typ.PtrBytes), mask: unsafe.Pointer(mask)})
	}
	typeMap := make(map[*_type]*_type, len(typeMappings))
	var typeOwners []*JITModuleOwner
	var typeOwnerSet map[*JITModuleOwner]struct{}
	addTypeOwners := func(owners []*JITModuleOwner) {
		for _, owner := range owners {
			if owner == nil {
				continue
			}
			if typeOwnerSet == nil {
				typeOwnerSet = make(map[*JITModuleOwner]struct{})
			}
			if _, seen := typeOwnerSet[owner]; !seen {
				typeOwnerSet[owner] = struct{}{}
				typeOwners = append(typeOwners, owner)
			}
		}
	}
	descriptorSize := unsafe.Sizeof(_type{})
	stagedTypeModule := &moduledata{
		types: typesStart, etypes: typesStart + typesSize,
		jitModule: true, typemap: typeMap,
	}
	seenTypePairs := make(map[_typePair]struct{})
	for _, pair := range typeMappings {
		if pair.Dynamic%goarch.PtrSize != 0 || pair.Dynamic < typesStart || pair.Dynamic > typesStart+typesSize ||
			typesStart+typesSize-pair.Dynamic < descriptorSize || pair.Canonical == 0 || pair.Canonical%goarch.PtrSize != 0 {
			return nil, JITModuleError("runtime: JIT type mapping is outside the module type range")
		}
		var canonicalModule *moduledata
		var canonicalOwners []*JITModuleOwner
		canonicalInHeap := false
		if pair.Canonical >= typesStart && pair.Canonical < typesStart+typesSize {
			canonicalModule = stagedTypeModule
		} else {
			for _, md := range activeModules() {
				if pair.Canonical >= md.types && pair.Canonical < md.etypes {
					canonicalModule = md
					break
				}
			}
		}
		if canonicalModule == nil {
			// Cross-module aliases must retain the canonical module. Promote its
			// weak owner before inspecting moduledata, then keep that owner on the
			// new module so the descriptor cannot be unmapped while this alias is
			// reachable. Because the target must already be published, these
			// dependency edges point to earlier registrations and cannot cycle.
			canonicalOwner, found := jitOwnerForMappedAddress(pair.Canonical)
			if found && canonicalOwner == nil {
				return nil, JITModuleError("runtime: canonical JIT type module is retiring")
			}
			if canonicalOwner != nil {
				candidate := (*moduledata)(unsafe.Pointer(canonicalOwner.module))
				if candidate != nil && candidate.jitModule && pair.Canonical >= candidate.types && pair.Canonical < candidate.etypes {
					canonicalModule = candidate
					canonicalOwners = append(canonicalOwners, canonicalOwner)
				}
			}
		}
		if canonicalModule == nil && len(canonicalOwners) == 0 {
			// reflect can synthesize a canonical descriptor whose storage is an
			// ordinary Go heap object rather than a module type range. Keep that
			// descriptor reachable through typeMap and retain its full owner set
			// when it was itself built from JIT types.
			canonicalType := (*_type)(unsafe.Pointer(pair.Canonical))
			obj, span, _ := findObject(pair.Canonical, 0, 0)
			if obj == pair.Canonical && span != nil && span.elemsize >= descriptorSize {
				canonicalInHeap = true
				if metadata := jitMetadataForHeapObject(pair.Canonical); metadata != nil {
					if len(metadata.owners) == 0 && jitTypeNeedsOwner(canonicalType) {
						return nil, JITModuleError("runtime: canonical reflected JIT type has incomplete owner metadata")
					}
					canonicalOwners = append(canonicalOwners, metadata.owners...)
					KeepAlive(metadata)
				} else if jitTypeNeedsOwner(canonicalType) {
					owners, dependent := jitOwnersForType(canonicalType)
					if !dependent || len(owners) == 0 {
						return nil, JITModuleError("runtime: canonical reflected JIT type has incomplete owner metadata")
					}
					canonicalOwners = append(canonicalOwners, owners...)
				}
			}
		}
		if canonicalModule == nil && !canonicalInHeap {
			return nil, JITModuleError("runtime: canonical type is not in a linked module or a reflected heap descriptor")
		}
		if canonicalModule != nil {
			if pair.Canonical < canonicalModule.types || pair.Canonical > canonicalModule.etypes ||
				canonicalModule.etypes-pair.Canonical < descriptorSize {
				return nil, JITModuleError("runtime: canonical JIT type descriptor is truncated")
			}
		}
		dynamicType := (*_type)(unsafe.Pointer(pair.Dynamic))
		canonicalType := (*_type)(unsafe.Pointer(pair.Canonical))
		if dynamicType.Hash != canonicalType.Hash {
			KeepAlive(pair.owner)
			KeepAlive(pair.heapType)
			return nil, JITModuleError("runtime: canonical JIT type hash mismatch")
		}
		equal := jitTypesEqual(dynamicType, canonicalType, seenTypePairs, stagedTypeModule)
		KeepAlive(pair.owner)
		KeepAlive(pair.heapType)
		if !equal {
			return nil, JITModuleError("runtime: canonical JIT type is not structurally identical")
		}
		clear(seenTypePairs)
		if _, exists := typeMap[dynamicType]; exists {
			return nil, JITModuleError("runtime: duplicate local JIT type mapping")
		}
		typeMap[dynamicType] = canonicalType
		addTypeOwners(canonicalOwners)
	}
	// A capture's static type descriptor can itself live in an unloadable JIT
	// module, or be a reflect-created structural type that depends on several
	// modules. Carry those dependencies into the new owner before publication.
	for _, capture := range captures {
		owners, dependent := jitOwnersForType((*_type)(capture.Type))
		if dependent && len(owners) == 0 {
			return nil, JITModuleError("runtime: captured JIT type has incomplete owner metadata")
		}
		addTypeOwners(owners)
	}

	// Compute one non-pointer allocation for moduledata and every table it
	// references. This mirrors the linker's moduledata lifetime and lets unload
	// release all runtime-owned metadata together.
	headerSize := unsafe.Sizeof(pcHeader{})
	mdSize := unsafe.Sizeof(moduledata{})
	nameSize, pcTabSize, fileTabSize, cutabEntries, gofuncSize := uintptr(1), uintptr(1), uintptr(1), uintptr(0), uintptr(0)
	fileCount := uint64(0)
	pcLnSize := uintptr(len(functions))*8 + 4
	for _, fn := range functions {
		var ok bool
		nameSize, ok = addJITMetadataSize(nameSize, uintptr(len(fn.Name))+1)
		if !ok {
			return nil, JITModuleError("runtime: JIT function names exceed size limit")
		}
		pcTabSize, ok = addJITMetadataSize(pcTabSize, uintptr(len(fn.PCSP)))
		if !ok {
			return nil, JITModuleError("runtime: JIT PC tables exceed size limit")
		}
		if (len(fn.PCFile) == 0) != (len(fn.PCLine) == 0) || (len(fn.PCFile) != 0 && len(fn.Files) == 0) {
			return nil, JITModuleError("runtime: incomplete JIT source metadata")
		}
		pcTabSize, ok = addJITMetadataSize(pcTabSize, uintptr(len(fn.PCFile)))
		if ok {
			pcTabSize, ok = addJITMetadataSize(pcTabSize, uintptr(len(fn.PCLine)))
		}
		if !ok {
			return nil, JITModuleError("runtime: JIT source tables exceed size limit")
		}
		if uintptr(len(fn.Files)) > ^uintptr(0)-cutabEntries {
			return nil, JITModuleError("runtime: JIT source files exceed size limit")
		}
		cutabEntries += uintptr(len(fn.Files))
		for _, file := range fn.Files {
			if file == "" {
				continue // Preserve the CU index as an unmapped cutab entry.
			}
			for i := 0; i < len(file); i++ {
				if file[i] == 0 {
					return nil, JITModuleError("runtime: invalid JIT source file")
				}
			}
			fileTabSize, ok = addJITMetadataSize(fileTabSize, uintptr(len(file))+1)
			if !ok {
				return nil, JITModuleError("runtime: JIT source files exceed size limit")
			}
			fileCount++
		}
		for _, table := range fn.PCData {
			pcTabSize, ok = addJITMetadataSize(pcTabSize, uintptr(len(table)))
			if !ok {
				return nil, JITModuleError("runtime: JIT PC tables exceed size limit")
			}
		}
		pcLnSize, ok = alignJITMetadataSize(pcLnSize, goarch.PtrSize)
		if ok {
			pcLnSize, ok = addJITMetadataSize(pcLnSize, 44)
		}
		pcdataSize, pcdataOK := mulJITMetadataSize(uintptr(len(fn.PCData)), 4)
		funcdataSize, funcdataOK := mulJITMetadataSize(uintptr(len(fn.FuncData)), 4)
		if ok && pcdataOK {
			pcLnSize, ok = addJITMetadataSize(pcLnSize, pcdataSize)
		}
		if ok {
			pcLnSize, ok = addJITMetadataSize(pcLnSize, funcdataSize)
		}
		if !ok || !funcdataOK {
			return nil, JITModuleError("runtime: JIT function tables exceed size limit")
		}
		for _, data := range fn.FuncData {
			if len(data) != 0 {
				gofuncSize, ok = alignJITMetadataSize(gofuncSize, goarch.PtrSize)
				if ok {
					gofuncSize, ok = addJITMetadataSize(gofuncSize, uintptr(len(data)))
				}
				if !ok {
					return nil, JITModuleError("runtime: JIT funcdata exceeds size limit")
				}
			}
		}
	}
	if fileCount > uint64(^uint32(0)) || cutabEntries > uintptr(^uint32(0)) {
		return nil, JITModuleError("runtime: too many JIT source files")
	}
	cutabStorageEntries := cutabEntries
	if cutabStorageEntries == 0 {
		cutabStorageEntries = 1
	}
	var sizeOK bool
	pcLnSize, sizeOK = alignJITMetadataSize(pcLnSize, goarch.PtrSize)
	if !sizeOK {
		return nil, JITModuleError("runtime: JIT function tables exceed size limit")
	}
	bucketCount := (textSize + abi.FuncTabBucketSize - 1) / abi.FuncTabBucketSize
	if bucketCount == 0 {
		bucketCount = 1
	}
	findFuncSize, sizeOK := mulJITMetadataSize(bucketCount, uintptr(unsafe.Sizeof(findfuncbucket{})))
	if !sizeOK {
		return nil, JITModuleError("runtime: JIT function index exceeds size limit")
	}
	cutabSize, sizeOK := mulJITMetadataSize(cutabStorageEntries, 4)
	if !sizeOK {
		return nil, JITModuleError("runtime: JIT source table exceeds size limit")
	}

	total := mdSize
	for _, part := range []uintptr{headerSize, nameSize, cutabSize, fileTabSize, pcTabSize, gofuncSize, pcLnSize, findFuncSize} {
		total, sizeOK = alignJITMetadataSize(total, goarch.PtrSize)
		if sizeOK {
			total, sizeOK = addJITMetadataSize(total, part)
		}
		if !sizeOK {
			return nil, JITModuleError("runtime: JIT runtime tables exceed size limit")
		}
	}
	if total > 256<<20 {
		return nil, JITModuleError("runtime: JIT runtime tables exceed size limit")
	}
	if err := reserveJITMemory(total); err != nil {
		return nil, err
	}
	base := sysAlloc(total, &memstats.other_sys, "jit module metadata")
	if base == nil {
		releaseJITMemory(total)
		return nil, JITModuleError("runtime: cannot allocate JIT runtime metadata")
	}
	memclrNoHeapPointers(base, total)

	cur := uintptr(base)
	md := (*moduledata)(unsafe.Pointer(cur))
	cur += mdSize
	cur = alignJIT(cur, goarch.PtrSize)
	hdr := (*pcHeader)(unsafe.Pointer(cur))
	cur += headerSize
	allocBytes := func(n uintptr) []byte {
		if n == 0 {
			return nil
		}
		p := unsafe.Pointer(cur)
		cur += n
		return unsafe.Slice((*byte)(p), int(n))
	}
	allocAlignedBytes := func(n uintptr) []byte {
		cur = alignJIT(cur, goarch.PtrSize)
		return allocBytes(n)
	}
	funcnames := allocAlignedBytes(nameSize)
	cur = alignJIT(cur, goarch.PtrSize)
	cutabStorage := unsafe.Slice((*uint32)(unsafe.Pointer(cur)), int(cutabStorageEntries))
	cur += cutabStorageEntries * 4
	cutab := cutabStorage[:int(cutabEntries)]
	filetab := allocAlignedBytes(fileTabSize)
	pctab := allocAlignedBytes(pcTabSize)
	gofunc := allocAlignedBytes(gofuncSize)
	pclntab := allocAlignedBytes(pcLnSize)
	findfunctab := allocAlignedBytes(findFuncSize)

	// Build names, PC tables, function records, and funcdata offsets.
	nameOffs := make([]uint32, len(functions))
	pcspOffs := make([]uint32, len(functions))
	pcfileOffs := make([]uint32, len(functions))
	pclineOffs := make([]uint32, len(functions))
	pcdataOffs := make([][]uint32, len(functions))
	gofuncOffs := make([][]uint32, len(functions))
	cuOffsets := make([]uint32, len(functions))
	nameAt, pcAt, fileAt, cutabAt, gofuncAt := uintptr(0), uintptr(1), uintptr(0), uintptr(0), uintptr(0)
	for i, fn := range functions {
		nameOffs[i] = uint32(nameAt)
		copy(funcnames[nameAt:], fn.Name)
		nameAt += uintptr(len(fn.Name) + 1)
		pcspOffs[i] = uint32(pcAt)
		copy(pctab[pcAt:], fn.PCSP)
		pcAt += uintptr(len(fn.PCSP))
		if len(fn.PCFile) != 0 {
			pcfileOffs[i] = uint32(pcAt)
			copy(pctab[pcAt:], fn.PCFile)
			pcAt += uintptr(len(fn.PCFile))
			pclineOffs[i] = uint32(pcAt)
			copy(pctab[pcAt:], fn.PCLine)
			pcAt += uintptr(len(fn.PCLine))
		}
		if len(fn.Files) != 0 {
			cuOffsets[i] = uint32(cutabAt)
			for _, file := range fn.Files {
				if file == "" {
					cutab[cutabAt] = ^uint32(0)
					cutabAt++
					continue
				}
				cutab[cutabAt] = uint32(fileAt)
				copy(filetab[fileAt:], file)
				fileAt += uintptr(len(file) + 1)
				cutabAt++
			}
		} else {
			cuOffsets[i] = ^uint32(0)
		}
		pcdataOffs[i] = make([]uint32, len(fn.PCData))
		for j, table := range fn.PCData {
			pcdataOffs[i][j] = uint32(pcAt)
			copy(pctab[pcAt:], table)
			pcAt += uintptr(len(table))
		}
		gofuncOffs[i] = make([]uint32, len(fn.FuncData))
		for j, data := range fn.FuncData {
			if len(data) == 0 {
				gofuncOffs[i][j] = ^uint32(0)
				continue
			}
			gofuncAt = alignJIT(gofuncAt, goarch.PtrSize)
			gofuncOffs[i][j] = uint32(gofuncAt)
			copy(gofunc[gofuncAt:], data)
			gofuncAt += uintptr(len(data))
		}
	}

	// The ftab occupies 8 bytes per function plus a final 4-byte end PC. Each
	// _func record starts at pointer alignment after this table.
	funcOffsets := make([]uint32, len(functions))
	pcAt = uintptr(len(functions))*8 + 4
	for i, fn := range functions {
		pcAt = alignJIT(pcAt, goarch.PtrSize)
		funcOffsets[i] = uint32(pcAt)
		pcAt += 44 + uintptr(len(fn.PCData)+len(fn.FuncData))*4
	}
	for i, fn := range functions {
		baseOff := uintptr(i) * 8
		*(*uint32)(unsafe.Pointer(&pclntab[baseOff])) = fn.Offset
		*(*uint32)(unsafe.Pointer(&pclntab[baseOff+4])) = funcOffsets[i]
	}
	*(*uint32)(unsafe.Pointer(&pclntab[uintptr(len(functions))*8])) = uint32(textSize)

	for i, fn := range functions {
		off := uintptr(funcOffsets[i])
		put32 := func(at uintptr, value uint32) { *(*uint32)(unsafe.Pointer(&pclntab[at])) = value }
		put32(off, fn.Offset)
		put32(off+4, nameOffs[i])
		put32(off+8, fn.ArgsSize)
		put32(off+12, 0) // deferreturn
		put32(off+16, pcspOffs[i])
		put32(off+20, pcfileOffs[i])
		put32(off+24, pclineOffs[i])
		put32(off+28, uint32(len(fn.PCData)))
		put32(off+32, cuOffsets[i])
		put32(off+36, uint32(fn.StartLine))
		pclntab[off+40] = fn.FuncID
		pclntab[off+41] = fn.FuncFlag
		pclntab[off+43] = uint8(len(fn.FuncData))
		pcdataAt := off + 44
		for _, pcdataOff := range pcdataOffs[i] {
			put32(pcdataAt, pcdataOff)
			pcdataAt += 4
		}
		for _, dataOff := range gofuncOffs[i] {
			put32(pcdataAt, dataOff)
			pcdataAt += 4
		}
	}

	hdr.magic = abi.CurrentPCLnTabMagic
	hdr.minLC = sys.PCQuantum
	hdr.ptrSize = goarch.PtrSize
	hdr.nfunc = len(functions)
	hdr.nfiles = uint(fileCount)
	headerAddress := uintptr(unsafe.Pointer(hdr))
	hdr.funcnameOffset = uintptr(unsafe.Pointer(unsafe.SliceData(funcnames))) - headerAddress
	hdr.cuOffset = uintptr(unsafe.Pointer(unsafe.SliceData(cutabStorage))) - headerAddress
	hdr.filetabOffset = uintptr(unsafe.Pointer(unsafe.SliceData(filetab))) - headerAddress
	hdr.pctabOffset = uintptr(unsafe.Pointer(unsafe.SliceData(pctab))) - headerAddress
	hdr.pclnOffset = uintptr(unsafe.Pointer(unsafe.SliceData(pclntab))) - headerAddress

	md.pcHeader = hdr
	md.funcnametab = funcnames
	md.cutab = cutab
	md.filetab = filetab
	md.pctab = pctab
	md.pclntable = pclntab
	md.gofunc = uintptr(unsafe.Pointer(unsafe.SliceData(gofunc)))
	md.epclntab = uintptr(unsafe.Pointer(unsafe.SliceData(pclntab))) + uintptr(len(pclntab))
	md.ftab = unsafe.Slice((*functab)(unsafe.Pointer(unsafe.SliceData(pclntab))), len(functions)+1)
	md.findfunctab = uintptr(unsafe.Pointer(unsafe.SliceData(findfunctab)))
	md.text, md.etext = textStart, textStart+textSize
	md.minpc, md.maxpc = textStart, textStart+textSize
	md.rodata = rodataStart
	md.types, md.etypes = typesStart, typesStart+typesSize
	md.typemap = typeMap
	// Dynamic images do not publish a sorted typelinks table. Explicit aliases
	// keep their relative-offset descriptors local while canonicalizing type
	// identity on resolution.
	md.typedesclen = 0
	md.jitModule = true
	md.jitGofuncSize = gofuncSize
	md.jitAllocBase, md.jitAllocSize = base, total
	owner := &JITModuleOwner{
		mapping: mapping, module: uintptr(unsafe.Pointer(md)), control: controlPage,
		captures: append([]JITCaptureBinding(nil), captures...),
		globals:  globalRoots, typeMap: typeMap,
		typeOwners: typeOwners, typeDescriptors: append([]uintptr(nil), typeDescriptors...),
	}
	if !registerJITOwnerRoots(owner) {
		sysFree(base, total, &memstats.other_sys)
		releaseJITMemory(total)
		return nil, JITModuleError("runtime: could not register JIT owner roots")
	}
	record := (*jitRetireRecord)(unsafe.Pointer(controlPage))
	memclrNoHeapPointers(unsafe.Pointer(record), unsafe.Sizeof(*record))
	record.module = uintptr(unsafe.Pointer(md))
	record.metadataBase = uintptr(base)
	record.metadataSize = total
	record.mappingBase = mapping.Base()
	record.mappingSize = mapping.Size()
	record.weakHandle = uintptr(owner.weakHandle)
	record.pinSlot = uintptr(unsafe.Pointer(owner.pinSlot))
	owner.generation = atomic.Xadd64(&jitModuleGeneration, 1)
	if owner.generation == 0 {
		throw("runtime: JIT module generation exhausted")
	}
	record.generation = owner.generation
	buildJITFindFuncTable(findfunctab, functions, textSize)
	moduledataverify1(md)
	startJITReaper()
	if !addJITOwner(owner, controlPage) {
		removeJITOwnerRoots(record.weakHandle, record.pinSlot)
		sysFree(base, total, &memstats.other_sys)
		releaseJITMemory(total)
		return nil, JITModuleError("runtime: could not attach JIT owner cleanup record")
	}
	if err := publishJITModule(md, owner); err != nil {
		removeJITOwner(owner)
		removeJITOwnerRoots(record.weakHandle, record.pinSlot)
		sysFree(base, total, &memstats.other_sys)
		releaseJITMemory(total)
		return nil, err
	}
	jitScanNewModuleGlobals(owner)
	lock(&mapping.mu)
	mapping.owner = owner
	unlock(&mapping.mu)
	if atomic.Loadint32(&jitRawPCPins) != 0 {
		PinJITModule(owner)
	}
	return owner, nil
}

// jitScanNewModuleGlobals closes the publication race with concurrent mark.
// A newly published module may appear after a shard of jitScanGlobals has
// already passed its index range. If the collector is marking, shade the new
// owner and scan its initialized writable globals on the publishing mutator's
// work buffer. If marking starts after this check, the normal mark-done pass
// sees the already-published immutable index entry.
func jitScanNewModuleGlobals(owner *JITModuleOwner) {
	if owner == nil || len(owner.globals) == 0 || gcphase != _GCmark {
		return
	}
	mp := acquirem()
	if gcphase == _GCmark {
		gcw := &mp.p.ptr().gcw
		shade(uintptr(unsafe.Pointer(owner)))
		for _, root := range owner.globals {
			scanblock(root.address, root.ptrBytes, (*byte)(root.mask), gcw, nil)
		}
	}
	releasem(mp)
}

// JITCaptureAddressOnCurrentStack reports whether address points into the
// current goroutine's stack. The jit package uses it to verify that compiler-
// generated capture storage has escaped before an owner retains it.
// The current direct-Eval ABI only captures variables from the calling
// goroutine, so an address in that goroutine's stack can never be a valid
// long-lived capture cell.
func JITCaptureAddressOnCurrentStack(address unsafe.Pointer) bool {
	if address == nil {
		return false
	}
	gp := getg()
	if gp == nil {
		// Without a current goroutine there is no safe way to prove that the
		// address is not stack storage. Fail closed at the jit API boundary.
		return true
	}
	p := uintptr(address)
	return p >= gp.stack.lo && p < gp.stack.hi
}

// JITCaptureAddr returns the address of a lexical variable captured for one
// generated module. The owner is a scanned Go object, so its capture slice
// keeps pointer-bearing variable storage reachable for as long as generated
// code or one of its closures retains the owner.
func JITCaptureAddr(owner *JITModuleOwner, bindingID uint64) unsafe.Pointer {
	if owner == nil || bindingID == 0 {
		throw("runtime: invalid JIT capture binding identity")
	}
	for _, capture := range owner.captures {
		if capture.BindingID == bindingID {
			return capture.Address
		}
	}
	throw("runtime: unknown JIT capture binding identity")
	return nil
}

// ResolveJITFunction returns the entry PC for a linked Go function by its
// full linker symbol name. It is used only for compiler-generated runtime
// references that have no source-level import object.
func ResolveJITFunction(name string) uintptr {
	if name == "runtime.unreachableMethod" {
		return abi.FuncPCABIInternal(unreachableMethod)
	}
	for _, md := range activeModules() {
		for i := 0; i+1 < len(md.ftab); i++ {
			f := funcInfo{(*_func)(unsafe.Pointer(&md.pclntable[md.ftab[i].funcoff])), md}
			actual := funcname(f)
			if actual == name || (len(actual) > len(name) && actual[:len(name)] == name && (actual[len(name)] == '<' || actual[len(name)] == '.' || actual[len(name)] == '\u00b7')) {
				return f.entry()
			}
		}
	}
	return 0
}

// ResolveJITVariable returns the address of runtime storage exposed to the
// in-process JIT. Keep this table limited to stable runtime data that compiler
// output can reference directly; ordinary packages use build-generated link
// metadata instead.
func ResolveJITVariable(name string) uintptr {
	switch name {
	case "emptyTypeAssertCache":
		return uintptr(unsafe.Pointer(&emptyTypeAssertCache))
	case "zeroVal":
		return uintptr(unsafe.Pointer(&zeroVal[0]))
	case "staticuint64s":
		return uintptr(unsafe.Pointer(&staticuint64s[0]))
	case "writeBarrier":
		return uintptr(unsafe.Pointer(&writeBarrier))
	}
	return 0
}

func jitFindFuncTableFits(functions []JITFunctionMetadata, textSize uintptr) bool {
	if len(functions) == 0 || textSize == 0 || functions[0].Offset != 0 {
		return false
	}
	const subbucketCount = 16
	const bucketSize = uintptr(abi.FuncTabBucketSize)
	const subbucketSize = bucketSize / subbucketCount
	bucketCount := (textSize + bucketSize - 1) / bucketSize
	index := 0
	for bucket := uintptr(0); bucket < bucketCount; bucket++ {
		base := index
		for sub := uintptr(0); sub < subbucketCount; sub++ {
			pc := bucket*bucketSize + sub*subbucketSize
			for index+1 < len(functions) && uintptr(functions[index+1].Offset) <= pc {
				index++
			}
			if index-base > 255 {
				return false
			}
		}
	}
	return true
}

func buildJITFindFuncTable(table []byte, functions []JITFunctionMetadata, textSize uintptr) {
	if !jitFindFuncTableFits(functions, textSize) {
		throw("runtime: invalid JIT findfunc table input")
	}
	const subbucketCount = 16
	const bucketSize = uintptr(abi.FuncTabBucketSize)
	const subbucketSize = bucketSize / subbucketCount
	bucketCount := (textSize + bucketSize - 1) / bucketSize
	index := 0
	for bucket := uintptr(0); bucket < bucketCount; bucket++ {
		var indexes [subbucketCount]uint32
		base := uint32(index)
		for sub := uintptr(0); sub < subbucketCount; sub++ {
			pc := bucket*bucketSize + sub*subbucketSize
			for index+1 < len(functions) && uintptr(functions[index+1].Offset) <= pc {
				index++
			}
			indexes[sub] = uint32(index)
		}
		if indexes[0] != base {
			throw("runtime: JIT findfunc bucket base changed unexpectedly")
		}
		at := bucket * uintptr(unsafe.Sizeof(findfuncbucket{}))
		*(*uint32)(unsafe.Pointer(&table[at])) = indexes[0]
		for sub := uintptr(0); sub < subbucketCount; sub++ {
			if indexes[sub]-base > 255 {
				throw("runtime: JIT findfunc subbucket index overflows")
			}
			table[at+4+sub] = byte(indexes[sub] - base)
		}
	}
}

// jitPCFileTableValid checks the compiler's encoded PC-to-CU-file table before
// publishing it to traceback readers. PCFile values index the complete CU
// file table, so a sparse or compacted filename slice must not be accepted.
func jitPCFileTableValid(table []byte, functionSize uint32, files []string) bool {
	var pc uintptr
	value := int32(-1)
	first := true
	for len(table) != 0 {
		uvdelta, n, ok := jitReadPCVarint(table)
		if !ok {
			return false
		}
		table = table[n:]
		if uvdelta == 0 && !first {
			return true
		}
		value += int32(-(uvdelta & 1) ^ (uvdelta >> 1))
		pcdelta, n, ok := jitReadPCVarint(table)
		if !ok || uintptr(pcdelta) > (^uintptr(0)-pc)/sys.PCQuantum {
			return false
		}
		table = table[n:]
		pc += uintptr(pcdelta) * sys.PCQuantum
		if pc > uintptr(functionSize) || value < -1 {
			return false
		}
		if value >= 0 && (int(value) >= len(files) || files[value] == "") {
			return false
		}
		first = false
	}
	return false
}

func jitReadPCVarint(data []byte) (uint32, int, bool) {
	var value uint32
	for i, b := range data {
		if i == 5 || (i == 4 && b&0xf0 != 0) {
			return 0, 0, false
		}
		value |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, i + 1, true
		}
	}
	return 0, 0, false
}

func alignJIT(n, alignment uintptr) uintptr {
	return (n + alignment - 1) &^ (alignment - 1)
}

func addJITMetadataSize(size, add uintptr) (uintptr, bool) {
	const limit = uintptr(256 << 20)
	if size > limit || add > limit-size {
		return 0, false
	}
	return size + add, true
}

func mulJITMetadataSize(size, count uintptr) (uintptr, bool) {
	const limit = uintptr(256 << 20)
	if count != 0 && size > limit/count {
		return 0, false
	}
	return size * count, true
}

func alignJITMetadataSize(size, alignment uintptr) (uintptr, bool) {
	const limit = uintptr(256 << 20)
	if alignment == 0 || alignment&(alignment-1) != 0 || size > limit-(alignment-1) {
		return 0, false
	}
	return (size + alignment - 1) &^ (alignment - 1), true
}
