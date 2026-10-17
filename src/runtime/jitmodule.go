// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"publicinternal/abi"
	"publicinternal/goarch"
	"publicinternal/runtime/sys"
	"unsafe"
)

// JITFunctionMetadata is the function-table input for one function in an
// in-memory Go module. The compiler/linker owns the bytecode tables and stack
// maps; the runtime copies them into non-Go memory before publishing the
// module to stack walkers and the garbage collector.
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
	PCData    [][]byte
	FuncData  [][]byte
}

// JITModuleError describes invalid runtime metadata passed to
// RegisterJITModule or an unknown handle passed to UnregisterJITModule.
type JITModuleError string

func (e JITModuleError) Error() string { return string(e) }

// RegisterJITModule publishes generated Go function metadata so runtime stack
// walking, profiling, panic unwinding, and GC stack scans can recognize PCs in
// textStart..textStart+textSize. All tables are copied into a single runtime
// allocation and remain valid until UnregisterJITModule returns.
func RegisterJITModule(textStart, textSize, rodataStart uintptr, functions []JITFunctionMetadata) (uintptr, error) {
	if textStart == 0 || textSize == 0 || textStart+textSize < textStart || textSize > uintptr(^uint32(0)) {
		return 0, JITModuleError("runtime: invalid JIT text range")
	}
	if len(functions) == 0 || len(functions) > 256 {
		return 0, JITModuleError("runtime: JIT module has no functions")
	}
	for i, fn := range functions {
		if fn.Size == 0 || uint64(fn.Offset)+uint64(fn.Size) > uint64(textSize) || fn.Name == "" || len(fn.PCSP) == 0 {
			return 0, JITModuleError("runtime: invalid JIT function metadata")
		}
		if i != 0 && functions[i-1].Offset >= fn.Offset {
			return 0, JITModuleError("runtime: JIT functions are not in text order")
		}
		if len(fn.PCData) < 2 || len(fn.FuncData) < 2 {
			return 0, JITModuleError("runtime: JIT function is missing GC metadata")
		}
	}

	// Compute one non-pointer allocation for moduledata and every table it
	// references. This mirrors the linker's moduledata lifetime and lets unload
	// release all runtime-owned metadata together.
	headerSize := unsafe.Sizeof(pcHeader{})
	mdSize := unsafe.Sizeof(moduledata{})
	nameSize, pcTabSize, gofuncSize := uintptr(1), uintptr(1), uintptr(0)
	pcLnSize := uintptr(len(functions))*8 + 4
	for _, fn := range functions {
		nameSize += uintptr(len(fn.Name) + 1)
		pcTabSize += uintptr(len(fn.PCSP))
		for _, table := range fn.PCData {
			pcTabSize += uintptr(len(table))
		}
		pcLnSize = alignJIT(pcLnSize, goarch.PtrSize)
		pcLnSize += 44 + uintptr(len(fn.PCData)+len(fn.FuncData))*4
		for _, data := range fn.FuncData {
			if len(data) != 0 {
				gofuncSize = alignJIT(gofuncSize, goarch.PtrSize)
				gofuncSize += uintptr(len(data))
			}
		}
	}
	pcLnSize = alignJIT(pcLnSize, goarch.PtrSize)
	bucketCount := (textSize + abi.FuncTabBucketSize - 1) / abi.FuncTabBucketSize
	if bucketCount == 0 {
		bucketCount = 1
	}
	findFuncSize := bucketCount * uintptr(unsafe.Sizeof(findfuncbucket{}))

	total := alignJIT(mdSize, goarch.PtrSize) + headerSize
	total = alignJIT(total, goarch.PtrSize) + nameSize
	total = alignJIT(total, goarch.PtrSize) + pcTabSize
	total = alignJIT(total, goarch.PtrSize) + gofuncSize
	total = alignJIT(total, goarch.PtrSize) + pcLnSize
	total = alignJIT(total, goarch.PtrSize) + findFuncSize
	if total > 256<<20 {
		return 0, JITModuleError("runtime: JIT runtime tables exceed size limit")
	}
	base := sysAlloc(total, &memstats.other_sys, "jit module metadata")
	if base == nil {
		return 0, JITModuleError("runtime: cannot allocate JIT runtime metadata")
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
	pctab := allocAlignedBytes(pcTabSize)
	gofunc := allocAlignedBytes(gofuncSize)
	pclntab := allocAlignedBytes(pcLnSize)
	findfunctab := allocAlignedBytes(findFuncSize)

	// Build names, PC tables, function records, and funcdata offsets.
	nameOffs := make([]uint32, len(functions))
	pcspOffs := make([]uint32, len(functions))
	pcdataOffs := make([][]uint32, len(functions))
	gofuncOffs := make([][]uint32, len(functions))
	nameAt, pcAt, gofuncAt := uintptr(0), uintptr(1), uintptr(0)
	for i, fn := range functions {
		nameOffs[i] = uint32(nameAt)
		copy(funcnames[nameAt:], fn.Name)
		nameAt += uintptr(len(fn.Name) + 1)
		pcspOffs[i] = uint32(pcAt)
		copy(pctab[pcAt:], fn.PCSP)
		pcAt += uintptr(len(fn.PCSP))
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
		put32(off+20, 0) // pcfile is intentionally absent
		put32(off+24, 0) // pcline is intentionally absent
		put32(off+28, uint32(len(fn.PCData)))
		put32(off+32, ^uint32(0)) // no compilation unit table
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
	hdr.nfiles = 0
	headerBytes := uintptr(unsafe.Sizeof(pcHeader{}))
	hdr.funcnameOffset = headerBytes
	hdr.cuOffset = headerBytes + uintptr(len(funcnames))
	hdr.filetabOffset = hdr.cuOffset
	hdr.pctabOffset = hdr.filetabOffset
	hdr.pclnOffset = headerBytes + uintptr(len(funcnames)+len(pctab))

	md.pcHeader = hdr
	md.funcnametab = funcnames
	md.pctab = pctab
	md.pclntable = pclntab
	md.gofunc = uintptr(unsafe.Pointer(unsafe.SliceData(gofunc)))
	md.epclntab = md.gofunc + uintptr(len(gofunc))
	md.ftab = unsafe.Slice((*functab)(unsafe.Pointer(unsafe.SliceData(pclntab))), len(functions)+1)
	md.findfunctab = uintptr(unsafe.Pointer(unsafe.SliceData(findfunctab)))
	md.text, md.etext = textStart, textStart+textSize
	md.minpc, md.maxpc = textStart, textStart+textSize
	md.rodata = rodataStart
	md.jitModule = true
	md.jitAllocBase, md.jitAllocSize = base, total
	buildJITFindFuncTable(findfunctab, functions, textSize)

	stw := stopTheWorld(stwAllGoroutinesStack)
	md.next = nil
	lastmoduledatap.next = md
	lastmoduledatap = md
	moduledataverify1(md)
	old := activeModules()
	modules := make([]*moduledata, len(old)+1)
	copy(modules, old)
	modules[len(old)] = md
	moduleSlice := new([]*moduledata)
	*moduleSlice = modules
	atomicstorep(unsafe.Pointer(&modulesSlice), unsafe.Pointer(moduleSlice))
	startTheWorld(stw)
	return uintptr(unsafe.Pointer(md)), nil
}

// UnregisterJITModule removes one generated module from runtime PC lookup and
// GC metadata before releasing its runtime tables. The caller must ensure no
// JIT function in the module is still executing.
func UnregisterJITModule(handle uintptr) error {
	if handle == 0 {
		return JITModuleError("runtime: invalid JIT module handle")
	}
	md := (*moduledata)(unsafe.Pointer(handle))
	stw := stopTheWorld(stwAllGoroutinesStack)
	var prev *moduledata
	for current := &firstmoduledata; current != nil; current = current.next {
		if current != md {
			prev = current
			continue
		}
		if !current.jitModule || current.jitAllocBase == nil || current.jitAllocSize == 0 {
			startTheWorld(stw)
			return JITModuleError("runtime: unknown JIT module handle")
		}
		if prev == nil {
			startTheWorld(stw)
			return JITModuleError("runtime: cannot unregister the first runtime module")
		}
		prev.next = current.next
		if lastmoduledatap == current {
			lastmoduledatap = prev
		}
		old := activeModules()
		modules := make([]*moduledata, 0, len(old)-1)
		for _, module := range old {
			if module != current {
				modules = append(modules, module)
			}
		}
		moduleSlice := new([]*moduledata)
		*moduleSlice = modules
		atomicstorep(unsafe.Pointer(&modulesSlice), unsafe.Pointer(moduleSlice))
		allocationBase, allocationSize := current.jitAllocBase, current.jitAllocSize
		current.next = nil
		current.jitAllocBase = nil
		current.jitAllocSize = 0
		startTheWorld(stw)
		sysFree(allocationBase, allocationSize, &memstats.other_sys)
		return nil
	}
	startTheWorld(stw)
	return JITModuleError("runtime: unknown JIT module handle")
}

// ResolveJITFunction returns the entry PC for a linked Go function by its
// full linker symbol name. It is used only for compiler-generated runtime
// references that have no source-level import object.
func ResolveJITFunction(name string) uintptr {
	for md := &firstmoduledata; md != nil; md = md.next {
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

func buildJITFindFuncTable(table []byte, functions []JITFunctionMetadata, textSize uintptr) {
	const subbucketCount = 16
	const bucketSize = uintptr(abi.FuncTabBucketSize)
	const subbucketSize = bucketSize / subbucketCount
	bucketCount := (textSize + bucketSize - 1) / bucketSize
	for bucket := uintptr(0); bucket < bucketCount; bucket++ {
		var indexes [subbucketCount]uint32
		for sub := uintptr(0); sub < subbucketCount; sub++ {
			pc := bucket*bucketSize + sub*subbucketSize
			index := 0
			for index+1 < len(functions) && uintptr(functions[index+1].Offset) <= pc {
				index++
			}
			indexes[sub] = uint32(index)
		}
		base := indexes[0]
		at := bucket * uintptr(unsafe.Sizeof(findfuncbucket{}))
		*(*uint32)(unsafe.Pointer(&table[at])) = base
		for sub := uintptr(0); sub < subbucketCount; sub++ {
			table[at+4+sub] = byte(indexes[sub] - base)
		}
	}
}

func alignJIT(n, alignment uintptr) uintptr {
	return (n + alignment - 1) &^ (alignment - 1)
}
