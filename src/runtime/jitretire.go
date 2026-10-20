// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"publicinternal/runtime/atomic"
	"publicinternal/runtime/sys"
	"unsafe"
)

// jitRetireRecord lives at the start of the JIT mapping, in its RW/NX control
// page. It contains no Go pointers and is independent of the owner object whose
// sweep queues it. The mapping is kept intact until the reaper has unpublished
// the module and drained PC-index readers.
type jitRetireRecord struct {
	next         uintptr
	module       uintptr
	metadataBase uintptr
	metadataSize uintptr
	mappingBase  uintptr
	mappingSize  uintptr
	weakHandle   uintptr
	pinSlot      uintptr
	generation   uint64
	sweepSeq     uint64
	state        uint32
	phase        uint32
	retries      uint32
	_            uint32
}

type specialJIT struct {
	_ sys.NotInHeap
	special
	record uintptr
}

var (
	jitModuleGeneration uint64
	jitRetireHead       uintptr
	jitRetireNote       note
	jitReaperLock       mutex
	jitReaperStarted    bool
	jitSweepSequence    uint64
	jitSweepCompleted   uint64
	jitRetirePending    uint64
)

// jitSweepComplete publishes a monotonic high-water mark only after all
// sweepers for the generation have left and the sweep cleanup queue is
// flushed. JIT retirement waits for this mark before releasing metadata that
// any remaining sweep path could have observed.
func jitSweepComplete(sequence uint64) {
	if sequence == 0 {
		throw("runtime: zero JIT sweep completion sequence")
	}
	for completed := atomic.Load64(&jitSweepCompleted); completed < sequence; completed = atomic.Load64(&jitSweepCompleted) {
		if atomic.Cas64(&jitSweepCompleted, completed, sequence) {
			return
		}
	}
}

func startJITReaper() {
	lock(&jitReaperLock)
	if jitReaperStarted {
		unlock(&jitReaperLock)
		return
	}
	jitReaperStarted = true
	unlock(&jitReaperLock)
	go jitReaper()
}

// addJITOwner attaches a sweep special to a pointerful GC owner. The special
// carries only the independent control-page address, never a pointer back to
// the owner or its children.
func addJITOwner(owner *JITModuleOwner, record uintptr) bool {
	lock(&mheap_.speciallock)
	s := (*specialJIT)(mheap_.specialJITAlloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialJIT
	s.record = record
	if !addspecial(unsafe.Pointer(owner), &s.special, false) {
		lock(&mheap_.speciallock)
		mheap_.specialJITAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
		return false
	}
	KeepAlive(owner)
	return true
}

func removeJITOwner(owner *JITModuleOwner) {
	s := (*specialJIT)(unsafe.Pointer(removespecial(unsafe.Pointer(owner), _KindSpecialJIT)))
	if s == nil {
		return
	}
	lock(&mheap_.speciallock)
	mheap_.specialJITAlloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// jitRetireEnqueue is called by the sweeper after the owner has become
// unreachable. The queue is an intrusive lock-free stack rooted outside the
// Go heap; all storage was reserved before the owner became collectible.
func jitRetireEnqueue(record uintptr) {
	if record == 0 {
		throw("runtime: nil JIT retirement record")
	}
	r := (*jitRetireRecord)(unsafe.Pointer(record))
	if r.sweepSeq == 0 {
		r.sweepSeq = atomic.Load64(&jitSweepSequence)
		if r.sweepSeq == 0 {
			throw("runtime: JIT owner retired outside a sweep generation")
		}
		atomic.Xadd64(&jitRetirePending, 1)
	}
	if !atomic.Cas(&r.state, 0, 1) {
		throw("runtime: duplicate JIT retirement record")
	}
	for {
		head := atomic.Loaduintptr(&jitRetireHead)
		r.next = head
		if atomic.Casuintptr(&jitRetireHead, head, record) {
			break
		}
	}
}

func jitRetirePop() uintptr {
	for {
		head := atomic.Loaduintptr(&jitRetireHead)
		if head == 0 {
			return 0
		}
		r := (*jitRetireRecord)(unsafe.Pointer(head))
		next := r.next
		if atomic.Casuintptr(&jitRetireHead, head, next) {
			r.next = 0
			return head
		}
	}
}

func jitReaper() {
	var prunedSweep uint64
	for {
		retryPending := false
		for record := jitRetirePop(); record != 0; record = jitRetirePop() {
			if !jitRetire(record) {
				// A failed record has been put back on the queue. Leave it for
				// the next pass so a persistent OS/runtime error cannot spin the
				// reaper or starve other runtime work.
				retryPending = true
				break
			}
		}
		if completed := atomic.Load64(&jitSweepCompleted); completed > prunedSweep {
			jitPruneModuleCaches(false)
			prunedSweep = completed
		}
		// Sweeping can run while the scheduler lock is held (notably during
		// procresize at the end of a stop-the-world GC). Waking a goroutine
		// from freeSpecial in that state can deadlock trying to reacquire the
		// scheduler lock. Polling a pointer-free queue avoids scheduler calls
		// from the sweep path; reclamation follows promptly after the sweep.
		if retryPending {
			notetsleepg(&jitRetireNote, 100e6)
		} else {
			notetsleepg(&jitRetireNote, 10e6)
		}
	}
}

func jitRetire(record uintptr) bool {
	r := (*jitRetireRecord)(unsafe.Pointer(record))
	if atomic.Load(&r.state) != 1 || r.module == 0 || r.metadataBase == 0 || r.metadataSize == 0 || r.mappingBase == 0 || r.mappingSize == 0 || r.weakHandle == 0 || r.pinSlot == 0 || r.generation == 0 || r.sweepSeq == 0 {
		throw("runtime: corrupt JIT retirement record")
	}
	if atomic.Load64(&jitSweepCompleted) < r.sweepSeq {
		return jitRetireRetry(r, "")
	}
	if r.phase == 0 {
		md := (*moduledata)(unsafe.Pointer(r.module))
		if err := unpublishJITModule(md, r.generation); err != nil {
			// Failing closed keeps both the mapping and metadata allocated. The
			// owner is already unreachable, so no new generated calls can begin.
			return jitRetireRetry(r, "could not retire JIT module: "+err.Error())
		}
		md.jitAllocBase = nil
		md.jitAllocSize = 0
		removeJITOwnerRoots(r.weakHandle, r.pinSlot)
		sysFree(unsafe.Pointer(r.metadataBase), r.metadataSize, &memstats.other_sys)
		releaseJITMemory(r.metadataSize)
		atomic.Store(&r.phase, 1)
	} else if r.phase != 1 {
		throw("runtime: corrupt JIT retirement phase")
	}
	if err := jitUnmapRetired(r.mappingBase, r.mappingSize); err != nil {
		// Keep a diagnostic state in the still-mapped control page. Accounting
		// remains charged until a later attempt confirms the unmap succeeded.
		return jitRetireRetry(r, "could not unmap retired JIT module: "+err.Error())
	}
	return true
}

func jitRetireRetry(r *jitRetireRecord, message string) bool {
	retries := atomic.Xadd(&r.retries, 1)
	if message != "" && (retries == 1 || retries%256 == 0) {
		println("runtime:", message, "(retry", retries, ")")
	}
	atomic.Store(&r.state, 0)
	jitRetireEnqueue(uintptr(unsafe.Pointer(r)))
	return false
}

// JITWaitForRetirements waits until all queued JIT owner retirements have
// completed. A zero timeout waits without a deadline; a positive timeout is
// in nanoseconds and returns false if work remains when it expires.
func JITWaitForRetirements(timeout int64) bool {
	if timeout < 0 {
		return false
	}
	start := nanotime()
	// A collection returns before concurrent sweeping necessarily reaches every
	// owner special. Snapshot the current generation and wait for its sweep
	// completion as well as the queue count, so a zero count observed just
	// before a special is queued cannot make Reclaim report early success.
	targetSweep := atomic.Load64(&jitSweepSequence)
	for atomic.Load64(&jitRetirePending) != 0 || atomic.Load64(&jitSweepCompleted) < targetSweep {
		wait := int64(10e6)
		if timeout > 0 {
			elapsed := nanotime() - start
			if elapsed >= timeout {
				return false
			}
			if remaining := timeout - elapsed; remaining < wait {
				wait = remaining
			}
		}
		// Use a private timed note: completion is polled, and multiple callers
		// may wait concurrently. A shared note would require one-waiter wakeup
		// coordination and could be woken twice.
		var waitNote note
		notetsleepg(&waitNote, wait)
	}
	return true
}

func jitRetirementComplete() {
	remaining := atomic.Xadd64(&jitRetirePending, -1)
	if remaining == ^uint64(0) {
		throw("runtime: JIT retirement count underflow")
	}
}
