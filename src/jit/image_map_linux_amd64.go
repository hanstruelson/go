// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

type imageImportResolver func(ImageImport) (uintptr, error)

// mapModuleImage maps a pointer-free module image, applies amd64 relocations,
// and enforces final section permissions. One contiguous reservation keeps
// direct RIP-relative references to host symbols within their encoded range.
func mapModuleImage(image ModuleImage, resolve imageImportResolver) (_ *mappedImage, err error) {
	if err := validateModuleImage(image); err != nil {
		return nil, err
	}
	if image.GOOS != "linux" || image.GOARCH != "amd64" {
		return nil, fmt.Errorf("jit: unsupported image target %s/%s", image.GOOS, image.GOARCH)
	}
	if resolve == nil && len(image.Imports) != 0 {
		return nil, fmt.Errorf("jit: image has imports but no host symbol resolver")
	}
	pageSize := uint64(syscall.Getpagesize())
	sectionOffset := make([]uint64, len(image.Sections))
	sectionSize := make([]uint64, len(image.Sections))
	totalSize := uint64(0)
	for i, section := range image.Sections {
		if section.MemSize > ^uint64(0)-(pageSize-1) {
			return nil, fmt.Errorf("jit: image section %d mapping size overflows", i)
		}
		mappedSize := (section.MemSize + pageSize - 1) &^ (pageSize - 1)
		if mappedSize == 0 || mappedSize > uint64(int(^uint(0)>>1)) || totalSize > uint64(int(^uint(0)>>1))-mappedSize {
			return nil, fmt.Errorf("jit: image section %d mapping size is invalid", i)
		}
		sectionOffset[i], sectionSize[i] = totalSize, mappedSize
		totalSize += mappedSize
	}
	importAddresses := make([]uintptr, len(image.Imports))
	for i, symbol := range image.Imports {
		address, resolveErr := resolve(symbol)
		if resolveErr != nil {
			return nil, fmt.Errorf("jit: resolve host import %s.%s: %w", symbol.PackagePath, symbol.Name, resolveErr)
		}
		if address == 0 {
			return nil, fmt.Errorf("jit: host import %s.%s has a nil address", symbol.PackagePath, symbol.Name)
		}
		importAddresses[i] = address
	}
	mapped := &mappedImage{sections: make([][]byte, len(image.Sections))}
	mapping, mapErr := mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset)
	if mapErr != nil {
		return nil, mapErr
	}
	mapped.mapping = mapping
	cleanup := true
	defer func() {
		if cleanup && mapped.mapping != nil {
			_ = munmapImage(mapped.mapping)
		}
	}()
	sectionAddr := make([]uintptr, len(image.Sections))
	for i, section := range image.Sections {
		start := int(sectionOffset[i])
		end := start + int(sectionSize[i])
		mapped.sections[i] = mapping[start:end]
		sectionAddr[i] = uintptrPointer(mapped.sections[i])
		copy(mapped.sections[i], section.Data)
	}

	resolveSymbol := func(target uint32, imported bool) (uintptr, error) {
		if imported {
			if int(target) >= len(image.Imports) {
				return 0, fmt.Errorf("jit: import index %d out of range", target)
			}
			return importAddresses[target], nil
		}
		if int(target) >= len(image.Symbols) {
			return 0, fmt.Errorf("jit: symbol index %d out of range", target)
		}
		symbol := image.Symbols[target]
		return sectionAddr[symbol.Section] + uintptr(symbol.Offset), nil
	}
	for i, reloc := range image.Relocations {
		place := sectionAddr[reloc.Section] + uintptr(reloc.Offset)
		target, resolveErr := resolveSymbol(reloc.Target, reloc.TargetImport)
		if resolveErr != nil {
			return nil, fmt.Errorf("jit: resolve relocation %d: %w", i, resolveErr)
		}
		value, ok := addSignedAddress(target, reloc.Addend)
		if !ok {
			return nil, fmt.Errorf("jit: relocation %d address overflows", i)
		}
		switch reloc.Kind {
		case RelocAMD64Abs64:
			binary.LittleEndian.PutUint64(mapped.sections[reloc.Section][reloc.Offset:], uint64(value))
		case RelocAMD64PC32, RelocAMD64PLT32:
			// AMD64 PC-relative relocations are based on the address after the
			// four-byte displacement, as are x86-64 call/jump instructions.
			delta := int64(value) - (int64(place) + 4)
			if delta < -(1<<31) || delta > (1<<31)-1 {
				label := fmt.Sprintf("symbol %d", reloc.Target)
				if reloc.TargetImport && int(reloc.Target) < len(image.Imports) {
					imp := image.Imports[reloc.Target]
					label = imp.PackagePath + "." + imp.Name
				}
				return nil, fmt.Errorf("jit: relocation %d PC-relative target %s is out of range (place=%#x target=%#x delta=%d)", i, label, place, value, delta)
			}
			binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(int32(delta)))
		case RelocAMD64Addroff32:
			if reloc.TargetImport || int(reloc.Target) >= len(image.Symbols) {
				return nil, fmt.Errorf("jit: relocation %d has an invalid addroff target", i)
			}
			targetOffset, ok := addSignedOffset(image.Symbols[reloc.Target].Offset, reloc.Addend)
			if !ok || targetOffset > uint64(^uint32(0)) {
				return nil, fmt.Errorf("jit: relocation %d addroff target is out of range", i)
			}
			binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(targetOffset))
		case RelocAMD64GOTPCREL, RelocAMD64GOTPCRELX:
			return nil, fmt.Errorf("jit: relocation %d requires a GOT slot, which this image format does not yet provide", i)
		default:
			return nil, fmt.Errorf("jit: unsupported relocation %d kind %d", i, reloc.Kind)
		}
	}

	for i, section := range image.Sections {
		protection := 0
		if section.Read {
			protection |= syscall.PROT_READ
		}
		if section.Write {
			protection |= syscall.PROT_WRITE
		}
		if section.Execute {
			protection |= syscall.PROT_EXEC
		}
		if protection&syscall.PROT_WRITE != 0 && protection&syscall.PROT_EXEC != 0 {
			return nil, fmt.Errorf("jit: image section %d requests writable executable memory", i)
		}
		if err := syscall.Mprotect(mapped.sections[i], protection); err != nil {
			return nil, fmt.Errorf("jit: protect image section %d: %w", i, err)
		}
	}
	for i := range image.Sections {
		start := sectionOffset[i]
		if start == 0 {
			continue
		}
		if start > sectionOffset[i-1]+sectionSize[i-1] {
			padStart := sectionOffset[i-1] + sectionSize[i-1]
			pad := mapping[padStart:start]
			if err := syscall.Mprotect(pad, syscall.PROT_NONE); err != nil {
				return nil, fmt.Errorf("jit: protect image padding: %w", err)
			}
		}
	}

	entry := image.Symbols[image.Entry]
	mapped.entry = sectionAddr[entry.Section] + uintptr(entry.Offset)
	cleanup = false
	return mapped, nil
}

func mapImageNearImports(totalSize, pageSize uint64, image ModuleImage, imports []uintptr, sectionOffset []uint64) ([]byte, error) {
	low, high := int64(0x10000), int64(0x00007ffffffff000-totalSize)
	hasPCImport := false
	for _, reloc := range image.Relocations {
		if !reloc.TargetImport || (reloc.Kind != RelocAMD64PC32 && reloc.Kind != RelocAMD64PLT32) {
			continue
		}
		hasPCImport = true
		if int(reloc.Target) >= len(imports) {
			return nil, fmt.Errorf("jit: PC-relative relocation has invalid import index %d", reloc.Target)
		}
		value, ok := addSignedAddress(imports[reloc.Target], reloc.Addend)
		if !ok {
			return nil, fmt.Errorf("jit: PC-relative import relocation address overflows")
		}
		placeOffset := sectionOffset[reloc.Section] + reloc.Offset + 4
		center := int64(value) - int64(placeOffset)
		low = max(low, center-((1<<31)-1))
		high = min(high, center+(1<<31))
	}
	if !hasPCImport {
		return mmapImage(0, int(totalSize), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	}
	low = (low + int64(pageSize-1)) &^ int64(pageSize-1)
	high &^= int64(pageSize - 1)
	if low < 0x10000 || high < low {
		return nil, fmt.Errorf("jit: host imports have no common amd64 PC-relative mapping range")
	}
	step := (high - low) / 2048
	step = (step + int64(pageSize-1)) &^ int64(pageSize-1)
	if step < int64(pageSize) {
		step = int64(pageSize)
	}
	center := ((low + (high-low)/2) + int64(pageSize-1)) &^ int64(pageSize-1)
	for i := int64(0); i < 4096; i++ {
		candidate := center
		if i != 0 {
			distance := ((i + 1) / 2) * step
			if i&1 != 0 {
				candidate += distance
			} else {
				candidate -= distance
			}
		}
		if candidate < low || candidate > high {
			continue
		}
		mapped, err := mmapFixed(uintptr(candidate), int(totalSize))
		if err == nil {
			return mapped, nil
		}
		if err != syscall.EEXIST && err != syscall.EINVAL {
			return nil, fmt.Errorf("jit: map image near host symbols: %w", err)
		}
	}
	return nil, fmt.Errorf("jit: no free amd64 mapping address satisfies host PC-relative relocations in [%#x,%#x]", low, high)
}

func mmapFixed(address uintptr, size int) ([]byte, error) {
	const mapFixedNoReplace = 0x100000
	return mmapImage(address, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON|mapFixedNoReplace)
}

func mmapImage(address uintptr, size int, protection, flags int) ([]byte, error) {
	r0, _, errno := syscall.RawSyscall6(syscall.SYS_MMAP, address, uintptr(size), uintptr(protection),
		uintptr(flags), ^uintptr(0), 0)
	if errno != 0 {
		return nil, fmt.Errorf("jit: mmap image: %w", errno)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(r0)), size), nil
}

func munmapImage(mapping []byte) error {
	if len(mapping) == 0 {
		return syscall.EINVAL
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_MUNMAP, uintptrPointer(mapping), uintptr(len(mapping)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func uintptrPointer(p []byte) uintptr { return uintptr(unsafe.Pointer(&p[0])) }

func closeMappedImage(mapped *mappedImage) error {
	if mapped == nil {
		return nil
	}
	if mapped.runtimeModule != 0 {
		if err := runtime.UnregisterJITModule(mapped.runtimeModule); err != nil {
			return err
		}
		mapped.runtimeModule = 0
	}
	var firstErr error
	if mapped.mapping != nil {
		if err := munmapImage(mapped.mapping); err != nil {
			firstErr = fmt.Errorf("jit: unmap image mapping at %#x (%d bytes): %w", uintptrPointer(mapped.mapping), len(mapped.mapping), err)
		}
	}
	mapped.mapping = nil
	for i := range mapped.sections {
		mapped.sections[i] = nil
	}
	mapped.entry = 0
	return firstErr
}

func addSignedAddress(address uintptr, addend int64) (uintptr, bool) {
	if addend >= 0 {
		delta := uint64(addend)
		if uint64(address) > ^uint64(0)-delta {
			return 0, false
		}
		return uintptr(uint64(address) + delta), true
	}
	delta := uint64(-(addend + 1)) + 1
	if uint64(address) < delta {
		return 0, false
	}
	return uintptr(uint64(address) - delta), true
}

func addSignedOffset(offset uint64, addend int64) (uint64, bool) {
	if addend >= 0 {
		delta := uint64(addend)
		if offset > ^uint64(0)-delta {
			return 0, false
		}
		return offset + delta, true
	}
	delta := uint64(-(addend + 1)) + 1
	if offset < delta {
		return 0, false
	}
	return offset - delta, true
}
