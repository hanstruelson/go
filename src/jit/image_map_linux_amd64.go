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
	guard := runtime.JITBeginTypeIdentityUpdate()
	defer guard.Release()
	return mapModuleImageWithTypeIdentityGuard(image, resolve, guard)
}

func mapModuleImageWithTypeIdentityGuard(image ModuleImage, resolve imageImportResolver, guard *runtime.JITTypeIdentityGuard) (result *mappedImage, err error) {
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
	hasRuntimeTypeDescriptors := false
	for _, symbol := range image.Symbols {
		if symbol.Kind == ImageType && uintptr(symbol.Size) >= runtime.JITTypeDescriptorSize() {
			hasRuntimeTypeDescriptors = true
			break
		}
	}
	legacyTypeImports := make(map[uint32]uint32)
	if !hasRuntimeTypeDescriptors {
		for _, alias := range image.TypeAliases {
			legacyTypeImports[alias.Symbol] = alias.Import
		}
	}
	mapping, runtimeMapping, mapErr := mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset, legacyTypeImports, nil)
	if isJITMemoryLimitError(mapErr) {
		// External JIT bytes participate in runtime memory accounting. If the
		// reservation is over budget, force collection and wait for eligible
		// owner retirements before retrying the private mapping.
		runtime.GC()
		if runtime.JITWaitForRetirements(5e9) {
			mapping, runtimeMapping, mapErr = mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset, legacyTypeImports, nil)
		}
	}
	if mapErr != nil {
		return nil, mapErr
	}
	mapped := &mappedImage{
		mapping: mapping,
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
	mapped.imports = append([]uintptr(nil), importAddresses...)
	var sectionAddr []uintptr
	installMappedSections := func() {
		mapped.sections = make([][]byte, len(image.Sections))
		sectionAddr = make([]uintptr, len(image.Sections))
		for i, section := range image.Sections {
			start := int(sectionOffset[i])
			end := start + int(sectionSize[i])
			mapped.sections[i] = mapping[start:end]
			sectionAddr[i] = uintptrPointer(mapped.sections[i])
			copy(mapped.sections[i], section.Data)
		}
		mapped.symbols = make([]uintptr, len(image.Symbols))
		for i, symbol := range image.Symbols {
			mapped.symbols[i] = sectionAddr[symbol.Section] + uintptr(symbol.Offset)
		}
	}
	installMappedSections()
	canonicalTypeAddresses := make(map[uintptr]uintptr, len(image.TypeAliases))
	if !hasRuntimeTypeDescriptors {
		for _, alias := range image.TypeAliases {
			canonicalTypeAddresses[mapped.symbols[alias.Symbol]] = importAddresses[alias.Import]
		}
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
		if canonical, ok := canonicalTypeAddresses[mapped.symbols[target]]; ok {
			return canonical, nil
		}
		symbol := image.Symbols[target]
		return sectionAddr[symbol.Section] + uintptr(symbol.Offset), nil
	}
	applyRelocations := func() error {
		for i, reloc := range image.Relocations {
			place := sectionAddr[reloc.Section] + uintptr(reloc.Offset)
			var value uintptr
			if reloc.Kind != RelocAMD64TLSLE32 {
				target, resolveErr := resolveSymbol(reloc.Target, reloc.TargetImport)
				if resolveErr != nil {
					return fmt.Errorf("jit: resolve relocation %d: %w", i, resolveErr)
				}
				var ok bool
				value, ok = addSignedAddress(target, reloc.Addend)
				if !ok {
					return fmt.Errorf("jit: relocation %d address overflows", i)
				}
			}
			switch reloc.Kind {
			case RelocAMD64Abs64:
				binary.LittleEndian.PutUint64(mapped.sections[reloc.Section][reloc.Offset:], uint64(value))
			case RelocAMD64Abs32:
				if uint64(value) > uint64(^uint32(0)) {
					return fmt.Errorf("jit: relocation %d absolute target %#x does not fit in 32 bits", i, value)
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
					return fmt.Errorf("jit: relocation %d PC-relative target %s is out of range (place=%#x target=%#x delta=%d)", i, label, place, value, delta)
				}
				binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(int32(delta)))
			case RelocAMD64Addroff32:
				if reloc.TargetImport || int(reloc.Target) >= len(image.Symbols) {
					return fmt.Errorf("jit: relocation %d has an invalid addroff target", i)
				}
				targetSymbol := image.Symbols[reloc.Target]
				baseIndex := rodataBaseIndex
				if targetSymbol.Section >= uint32(len(image.Sections)) {
					return fmt.Errorf("jit: relocation %d addroff target has invalid section %d", i, targetSymbol.Section)
				}
				targetKind := image.Sections[targetSymbol.Section].Kind
				if targetKind == ImageText {
					baseIndex = textBaseIndex
				} else if targetKind != ImageReadOnly && targetKind != ImageTypes && targetKind != ImageITabs {
					return fmt.Errorf("jit: relocation %d addroff target is in non-metadata section %d", i, targetKind)
				}
				if baseIndex < 0 || int(targetSymbol.Section) < baseIndex {
					return fmt.Errorf("jit: relocation %d addroff target is outside its module base section", i)
				}
				targetOffset := sectionOffset[targetSymbol.Section] - sectionOffset[baseIndex] + targetSymbol.Offset
				targetOffset, ok := addSignedOffset(targetOffset, reloc.Addend)
				if !ok || targetOffset > uint64(^uint32(0)) {
					return fmt.Errorf("jit: relocation %d addroff target is out of range", i)
				}
				binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(targetOffset))
			case RelocAMD64TLSLE32:
				tlsOffset := linuxAMD64TLSThreadOffset + reloc.Addend
				if tlsOffset < -(1<<31) || tlsOffset > (1<<31)-1 {
					return fmt.Errorf("jit: relocation %d TLS offset %d does not fit in 32 bits", i, tlsOffset)
				}
				binary.LittleEndian.PutUint32(mapped.sections[reloc.Section][reloc.Offset:], uint32(int32(tlsOffset)))
			default:
				return fmt.Errorf("jit: unsupported relocation %d kind %d", i, reloc.Kind)
			}
		}
		return nil
	}
	if err := applyRelocations(); err != nil {
		return nil, err
	}

	typesStart, typesEnd := uintptr(0), uintptr(0)
	if rodataBaseIndex >= 0 {
		typesStart = sectionAddr[rodataBaseIndex]
		for i, section := range image.Sections {
			switch section.Kind {
			case ImageReadOnly, ImageTypes, ImageITabs:
				if end := sectionAddr[i] + uintptr(section.MemSize); end > typesEnd {
					typesEnd = end
				}
			}
		}
	}
	explicitTypeMappings := make([]runtime.JITTypeMapping, 0, len(image.TypeAliases))
	explicitTypeMappingByAddress := make(map[uintptr]int, len(image.TypeAliases))
	conflictingTypeAddresses := make(map[uintptr]bool)
	for _, alias := range image.TypeAliases {
		dynamic, canonical := mapped.symbols[alias.Symbol], importAddresses[alias.Import]
		if conflictingTypeAddresses[dynamic] {
			continue
		}
		if previous, exists := explicitTypeMappingByAddress[dynamic]; exists {
			if explicitTypeMappings[previous].Canonical != canonical {
				explicitTypeMappings[previous].Dynamic = 0
				delete(explicitTypeMappingByAddress, dynamic)
				conflictingTypeAddresses[dynamic] = true
			}
			continue
		}
		explicitTypeMappingByAddress[dynamic] = len(explicitTypeMappings)
		explicitTypeMappings = append(explicitTypeMappings, runtime.JITTypeMapping{Dynamic: dynamic, Canonical: canonical})
	}
	if len(conflictingTypeAddresses) != 0 {
		filtered := explicitTypeMappings[:0]
		for _, mapping := range explicitTypeMappings {
			if mapping.Dynamic != 0 {
				filtered = append(filtered, mapping)
			}
		}
		explicitTypeMappings = filtered
	}
	mapped.typeMappings = append([]runtime.JITTypeMapping(nil), explicitTypeMappings...)
	collectTypeDescriptors := func() {
		mapped.typeDescriptors = nil
		seenTypeDescriptors := make(map[uintptr]struct{})
		for i, symbol := range image.Symbols {
			if symbol.Kind == ImageType && uintptr(symbol.Size) >= runtime.JITTypeDescriptorSize() {
				descriptor := mapped.symbols[i]
				if _, duplicate := seenTypeDescriptors[descriptor]; !duplicate {
					seenTypeDescriptors[descriptor] = struct{}{}
					mapped.typeDescriptors = append(mapped.typeDescriptors, descriptor)
				}
			}
		}
	}
	collectTypeDescriptors()
	if len(mapped.typeDescriptors) != 0 {
		if typesStart == 0 || typesEnd <= typesStart {
			return nil, fmt.Errorf("jit: type descriptors have no read-only metadata range")
		}
		descriptors := make([]unsafe.Pointer, len(mapped.typeDescriptors))
		for i, descriptor := range mapped.typeDescriptors {
			descriptors[i] = unsafe.Pointer(descriptor)
		}
		canonicalMappings, canonicalErr := runtime.JITCanonicalizeTypes(guard, typesStart, typesEnd-typesStart, descriptors, explicitTypeMappings)
		if canonicalErr != nil {
			return nil, fmt.Errorf("jit: canonicalize image types: %w", canonicalErr)
		}
		if !typeMappingsRelocationsFit(image, mapped.symbols, sectionAddr, canonicalMappings) {
			// The first reservation let the runtime inspect the relocated type
			// graph. Rebase once near its discovered canonical descriptors so every
			// direct PC-relative type reference can use the same identity.
			oldSymbolsForAddress := make(map[uintptr][]uint32, len(image.Symbols))
			for i, symbol := range image.Symbols {
				oldSymbolsForAddress[mapped.symbols[i]] = append(oldSymbolsForAddress[mapped.symbols[i]], uint32(i))
			}
			canonicalTargets := make(map[uint32]uintptr, len(canonicalMappings))
			for _, alias := range canonicalMappings {
				symbols := oldSymbolsForAddress[alias.Dynamic]
				if len(symbols) == 0 {
					return nil, fmt.Errorf("jit: canonical type descriptor %#x has no image symbol", alias.Dynamic)
				}
				if len(oldSymbolsForAddress[alias.Canonical]) == 0 {
					for _, symbol := range symbols {
						canonicalTargets[symbol] = alias.Canonical
					}
				}
			}
			if unmapErr := mapped.runtimeMapping.Unmap(); unmapErr != nil {
				return nil, fmt.Errorf("jit: release staging image before type rebase: %w", unmapErr)
			}
			mapped.runtimeMapping = nil
			mapped.mapping = nil
			mapped.sections = nil
			mapping, runtimeMapping, mapErr = mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset, nil, canonicalTargets)
			if isJITMemoryLimitError(mapErr) {
				runtime.GC()
				if runtime.JITWaitForRetirements(5e9) {
					mapping, runtimeMapping, mapErr = mapImageNearImports(totalSize, pageSize, image, importAddresses, sectionOffset, nil, canonicalTargets)
				}
			}
			if mapErr != nil {
				return nil, mapErr
			}
			mapped.mapping = mapping
			mapped.runtimeMapping = runtimeMapping
			mapped.control = uintptrPointer(mapping[:pageSize])
			installMappedSections()
			for i := range canonicalMappings {
				oldDynamic := canonicalMappings[i].Dynamic
				oldCanonical := canonicalMappings[i].Canonical
				symbols := oldSymbolsForAddress[oldDynamic]
				if len(symbols) == 0 {
					return nil, fmt.Errorf("jit: canonical type descriptor %#x lost its image symbol", oldDynamic)
				}
				canonicalMappings[i].Dynamic = mapped.symbols[symbols[0]]
				if canonicalSymbols := oldSymbolsForAddress[oldCanonical]; len(canonicalSymbols) != 0 {
					canonicalSymbol := canonicalSymbols[0]
					canonicalMappings[i].Canonical = mapped.symbols[canonicalSymbol]
				}
			}
		}
		mapped.typeMappings = canonicalMappings
		collectTypeDescriptors()
		clear(canonicalTypeAddresses)
		for _, alias := range canonicalMappings {
			canonicalTypeAddresses[alias.Dynamic] = alias.Canonical
		}
		if err := applyRelocations(); err != nil {
			return nil, err
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

func typeMappingsRelocationsFit(image ModuleImage, symbols, sectionAddr []uintptr, mappings []runtime.JITTypeMapping) bool {
	byAddress := make(map[uintptr]int, len(mappings))
	for i, mapping := range mappings {
		byAddress[mapping.Dynamic] = i
	}
	for _, reloc := range image.Relocations {
		if reloc.TargetImport || int(reloc.Target) >= len(symbols) {
			continue
		}
		mappingIndex, aliased := byAddress[symbols[reloc.Target]]
		if !aliased || reloc.Kind == RelocAMD64Addroff32 || reloc.Kind == RelocAMD64TLSLE32 {
			continue
		}
		value, ok := addSignedAddress(mappings[mappingIndex].Canonical, reloc.Addend)
		if !ok {
			return false
		}
		switch reloc.Kind {
		case RelocAMD64Abs32:
			if uint64(value) > uint64(^uint32(0)) {
				return false
			}
		case RelocAMD64PC32, RelocAMD64PLT32, RelocAMD64GOTPCREL, RelocAMD64GOTPCRELX:
			place := sectionAddr[reloc.Section] + uintptr(reloc.Offset)
			delta := int64(value) - (int64(place) + 4)
			if delta < -(1<<31) || delta > (1<<31)-1 {
				return false
			}
		}
	}
	return true
}

func isJITMemoryLimitError(err error) bool {
	var limitError runtime.JITMemoryLimitError
	return errors.As(err, &limitError)
}

func mapImageNearImports(totalSize, pageSize uint64, image ModuleImage, imports []uintptr, sectionOffset []uint64, legacyTypeImports map[uint32]uint32, canonicalTargets map[uint32]uintptr) ([]byte, *runtime.JITMapping, error) {
	low, high := int64(0x10000), int64(0x00007ffffffff000-totalSize)
	hasPCImport := false
	for _, reloc := range image.Relocations {
		if reloc.Kind != RelocAMD64PC32 && reloc.Kind != RelocAMD64PLT32 &&
			reloc.Kind != RelocAMD64GOTPCREL && reloc.Kind != RelocAMD64GOTPCRELX {
			continue
		}
		var target uintptr
		if reloc.TargetImport {
			if int(reloc.Target) >= len(imports) {
				return nil, nil, fmt.Errorf("jit: PC-relative relocation has invalid import index %d", reloc.Target)
			}
			target = imports[reloc.Target]
		} else if importIndex, canonical := legacyTypeImports[reloc.Target]; canonical {
			if int(importIndex) >= len(imports) {
				return nil, nil, fmt.Errorf("jit: PC-relative relocation has invalid type import index %d", importIndex)
			}
			target = imports[importIndex]
		} else if canonical, ok := canonicalTargets[reloc.Target]; ok {
			target = canonical
		} else {
			continue
		}
		hasPCImport = true
		value, ok := addSignedAddress(target, reloc.Addend)
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
		return nil, nil, fmt.Errorf("jit: canonical targets have no common amd64 PC-relative mapping range")
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
		// MAP_FIXED_NOREPLACE reports EEXIST when this address is occupied.
		// Other errors, including EINVAL, indicate an invalid request or an
		// unsupported mapping mode and must not be mistaken for address
		// collisions.
		if !errors.Is(err, syscall.EEXIST) {
			return nil, nil, fmt.Errorf("jit: map image near host symbols: %w", err)
		}
	}
	return nil, nil, fmt.Errorf("jit: no free amd64 mapping address satisfies PC-relative relocations in [%#x,%#x]", low, high)
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
	mapped.typeMappings = nil
	mapped.typeDescriptors = nil
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
