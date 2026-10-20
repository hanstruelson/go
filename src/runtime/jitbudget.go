// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "publicinternal/runtime/atomic"

var (
	jitMemoryLimit uint64
	jitMemoryUsed  uint64
)

// JITMemoryLimitError reports that a JIT mapping or runtime-owned auxiliary
// storage could not reserve space under the configured limit. Used and
// requested bytes stay charged until the corresponding memory is released.
type JITMemoryLimitError struct {
	used      uint64
	limit     uint64
	requested uint64
}

func (e JITMemoryLimitError) Error() string {
	return "runtime: JIT memory limit exceeded"
}

func (e JITMemoryLimitError) Used() uint64      { return e.used }
func (e JITMemoryLimitError) Limit() uint64     { return e.limit }
func (e JITMemoryLimitError) Requested() uint64 { return e.requested }

// SetJITMemoryLimit configures the combined byte limit for executable image
// mappings, per-module runtime tables, published JIT module-index snapshots,
// and span owner side tables. A limit of zero means unlimited. Lowering the
// limit does not forcibly retire live code; future reservations fail until
// enough eligible memory has been reclaimed.
func SetJITMemoryLimit(limit uint64) {
	atomic.Store64(&jitMemoryLimit, limit)
}

// JITMemoryStats reports currently reserved JIT mapping, per-module runtime
// table, module-index, and span owner side-table bytes, and the configured
// limit. During publication it includes both old and replacement index
// snapshots until readers drain. It does not count Go heap objects retained
// by JIT owners.
func JITMemoryStats() (used, limit uint64) {
	return atomic.Load64(&jitMemoryUsed), atomic.Load64(&jitMemoryLimit)
}

func reserveJITMemory(size uintptr) error {
	requested := uint64(size)
	if uintptr(requested) != size {
		return JITModuleError("runtime: JIT memory reservation overflows uint64")
	}
	for {
		used := atomic.Load64(&jitMemoryUsed)
		limit := atomic.Load64(&jitMemoryLimit)
		if requested > ^uint64(0)-used || (limit != 0 && (used > limit || requested > limit-used)) {
			return JITMemoryLimitError{used: used, limit: limit, requested: requested}
		}
		if atomic.Cas64(&jitMemoryUsed, used, used+requested) {
			return nil
		}
	}
}

func releaseJITMemory(size uintptr) {
	requested := uint64(size)
	if uintptr(requested) != size {
		throw("runtime: JIT memory release overflows uint64")
	}
	for {
		used := atomic.Load64(&jitMemoryUsed)
		if used < requested {
			throw("runtime: JIT memory accounting underflow")
		}
		if atomic.Cas64(&jitMemoryUsed, used, used-requested) {
			return
		}
	}
}
