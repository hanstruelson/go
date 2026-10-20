// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

type imageImportResolver func(ImageImport) (uintptr, error)

// Linux/amd64's internal Go linker uses R_TLS_LE addends relative to -8(FS),
// the slot where the runtime's g pointer is installed. See
// cmd/link/publicinternal/ld.computeTLSOffset and runtime's clone setup.
const linuxAMD64TLSThreadOffset int64 = -8

// mapModuleImage maps a pointer-free module image, applies amd64 relocations,
// and enforces final section permissions. One contiguous reservation keeps
// direct RIP-relative references to host symbols within their encoded range.
func mapModuleImage(image ModuleImage, resolve imageImportResolver) (result *mappedImage, err error) {
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
	// Reserve the first page for runtime-owned retirement state. It remains
	// RW/NX for the module lifetime and is never part of executable sections.
	totalSize := pageSize
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
	rodataBaseIndex := moduleImageReadOnlyBase(image.Sections)
	textBaseIndex := -1
	for i, section := range image.Sections {
		if section.Kind == ImageText {
			textBaseIndex = i
			break
		}
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
	// Keep the image-local descriptor for resolving compiler offsets, but
	// bind direct address relocations to the verified host descriptor. This
	// prevents an unnamed structural type from acquiring a second runtime
	// identity just because it was emitted in a JIT object.
	typeAliasImports := make(map[uint32]uint32, len(image.TypeAliases))
	for _, alias := range image.TypeAliases {
		typeAliasImports[alias.Symbol] = alias.Import
	}
	mapping, runtimeMapping, mapErr := mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset)
	if isJITMemoryLimitError(mapErr) {
		// External JIT bytes participate in runtime memory accounting. If the
		// reservation is over budget, force collection and wait for eligible
		// owner retirements before retrying the private mapping.
		runtime.GC()
		if runtime.JITWaitForRetirements(5e9) {
			mapping, runtimeMapping, mapErr = mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset)
		}
	}
	if mapErr != nil {
		return nil, mapErr
	}
	mapped := &mappedImage{
		sections: make([][]byte, len(image.Sections)), mapping: mapping,
		runtimeMapping: runtimeMapping, control: uintptrPointer(mapping[:pageSize]),
	}
	cleanup := true
	defer func() {
		if cleanup && mapped.runtimeMapping != nil {
			if unmapErr := mapped.runtimeMapping.Unmap(); unmapErr != nil {
				// Keep the ownership token reachable so the caller can retry the
				// failed release. Returning only the original mapping error here
				// would orphan an accounted reservation with no way to reclaim it.
				result = mapped
				err = errors.Join(err, fmt.Errorf("jit: release partially constructed image: %w", unmapErr))
			}
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
	mapped.imports = append([]uintptr(nil), importAddresses...)
	mapped.symbols = make([]uintptr, len(image.Symbols))
	for i, symbol := range image.Symbols {
		mapped.symbols[i] = sectionAddr[symbol.Section] + uintptr(symbol.Offset)
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
		if importIndex, ok := typeAliasImports[target]; ok {
			if int(importIndex) >= len(importAddresses) {
				return 0, fmt.Errorf("jit: canonical type import index %d out of range", importIndex)
			}
			return importAddresses[importIndex], nil
		}
		symbol := image.Symbols[target]
		return sectionAddr[symbol.Section] + uintptr(symbol.Offset), nil
	}
	for i, reloc := range image.Relocations {
		place := sectionAddr[reloc.Section] + uintptr(reloc.Offset)
		var value uintptr
		if reloc.Kind != RelocAMD64TLSLE32 {
			target, resolveErr := resolveSymbol(reloc.Target, reloc.TargetImport)
			if resolveErr != nil {
				return nil, fmt.Errorf("jit: resolve relocation %d: %w", i, resolveErr)
			}
			var ok bool
			value, ok = addSignedAddress(target, reloc.Addend)
			if !ok {
				return nil, fmt.Errorf("jit: relocation %d address overflows", i)
			}
		}
		switch reloc.Kind {
		case RelocAMD64Abs64:
			binary.LittleEndian.PutUint64(mapped.sections[reloc.Section][reloc.Offset:], uint64(value))
		case RelocAMD64Abs32:
			if uint64(value) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("jit: relocation %d absolute target %#x does not fit in 32 bits", i, value)
			}
			binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(value))
		case RelocAMD64PC32, RelocAMD64PLT32, RelocAMD64GOTPCREL, RelocAMD64GOTPCRELX:
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
			targetSymbol := image.Symbols[reloc.Target]
			baseIndex := rodataBaseIndex
			if targetSymbol.Section >= uint32(len(image.Sections)) {
				return nil, fmt.Errorf("jit: relocation %d addroff target has invalid section %d", i, targetSymbol.Section)
			}
			targetKind := image.Sections[targetSymbol.Section].Kind
			if targetKind == ImageText {
				baseIndex = textBaseIndex
			} else if targetKind != ImageReadOnly && targetKind != ImageTypes && targetKind != ImageITabs {
				return nil, fmt.Errorf("jit: relocation %d addroff target is in non-metadata section %d", i, targetKind)
			}
			if baseIndex < 0 || int(targetSymbol.Section) < baseIndex {
				return nil, fmt.Errorf("jit: relocation %d addroff target is outside its module base section", i)
			}
			targetOffset := sectionOffset[targetSymbol.Section] - sectionOffset[baseIndex] + targetSymbol.Offset
			targetOffset, ok := addSignedOffset(targetOffset, reloc.Addend)
			if !ok || targetOffset > uint64(^uint32(0)) {
				return nil, fmt.Errorf("jit: relocation %d addroff target is out of range", i)
			}
			binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(targetOffset))
		case RelocAMD64TLSLE32:
			tlsOffset := linuxAMD64TLSThreadOffset + reloc.Addend
			if tlsOffset < -(1<<31) || tlsOffset > (1<<31)-1 {
				return nil, fmt.Errorf("jit: relocation %d TLS offset %d does not fit in 32 bits", i, tlsOffset)
			}
			binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(int32(tlsOffset)))
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
		if err := mapped.runtimeMapping.Protect(uintptrPointer(mapped.sections[i]), uintptr(len(mapped.sections[i])), uint32(protection)); err != nil {
			return nil, fmt.Errorf("jit: protect image section %d: %w", i, err)
		}
	}
	for i := range image.Sections {
		start := sectionOffset[i]
		if i == 0 {
			continue
		}
		if start > sectionOffset[i-1]+sectionSize[i-1] {
			padStart := sectionOffset[i-1] + sectionSize[i-1]
			pad := mapping[padStart:start]
			if err := mapped.runtimeMapping.Protect(uintptrPointer(pad), uintptr(len(pad)), 0); err != nil {
				return nil, fmt.Errorf("jit: protect image padding: %w", err)
			}
		}
	}

	entry := image.Symbols[image.Entry]
	mapped.entry = sectionAddr[entry.Section] + uintptr(entry.Offset)
	cleanup = false
	return mapped, nil
}

func isJITMemoryLimitError(err error) bool {
	var limitError runtime.JITMemoryLimitError
	return errors.As(err, &limitError)
}

func mapImageNearImports(totalSize, pageSize uint64, image ModuleImage, imports []uintptr, sectionOffset []uint64) ([]byte, *runtime.JITMapping, error) {
	low, high := int64(0x10000), int64(0x00007ffffffff000-totalSize)
	hasPCImport := false
	canonicalTypes := make(map[uint32]uint32, len(image.TypeAliases))
	for _, alias := range image.TypeAliases {
		canonicalTypes[alias.Symbol] = alias.Import
	}
	for _, reloc := range image.Relocations {
		if reloc.Kind != RelocAMD64PC32 && reloc.Kind != RelocAMD64PLT32 {
			continue
		}
		targetIndex, imported := reloc.Target, reloc.TargetImport
		if !imported {
			targetIndex, imported = canonicalTypes[reloc.Target]
		}
		if !imported {
			continue
		}
		hasPCImport = true
		if int(targetIndex) >= len(imports) {
			return nil, nil, fmt.Errorf("jit: PC-relative relocation has invalid import index %d", targetIndex)
		}
		value, ok := addSignedAddress(imports[targetIndex], reloc.Addend)
		if !ok {
			return nil, nil, fmt.Errorf("jit: PC-relative import relocation address overflows")
		}
		placeOffset := sectionOffset[reloc.Section] + reloc.Offset + 4
		center := int64(value) - int64(placeOffset)
		low = max(low, center-((1<<31)-1))
		high = min(high, center+(1<<31))
	}
	if !hasPCImport {
		return mmapImage(0, int(totalSize))
	}
	low = (low + int64(pageSize-1)) &^ int64(pageSize-1)
	high &^= int64(pageSize - 1)
	if low < 0x10000 || high < low {
		return nil, nil, fmt.Errorf("jit: host imports have no common amd64 PC-relative mapping range")
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
		mapped, token, err := mmapFixed(uintptr(candidate), int(totalSize))
		if err == nil {
			return mapped, token, nil
		}
		if !errors.Is(err, syscall.EEXIST) && !errors.Is(err, syscall.EINVAL) {
			return nil, nil, fmt.Errorf("jit: map image near host symbols: %w", err)
		}
	}
	return nil, nil, fmt.Errorf("jit: no free amd64 mapping address satisfies host PC-relative relocations in [%#x,%#x]", low, high)
}

func mmapFixed(address uintptr, size int) ([]byte, *runtime.JITMapping, error) {
	return mmapImage(address, size)
}

func mmapImage(address uintptr, size int) ([]byte, *runtime.JITMapping, error) {
	token, err := runtime.JITMapMemory(address, uintptr(size))
	if err != nil {
		var errnoError interface{ Errno() uintptr }
		if errors.As(err, &errnoError) {
			return nil, nil, syscall.Errno(errnoError.Errno())
		}
		return nil, nil, fmt.Errorf("jit: mmap image: %w", err)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(token.Base())), size), token, nil
}

func uintptrPointer(p []byte) uintptr { return uintptr(unsafe.Pointer(&p[0])) }

func closeMappedImage(mapped *mappedImage) error {
	if mapped == nil {
		return nil
	}
	if mapped.runtimeOwner != nil {
		return fmt.Errorf("jit: registered module lifetime is owned by the runtime; release its Code handle instead")
	}
	// Once runtime metadata is unpublished, the entry must not remain callable
	// even if the operating system rejects the subsequent unmap.
	mapped.entry = 0
	if mapped.mapping != nil && mapped.runtimeMapping == nil {
		return fmt.Errorf("jit: image mapping has no runtime ownership token")
	}
	if mapped.runtimeMapping != nil {
		if err := mapped.runtimeMapping.Unmap(); err != nil {
			return fmt.Errorf("jit: unmap image mapping at %#x (%d bytes): %w", uintptrPointer(mapped.mapping), len(mapped.mapping), err)
		}
		mapped.runtimeMapping = nil
	}
	mapped.mapping = nil
	mapped.symbols = nil
	mapped.imports = nil
	for i := range mapped.sections {
		mapped.sections[i] = nil
	}
	return nil
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
