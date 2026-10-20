// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package runtime

import (
	"publicinternal/runtime/syscall/linux"
	"unsafe"
)

const (
	jitMapFixedNoReplace = 0x100000
	jitSysMunmap         = 11
)

func jitPlatformSupported() bool { return true }

// JITMemoryError reports the errno from a JIT mapping syscall. Callers may
// inspect Errno to distinguish an unavailable address from a fatal failure.
type JITMemoryError struct {
	errno uintptr
}

func (e JITMemoryError) Error() string  { return "runtime: JIT memory syscall failed" }
func (e JITMemoryError) Errno() uintptr { return e.errno }

// JITMapMemory creates a page-aligned RW/NX mapping. A nonzero hint requests
// that exact address without replacing an existing mapping. The runtime
// accounts the reservation in OtherSys and mappedReady before returning it.
func JITMapMemory(hint, size uintptr) (*JITMapping, error) {
	if size == 0 || size%physPageSize != 0 || size > uintptr(^uint64(0)>>1) || hint%physPageSize != 0 || hint+size < hint {
		return nil, JITModuleError("runtime: invalid JIT mapping range")
	}
	if err := reserveJITMemory(size); err != nil {
		return nil, err
	}
	flags := int32(_MAP_ANON | _MAP_PRIVATE)
	if hint != 0 {
		flags |= jitMapFixedNoReplace
	}
	p, errno := mmap(unsafe.Pointer(hint), size, _PROT_READ|_PROT_WRITE, flags, -1, 0)
	if errno != 0 {
		releaseJITMemory(size)
		return nil, JITMemoryError{errno: uintptr(errno)}
	}
	if p == nil {
		jitDiscardUnpublishedMapping(uintptr(p), size)
		return nil, JITModuleError("runtime: cannot map JIT memory")
	}
	if hint != 0 && uintptr(p) != hint {
		jitDiscardUnpublishedMapping(uintptr(p), size)
		return nil, JITModuleError("runtime: JIT mapping did not honor its address")
	}
	if uintptr(p)+size < uintptr(p) {
		jitDiscardUnpublishedMapping(uintptr(p), size)
		return nil, JITModuleError("runtime: JIT mapping address overflows")
	}
	if asanenabled {
		lsanregisterrootregion(p, size)
	}
	memstats.other_sys.add(int64(size))
	gcController.mappedReady.Add(int64(size))
	setVMAName(p, size, "jit executable")
	return &JITMapping{base: uintptr(p), size: size}, nil
}

// jitDiscardUnpublishedMapping rolls back a mapping that failed validation
// before it became visible to the caller. Keep its reservation charged if the
// kernel refuses to unmap it; releasing the charge would make leaked address
// space available to later reservations.
func jitDiscardUnpublishedMapping(base, size uintptr) {
	_, _, errno := linux.Syscall6(jitSysMunmap, base, size, 0, 0, 0, 0)
	if errno != 0 {
		memstats.other_sys.add(int64(size))
		gcController.mappedReady.Add(int64(size))
		throw("runtime: cannot roll back invalid JIT mapping")
	}
	releaseJITMemory(size)
}

// Protect changes permissions on a subrange of this mapping. Writable
// executable memory is rejected so relocation and execution remain separate.
func (m *JITMapping) Protect(address, size uintptr, protection uint32) error {
	if m == nil || address == 0 || address%physPageSize != 0 || size == 0 || size%physPageSize != 0 || address+size < address || protection&^uint32(_PROT_READ|_PROT_WRITE|_PROT_EXEC) != 0 ||
		protection&uint32(_PROT_WRITE|_PROT_EXEC) == uint32(_PROT_WRITE|_PROT_EXEC) {
		return JITModuleError("runtime: invalid JIT memory protection")
	}
	lock(&m.mu)
	defer unlock(&m.mu)
	if m.owner != nil {
		return JITModuleError("runtime: cannot change protections after JIT module publication")
	}
	if m.base == 0 || address < m.base || address+size > m.base+m.size {
		return JITModuleError("runtime: protection range is outside JIT mapping")
	}
	if ret, errno := mprotect(unsafe.Pointer(address), size, int32(protection)); ret != 0 || errno != 0 {
		return JITMemoryError{errno: uintptr(errno)}
	}
	return nil
}

// Unmap releases this complete JIT mapping. Accounting is removed only
// after munmap succeeds; on failure, callers must keep the range charged.
func (m *JITMapping) Unmap() error {
	if m == nil {
		return JITModuleError("runtime: invalid JIT mapping")
	}
	lock(&m.mu)
	defer unlock(&m.mu)
	if m.owner != nil {
		return JITModuleError("runtime: published JIT mapping is retired by its GC owner")
	}
	if m.base == 0 || m.size == 0 {
		return JITModuleError("runtime: JIT mapping is already unmapped")
	}
	base, size := m.base, m.size
	_, _, errno := linux.Syscall6(jitSysMunmap, base, size, 0, 0, 0, 0)
	if errno != 0 {
		return JITMemoryError{errno: errno}
	}
	if asanenabled {
		lsanunregisterrootregion(unsafe.Pointer(base), size)
	}
	memstats.other_sys.add(-int64(size))
	gcController.mappedReady.Add(-int64(size))
	releaseJITMemory(size)
	m.base, m.size = 0, 0
	return nil
}

// jitUnmapRetired releases an ownerless mapping after the runtime has removed
// its PC index entry and waited for all index readers. The control record is
// stored inside this mapping and must not be read after this call succeeds.
func jitUnmapRetired(base, size uintptr) error {
	if base == 0 || size == 0 || base%physPageSize != 0 || size%physPageSize != 0 {
		return JITModuleError("runtime: invalid retired JIT mapping")
	}
	_, _, errno := linux.Syscall6(jitSysMunmap, base, size, 0, 0, 0, 0)
	if errno != 0 {
		return JITMemoryError{errno: errno}
	}
	if asanenabled {
		lsanunregisterrootregion(unsafe.Pointer(base), size)
	}
	memstats.other_sys.add(-int64(size))
	gcController.mappedReady.Add(-int64(size))
	releaseJITMemory(size)
	jitRetirementComplete()
	return nil
}
