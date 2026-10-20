// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"unsafe"
)

const (
	linkManifestHeaderSize  = 48
	linkManifestPackageSize = 16
	linkManifestSymbolSize  = 40
	linkManifestMaxSize     = 256 << 20
	linkManifestMaxEntries  = 1 << 20
)

var linkManifestMagic = [8]byte{'G', 'O', 'J', 'I', 'T', 'M', 'F', '2'}

type linkerManifestPackage struct {
	path        string
	fingerprint [8]byte
}

// loadLinkerManifest validates and decodes the relocation-backed symbol table
// produced by cmd/link. Addresses in this table are process addresses from the
// final executable, not symbol values reconstructed from archives.
func loadLinkerManifest() ([]linkerManifestPackage, []LinkedSymbol, error) {
	data := runtime.JITManifest()
	if len(data) < linkManifestHeaderSize || len(data) > linkManifestMaxSize {
		return nil, nil, fmt.Errorf("jit: invalid linker manifest size %d", len(data))
	}
	if string(data[:8]) != string(linkManifestMagic[:]) {
		return nil, nil, fmt.Errorf("jit: invalid linker manifest magic")
	}
	if binary.LittleEndian.Uint32(data[8:]) != 2 || data[12] != uint8(unsafe.Sizeof(uintptr(0))) || data[13] != 1 || data[14] != 1 || data[15] != 0 {
		return nil, nil, fmt.Errorf("jit: unsupported linker manifest version or target")
	}
	packageCount := uint64(binary.LittleEndian.Uint32(data[16:]))
	symbolCount := uint64(binary.LittleEndian.Uint32(data[20:]))
	packageOffset := uint64(binary.LittleEndian.Uint32(data[24:]))
	symbolOffset := uint64(binary.LittleEndian.Uint32(data[28:]))
	stringsOffset := uint64(binary.LittleEndian.Uint32(data[32:]))
	totalSize := uint64(binary.LittleEndian.Uint32(data[36:]))
	versionOffset := uint64(binary.LittleEndian.Uint32(data[40:]))
	versionLength := uint64(binary.LittleEndian.Uint32(data[44:]))
	if packageCount == 0 || packageCount > linkManifestMaxEntries || symbolCount == 0 || symbolCount > linkManifestMaxEntries {
		return nil, nil, fmt.Errorf("jit: linker manifest entry count is invalid")
	}
	wantSymbolOffset := uint64(linkManifestHeaderSize) + packageCount*linkManifestPackageSize
	wantStringsOffset := wantSymbolOffset + symbolCount*linkManifestSymbolSize
	if packageOffset != linkManifestHeaderSize || symbolOffset != wantSymbolOffset || stringsOffset != wantStringsOffset ||
		totalSize != uint64(len(data)) || stringsOffset > totalSize {
		return nil, nil, fmt.Errorf("jit: linker manifest offsets are inconsistent")
	}
	compilerVersion, err := linkerManifestString(data, versionOffset, versionLength, stringsOffset, totalSize)
	if err != nil {
		return nil, nil, fmt.Errorf("jit: invalid linker manifest compiler version: %w", err)
	}
	if compilerVersion != runtime.Version() {
		return nil, nil, fmt.Errorf("jit: linker manifest compiler %q does not match runtime %q", compilerVersion, runtime.Version())
	}

	packages := make([]linkerManifestPackage, int(packageCount))
	for i := range packages {
		off := packageOffset + uint64(i)*linkManifestPackageSize
		pathOffset := uint64(binary.LittleEndian.Uint32(data[off:]))
		pathLength := uint64(binary.LittleEndian.Uint32(data[off+4:]))
		pkgPath, err := linkerManifestString(data, pathOffset, pathLength, stringsOffset, totalSize)
		if err != nil {
			return nil, nil, fmt.Errorf("jit: invalid linker manifest package %d: %w", i, err)
		}
		packages[i].path = pkgPath
		copy(packages[i].fingerprint[:], data[off+8:off+16])
		if packages[i].fingerprint == [8]byte{} || (i > 0 && packages[i-1].path >= pkgPath) {
			return nil, nil, fmt.Errorf("jit: linker manifest package identities are empty, duplicated, or unsorted")
		}
	}

	symbols := make([]LinkedSymbol, int(symbolCount))
	for i := range symbols {
		off := symbolOffset + uint64(i)*linkManifestSymbolSize
		pkgIndex := uint64(binary.LittleEndian.Uint32(data[off:]))
		nameOffset := uint64(binary.LittleEndian.Uint32(data[off+4:]))
		nameLength := uint64(binary.LittleEndian.Uint32(data[off+8:]))
		abi := binary.LittleEndian.Uint16(data[off+12:])
		kind := data[off+14]
		flags := data[off+15]
		address := binary.LittleEndian.Uint64(data[off+16:])
		typeAddress := binary.LittleEndian.Uint64(data[off+24:])
		reserved := binary.LittleEndian.Uint64(data[off+32:])
		if pkgIndex >= packageCount || abi > 1 || kind < 1 || kind > 4 || flags&^uint8(HostEffectUnknown|HostEffectUnsafe|HostEffectForeign|HostEffectCodePointer|HostEffectRetainsCallbacks|HostEffectInterfaceCall) != 0 || (kind != 1 && flags != 0) || reserved != 0 || address == 0 || uint64(uintptr(address)) != address {
			return nil, nil, fmt.Errorf("jit: invalid linker manifest symbol record %d", i)
		}
		name, err := linkerManifestString(data, nameOffset, nameLength, stringsOffset, totalSize)
		if err != nil {
			return nil, nil, fmt.Errorf("jit: invalid linker manifest symbol name %d: %w", i, err)
		}
		linkKind := LinkKind("")
		switch kind {
		case 1:
			linkKind = FunctionLink
		case 2:
			linkKind = VariableLink
		case 3:
			linkKind = TypeLink
		case 4:
			linkKind = ITableLink
		}
		var typePtr uintptr
		if uint64(uintptr(typeAddress)) != typeAddress {
			return nil, nil, fmt.Errorf("jit: linker manifest type address %d overflows uintptr", i)
		}
		typePtr = uintptr(typeAddress)
		var reflectedType reflect.Type
		if linkKind != ITableLink {
			reflectedType = linkerManifestType(typePtr)
		}
		symbols[i] = LinkedSymbol{
			PackagePath:  packages[pkgIndex].path,
			Name:         name,
			Kind:         linkKind,
			ABI:          abi,
			Effects:      HostEffect(flags),
			Address:      uintptr(address),
			TypeAddress:  typePtr,
			Type:         reflectedType,
			FromManifest: true,
		}
		if i > 0 {
			previous := symbols[i-1]
			current := symbols[i]
			if previous.PackagePath == current.PackagePath && previous.Name == current.Name && previous.ABI == current.ABI {
				return nil, nil, fmt.Errorf("jit: linker manifest has ambiguous symbol identity %s.%s ABI %d", current.PackagePath, current.Name, current.ABI)
			}
			if !linkerManifestSymbolLess(previous, current) {
				return nil, nil, fmt.Errorf("jit: linker manifest symbols are unsorted")
			}
		}
	}
	return packages, symbols, nil
}

// linkerManifestType reconstructs the reflect.Type interface for a linker
// selected runtime type descriptor. The descriptor belongs to the static
// executable and remains mapped for process life. The interface table is
// taken from an ordinary reflect.Type value of the same concrete runtime
// implementation; only its data word is replaced.
func linkerManifestType(address uintptr) reflect.Type {
	if address == 0 {
		return nil
	}
	typ := reflect.TypeOf(0)
	typeWords := *(*[2]unsafe.Pointer)(unsafe.Pointer(&typ))
	typeWords[1] = unsafe.Pointer(address)
	return *(*reflect.Type)(unsafe.Pointer(&typeWords))
}

func linkerManifestString(data []byte, offset, length, stringsOffset, totalSize uint64) (string, error) {
	if length == 0 || offset < stringsOffset || offset > totalSize || length > totalSize-offset {
		return "", fmt.Errorf("string range is out of bounds")
	}
	value := string(data[offset : offset+length])
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("string contains NUL")
	}
	return value, nil
}

func linkerManifestSymbolLess(a, b LinkedSymbol) bool {
	if a.PackagePath != b.PackagePath {
		return a.PackagePath < b.PackagePath
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.ABI != b.ABI {
		return a.ABI < b.ABI
	}
	return linkerManifestKindRank(a.Kind) < linkerManifestKindRank(b.Kind)
}

func linkerManifestKindRank(kind LinkKind) uint8 {
	switch kind {
	case FunctionLink:
		return 1
	case VariableLink:
		return 2
	case TypeLink:
		return 3
	case ITableLink:
		return 4
	default:
		return 0
	}
}
