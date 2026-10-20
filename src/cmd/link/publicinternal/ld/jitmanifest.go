// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"cmd/link/publicinternal/loader"
	"cmd/link/publicinternal/sym"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"publicinternal/buildcfg"
)

const (
	jitManifestVersion      = 1
	jitManifestHeaderSize   = 48
	jitManifestPackageSize  = 16
	jitManifestSymbolSize   = 40
	jitManifestKindFunction = 1
	jitManifestKindVariable = 2
	jitManifestKindType     = 3
	jitManifestKindITable   = 4
)

var jitManifestMagic = [8]byte{'G', 'O', 'J', 'I', 'T', 'M', 'F', '1'}

type jitManifestSymbol struct {
	pkg     string
	name    string
	abi     uint16
	kind    uint8
	sym     loader.Sym
	typeSym loader.Sym
}

// buildJITManifest emits the post-deadcode symbol catalog as ordinary
// relocation-backed read-only data. The package roots have already been
// validated and marked by deadcodePass; every address below is therefore the
// exact symbol selected by the normal Go linker.
func (ctxt *Link) buildJITManifest() error {
	if len(ctxt.jitPackages) == 0 {
		return nil
	}
	if !ctxt.IsLinux() || !ctxt.IsAMD64() || ctxt.LinkMode != LinkInternal || ctxt.linkShared ||
		(ctxt.BuildMode != BuildModeExe && ctxt.BuildMode != BuildModePIE) || ctxt.Arch.PtrSize != 8 {
		return fmt.Errorf("JIT symbol manifest requires a statically linked Linux/amd64 executable")
	}
	ldr := ctxt.loader
	packages := make([]string, 0, len(ctxt.jitPackages))
	for pkg := range ctxt.jitPackages {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	packageIndex := make(map[string]uint32, len(packages))
	for i, pkg := range packages {
		lib := ctxt.LibraryByPkg[pkg]
		if lib == nil || lib.Fingerprint.IsZero() {
			return fmt.Errorf("JIT package %q has no selected package fingerprint", pkg)
		}
		packageIndex[pkg] = uint32(i)
	}

	entries := make([]jitManifestSymbol, 0)
	for i := 1; i < ldr.NDef(); i++ {
		s := loader.Sym(i)
		if !ldr.AttrReachable(s) {
			continue
		}
		// NDef includes symbols from every input object. Duplicate-ok
		// definitions can therefore appear more than once even though the
		// normal linker has selected one definition for relocations. Export
		// only that selected identity; otherwise a package containing generic
		// code (or a closure emitted with it) can produce duplicate manifest
		// keys for the same source-level symbol.
		if selected := ldr.Lookup(ldr.SymName(s), ldr.SymVersion(s)); selected != s {
			continue
		}
		pkg, name := jitManifestIdentity(ldr.SymPkg(s), ldr.SymName(s), ctxt.jitPackages)
		if !ctxt.jitPackages[pkg] {
			continue
		}
		if name == "" && ldr.IsGoType(s) {
			// Unnamed type descriptors are emitted in a type namespace whose
			// linker name has no package prefix (for example,
			// "type:func(int) string"). They still have one selected descriptor
			// in the final executable and are needed to canonicalize structural
			// types produced by runtime compilation. Key them by their exact Go
			// type spelling inside the package that contributed the selected
			// object. The runtime importer only treats descriptors with an empty
			// reflect.Type.Name as structural aliases.
			name = jitManifestUnnamedTypeName(ldr.SymName(s))
		}
		if name == "" {
			continue
		}
		abi, ok := jitManifestABI(ldr.SymVersion(s))
		if !ok {
			continue // file-local and linker-private ABI versions are not import identities
		}
		kind := uint8(0)
		switch {
		case ldr.IsGoType(s):
			kind = jitManifestKindType
		case ldr.IsItab(s):
			kind = jitManifestKindITable
		case ldr.SymType(s).IsText():
			kind = jitManifestKindFunction
		case ldr.SymType(s).IsData():
			kind = jitManifestKindVariable
		default:
			continue
		}
		typeSym := ldr.SymGoType(s)
		if kind == jitManifestKindType {
			typeSym = s
		}
		entries = append(entries, jitManifestSymbol{
			pkg: pkg, name: name, abi: abi, kind: kind, sym: s, typeSym: typeSym,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.pkg != b.pkg {
			return a.pkg < b.pkg
		}
		if a.name != b.name {
			return a.name < b.name
		}
		if a.abi != b.abi {
			return a.abi < b.abi
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.sym < b.sym
	})
	if len(entries) == 0 {
		return fmt.Errorf("JIT package closure has no linkable symbols")
	}
	for i := 1; i < len(entries); i++ {
		prev, current := entries[i-1], entries[i]
		if prev.pkg == current.pkg && prev.name == current.name && prev.abi == current.abi {
			if prev.sym != current.sym || prev.kind != current.kind {
				return fmt.Errorf("ambiguous linked JIT symbol %s.%s (ABI %d)", current.pkg, current.name, current.abi)
			}
			return fmt.Errorf("duplicate linked JIT symbol %s.%s (ABI %d)", current.pkg, current.name, current.abi)
		}
	}
	if len(packages) > 1<<16 || len(entries) > 1<<20 {
		return fmt.Errorf("JIT symbol manifest exceeds entry limits")
	}

	packageOffset := uint32(jitManifestHeaderSize)
	symbolOffset := packageOffset + uint32(len(packages))*jitManifestPackageSize
	stringOffset := symbolOffset + uint32(len(entries))*jitManifestSymbolSize
	if uint64(stringOffset) > uint64(^uint32(0)) {
		return fmt.Errorf("JIT symbol manifest exceeds 4 GiB")
	}
	stringsData := make([]byte, 0)
	addString := func(value string) (uint32, uint32, error) {
		if value == "" || strings.IndexByte(value, 0) >= 0 {
			return 0, 0, fmt.Errorf("invalid empty or NUL-containing JIT manifest string")
		}
		stringBytes := uint64(len(stringsData)) + uint64(len(value))
		if stringBytes > uint64(^uint32(0))-uint64(stringOffset) {
			return 0, 0, fmt.Errorf("JIT symbol manifest exceeds 4 GiB")
		}
		if uint64(stringOffset)+stringBytes > 256<<20 {
			return 0, 0, fmt.Errorf("JIT symbol manifest exceeds size limit")
		}
		off := stringOffset + uint32(len(stringsData))
		stringsData = append(stringsData, value...)
		return off, uint32(len(value)), nil
	}
	versionOffset, versionLength, err := addString(buildcfg.Version)
	if err != nil {
		return err
	}
	packageStrings := make([]struct{ off, size uint32 }, len(packages))
	for i, pkg := range packages {
		off, size, err := addString(pkg)
		if err != nil {
			return err
		}
		packageStrings[i] = struct{ off, size uint32 }{off, size}
	}
	entryStrings := make([]struct{ off, size uint32 }, len(entries))
	for i, entry := range entries {
		off, size, err := addString(entry.name)
		if err != nil {
			return err
		}
		entryStrings[i] = struct{ off, size uint32 }{off, size}
	}
	totalSize := uint64(stringOffset) + uint64(len(stringsData))
	if totalSize > 256<<20 || totalSize > uint64(^uint32(0)) {
		return fmt.Errorf("JIT symbol manifest exceeds size limit")
	}
	blob := make([]byte, int(totalSize))
	copy(blob[:8], jitManifestMagic[:])
	binary.LittleEndian.PutUint32(blob[8:], jitManifestVersion)
	blob[12], blob[13], blob[14] = uint8(ctxt.Arch.PtrSize), 1, 1 // 64-bit, Linux, AMD64
	binary.LittleEndian.PutUint32(blob[16:], uint32(len(packages)))
	binary.LittleEndian.PutUint32(blob[20:], uint32(len(entries)))
	binary.LittleEndian.PutUint32(blob[24:], packageOffset)
	binary.LittleEndian.PutUint32(blob[28:], symbolOffset)
	binary.LittleEndian.PutUint32(blob[32:], stringOffset)
	binary.LittleEndian.PutUint32(blob[36:], uint32(totalSize))
	binary.LittleEndian.PutUint32(blob[40:], versionOffset)
	binary.LittleEndian.PutUint32(blob[44:], versionLength)
	for i, pkg := range packages {
		off := int(packageOffset) + i*jitManifestPackageSize
		binary.LittleEndian.PutUint32(blob[off:], packageStrings[i].off)
		binary.LittleEndian.PutUint32(blob[off+4:], packageStrings[i].size)
		copy(blob[off+8:], ctxt.LibraryByPkg[pkg].Fingerprint[:])
	}
	for i, entry := range entries {
		off := int(symbolOffset) + i*jitManifestSymbolSize
		binary.LittleEndian.PutUint32(blob[off:], packageIndex[entry.pkg])
		binary.LittleEndian.PutUint32(blob[off+4:], entryStrings[i].off)
		binary.LittleEndian.PutUint32(blob[off+8:], entryStrings[i].size)
		binary.LittleEndian.PutUint16(blob[off+12:], entry.abi)
		blob[off+14] = entry.kind
	}
	copy(blob[stringOffset:], stringsData)

	manifest := ldr.CreateSymForUpdate("runtime.jitmanifestdata", 0)
	manifest.SetType(sym.SRODATA)
	manifest.SetAlign(int32(ctxt.Arch.PtrSize))
	manifest.SetData(blob)
	manifest.SetSize(int64(len(blob)))
	for i, entry := range entries {
		off := int32(symbolOffset) + int32(i*jitManifestSymbolSize)
		addrReloc, _ := manifest.AddRel(objabi.R_ADDR)
		addrReloc.SetOff(off + 16)
		addrReloc.SetSiz(uint8(ctxt.Arch.PtrSize))
		addrReloc.SetSym(entry.sym)
		if entry.typeSym != 0 {
			typeReloc, _ := manifest.AddRel(objabi.R_ADDR)
			typeReloc.SetOff(off + 24)
			typeReloc.SetSiz(uint8(ctxt.Arch.PtrSize))
			typeReloc.SetSym(entry.typeSym)
		}
	}

	manifestVar := ldr.Lookup("runtime.jitManifestBytes", 0)
	if manifestVar == 0 {
		return fmt.Errorf("runtime.JITManifest data variable was removed before linker manifest generation")
	}
	manifestSlice := ldr.MakeSymbolUpdater(manifestVar)
	manifestSlice.SetType(sym.SDATA)
	manifestSlice.SetAlign(int32(ctxt.Arch.PtrSize))
	manifestSlice.SetSize(0)
	manifestSlice.SetData(nil)
	manifestSlice.ResetRelocs()
	manifestSlice.AddAddr(ctxt.Arch, manifest.Sym())
	manifestSlice.AddUint(ctxt.Arch, uint64(len(blob)))
	manifestSlice.AddUint(ctxt.Arch, uint64(len(blob)))
	return nil
}

func jitManifestName(pkg, symbol string) string {
	prefix := objabi.PathToPrefix(pkg) + "."
	if strings.HasPrefix(symbol, "type:"+prefix) {
		return strings.TrimPrefix(symbol, "type:"+prefix)
	}
	if strings.HasPrefix(symbol, prefix) {
		return strings.TrimPrefix(symbol, prefix)
	}
	return ""
}

// jitManifestIdentity derives the package identity exposed to compiled source
// from the linker symbol's fully qualified name. Most symbols are owned by
// their source package, but //go:linkname can place a symbol in one package's
// object while giving it another package's public linker name. For example,
// runtime/debug.SetTraceback is implemented in runtime/runtime1.go. Prefer
// the longest included package prefix so nested import paths remain
// unambiguous, and fall back to the owning package for ordinary and unnamed
// symbols.
func jitManifestIdentity(ownerPkg, symbol string, included map[string]bool) (string, string) {
	qualified := symbol
	typePrefix := ""
	if strings.HasPrefix(qualified, "type:") {
		typePrefix = "type:"
		qualified = strings.TrimPrefix(qualified, typePrefix)
	}
	bestPkg, bestPrefix := "", ""
	for pkg := range included {
		prefix := objabi.PathToPrefix(pkg) + "."
		if strings.HasPrefix(qualified, prefix) && len(prefix) > len(bestPrefix) {
			bestPkg, bestPrefix = pkg, prefix
		}
	}
	if bestPkg != "" {
		return bestPkg, strings.TrimPrefix(qualified, bestPrefix)
	}
	name := jitManifestName(ownerPkg, symbol)
	if name == "" {
		return ownerPkg, ""
	}
	return ownerPkg, name
}

func jitManifestUnnamedTypeName(symbol string) string {
	if !strings.HasPrefix(symbol, "type:") {
		return ""
	}
	return strings.TrimPrefix(symbol, "type:")
}

func jitManifestABI(version int) (uint16, bool) {
	abi0Version := sym.ABIToVersion(obj.ABI0)
	internalVersion := sym.ABIToVersion(obj.ABIInternal)
	if internalVersion != abi0Version && version == internalVersion {
		return uint16(obj.ABIInternal), true
	}
	if version == abi0Version {
		return uint16(obj.ABI0), true
	}
	return 0, false
}
