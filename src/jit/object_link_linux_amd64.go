// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"cmd/publicinternal/goobj"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
)

type objectKey struct {
	object int
	ref    goobj.SymRef
}

type objectDefinition struct {
	key    objectKey
	symbol ObjectSymbol
}

type objectLocation struct {
	section ImageSectionKind
	offset  uint64
}

type pendingObjectRelocation struct {
	source       int
	relocation   ObjectRelocation
	targetLocal  int
	targetImport int
	imported     bool
	discard      bool
}

type pendingGlobalRoot struct {
	dataDef    int
	typeDef    int
	typeImport int
	imported   bool
}

type gotTargetKey struct {
	local       int
	importIndex int
	addend      int64
	remote      bool
}

type gotTarget struct {
	key    gotTargetKey
	offset uint64
}

func gotKey(relocation pendingObjectRelocation) gotTargetKey {
	key := gotTargetKey{addend: relocation.relocation.Addend}
	if relocation.imported {
		key.remote, key.importIndex = true, relocation.targetImport
	} else {
		key.local = relocation.targetLocal
	}
	return key
}

func objectRelocType(relocation ObjectRelocation) objabi.RelocType {
	return objabi.RelocType(relocation.Type) &^ objabi.R_WEAK
}

type imageSectionBuilder struct {
	kind    ImageSectionKind
	align   uint32
	memSize uint64
	data    []byte
}

// LinkObjectArchive links the object closure reachable from entryName into a
// pointer-free ModuleImage. Local symbols are laid out by section, supported
// relocations are applied by the mapper, and imported symbols resolve through
// the host link manifest. The linker rejects symbol forms and relocations it
// cannot preserve exactly.
func LinkObjectArchive(objectArchive []byte, entryName string, host *Metadata) (ModuleImage, error) {
	objects, err := ParseObjectArchive(objectArchive)
	if err != nil {
		return ModuleImage{}, err
	}
	definitions := make([]objectDefinition, 0)
	definitionByRef := make(map[objectKey]int)
	entryDef := -1
	packagePath := ""
	for objectIndex := range objects {
		object := &objects[objectIndex]
		symbols, err := object.Symbols()
		if err != nil {
			return ModuleImage{}, err
		}
		for _, symbol := range symbols {
			key := objectKey{object: objectIndex, ref: symbol.Ref}
			if _, exists := definitionByRef[key]; exists {
				return ModuleImage{}, fmt.Errorf("jit: object %q defines duplicate local reference %+v", object.Name, symbol.Ref)
			}
			definitionByRef[key] = len(definitions)
			definitions = append(definitions, objectDefinition{key: key, symbol: symbol})
			if objabi.SymKind(symbol.Type).IsText() && strings.HasSuffix(symbol.Name, "."+entryName) {
				if entryDef >= 0 {
					return ModuleImage{}, fmt.Errorf("jit: object archive defines more than one text symbol named %q", entryName)
				}
				entryDef = len(definitions) - 1
				packagePath = strings.TrimSuffix(symbol.Name, "."+entryName)
			}
		}
	}
	if entryDef < 0 {
		return ModuleImage{}, fmt.Errorf("jit: object archive has no text symbol named %q", entryName)
	}
	if packagePath == "" {
		return ModuleImage{}, fmt.Errorf("jit: entry %q has no package path", entryName)
	}

	selected := make(map[int]bool)
	var visitOrder []int
	queue := []int{entryDef}
	imports := make([]ImageImport, 0)
	importIndex := make(map[imageImportKey]int)
	addImport := func(imp ImageImport) int {
		key := imageImportKey{packagePath: imp.PackagePath, name: imp.Name, kind: imp.Kind, callingABI: imp.ABI, abiHash: imp.ABIHash}
		if index, ok := importIndex[key]; ok {
			return index
		}
		index := len(imports)
		imports = append(imports, imp)
		importIndex[key] = index
		return index
	}
	// Same-package declaration stubs have local object symbols but represent
	// storage and code that already belong to the host package. Bind those
	// definitions to the host manifest before walking reachability so the JIT
	// image never creates a second package variable or function.
	hostDefinitions := make(map[int]int)
	unnamedTypeAliases := make(map[int]int)
	if host != nil {
		for index, definition := range definitions {
			if name, ok := hostPackageSymbolName(packagePath, definition.symbol.Name); ok {
				linked, found := host.LookupLinkerSymbol(packagePath, name, definition.symbol.ABI)
				if !found {
					continue
				}
				imp, _, err := hostImport(host, ObjectReference{PackagePath: linked.PackagePath, Name: linked.Name, ABI: linked.ABI})
				if err != nil {
					return ModuleImage{}, fmt.Errorf("jit: bind same-package symbol %s.%s: %w", linked.PackagePath, linked.Name, err)
				}
				hostDefinitions[index] = addImport(imp)
				continue
			}
			if kind, _, err := imageSymbolSection(definition.symbol); err == nil && kind == ImageType {
				// Unnamed type symbols (notably method function signatures) are
				// structurally canonical in Go. Keep the local descriptor so its
				// module-relative offsets remain valid, and record the canonical
				// host type for runtime typeOff resolution.
				name := strings.TrimPrefix(strings.TrimPrefix(definition.symbol.Name, "type:"), "type.")
				linked, found, err := host.lookupUnnamedType(name)
				if err != nil {
					return ModuleImage{}, err
				}
				if found {
					imp, _, err := hostImport(host, ObjectReference{PackagePath: linked.PackagePath, Name: linked.Name, ABI: linked.ABI})
					if err != nil {
						return ModuleImage{}, fmt.Errorf("jit: bind canonical type %s.%s: %w", linked.PackagePath, linked.Name, err)
					}
					unnamedTypeAliases[index] = addImport(imp)
				}
			}
		}
	}
	var pending []pendingObjectRelocation
	var pendingGlobalRoots []pendingGlobalRoot
	for cursor := 0; cursor < len(queue); cursor++ {
		defIndex := queue[cursor]
		if selected[defIndex] {
			continue
		}
		if _, hostBound := hostDefinitions[defIndex]; hostBound {
			continue
		}
		selected[defIndex] = true
		visitOrder = append(visitOrder, defIndex)
		definition := definitions[defIndex]
		object := objects[definition.key.object]
		if isPointerfulGlobal(definition.symbol.Type) {
			if !definition.symbol.HasGotype {
				return ModuleImage{}, fmt.Errorf("jit: writable Go global %s has no compiler GC type metadata", definition.symbol.Name)
			}
			ref, err := object.ResolveReference(definition.symbol.Gotype)
			if err != nil {
				return ModuleImage{}, fmt.Errorf("jit: resolve GC type for %s: %w", definition.symbol.Name, err)
			}
			if ref.Local {
				typeDef, ok := definitionByRef[objectKey{object: definition.key.object, ref: ref.Ref}]
				if !ok {
					return ModuleImage{}, fmt.Errorf("jit: writable Go global %s references missing GC type %s", definition.symbol.Name, ref.Name)
				}
				if typeImport, hostBound := hostDefinitions[typeDef]; hostBound {
					pendingGlobalRoots = append(pendingGlobalRoots, pendingGlobalRoot{dataDef: defIndex, typeImport: typeImport, imported: true})
				} else {
					queue = append(queue, typeDef)
					pendingGlobalRoots = append(pendingGlobalRoots, pendingGlobalRoot{dataDef: defIndex, typeDef: typeDef})
				}
			} else {
				imp, _, err := hostImport(host, ref)
				if err != nil {
					return ModuleImage{}, fmt.Errorf("jit: resolve GC type for %s: %w", definition.symbol.Name, err)
				}
				pendingGlobalRoots = append(pendingGlobalRoots, pendingGlobalRoot{dataDef: defIndex, typeImport: addImport(imp), imported: true})
			}
		}
		if objabi.SymKind(definition.symbol.Type).IsText() {
			for _, ref := range definition.symbol.Function.FuncDataRefs {
				if ref.PkgIdx == goobj.PkgIdxInvalid {
					continue
				}
				target, ok := definitionByRef[objectKey{object: definition.key.object, ref: ref}]
				if !ok {
					return ModuleImage{}, fmt.Errorf("jit: %s references missing funcdata symbol %+v", definition.symbol.Name, ref)
				}
				queue = append(queue, target)
			}
		}
		for _, relocation := range definition.symbol.Relocations {
			rtype := objabi.RelocType(relocation.Type)
			weak := rtype&objabi.R_WEAK != 0
			rtype &^= objabi.R_WEAK
			if rtype == objabi.R_TLS_LE {
				if weak || relocation.Target.PkgIdx != goobj.PkgIdxInvalid || relocation.Target.SymIdx != 0 {
					return ModuleImage{}, fmt.Errorf("jit: %s has malformed TLS local-exec relocation", definition.symbol.Name)
				}
				if _, err := imageRelocationFor(rtype, relocation.Size); err != nil {
					return ModuleImage{}, fmt.Errorf("jit: %s: %w", definition.symbol.Name, err)
				}
				pending = append(pending, pendingObjectRelocation{source: defIndex, relocation: relocation})
				continue
			}
			ref, err := object.ResolveReference(relocation.Target)
			if err != nil {
				return ModuleImage{}, fmt.Errorf("jit: %s relocation at offset %d: %w", definition.symbol.Name, relocation.Offset, err)
			}
			if relocation.Size == 0 {
				if !isMarkerRelocation(rtype) {
					return ModuleImage{}, fmt.Errorf("jit: %s has unsupported zero-width relocation type %s", definition.symbol.Name, rtype)
				}
				if ref.Local {
					target, ok := definitionByRef[objectKey{object: definition.key.object, ref: ref.Ref}]
					if !ok {
						return ModuleImage{}, fmt.Errorf("jit: %s marker relocation references missing local symbol %s", definition.symbol.Name, ref.Name)
					}
					queue = append(queue, target)
				}
				continue
			}
			if _, err := imageRelocationFor(rtype, relocation.Size); err != nil {
				return ModuleImage{}, fmt.Errorf("jit: %s relocation to %s: %w", definition.symbol.Name, referenceLabel(ref), err)
			}
			if ref.Local {
				target, ok := definitionByRef[objectKey{object: definition.key.object, ref: ref.Ref}]
				if !ok {
					name, hostName := hostPackageSymbolName(packagePath, ref.Name)
					if hostName {
						if _, exists := host.LookupLinkerSymbol(packagePath, name, ref.ABI); exists {
							ref.PackagePath = packagePath
							ref.Name = name
							imp, _, err := hostImport(host, ref)
							if err != nil {
								return ModuleImage{}, fmt.Errorf("jit: bind same-package reference %s.%s: %w", packagePath, name, err)
							}
							index := addImport(imp)
							pending = append(pending, pendingObjectRelocation{
								source: defIndex, relocation: relocation, targetImport: index, imported: true,
							})
							continue
						}
					}
					return ModuleImage{}, fmt.Errorf("jit: %s relocation references missing local symbol %s", definition.symbol.Name, ref.Name)
				}
				if importIndex, hostBound := hostDefinitions[target]; hostBound {
					pending = append(pending, pendingObjectRelocation{
						source: defIndex, relocation: relocation, targetImport: importIndex, imported: true,
					})
					continue
				}
				// Weak references must not make their target reachable. Method
				// offsets are different: a reachable type descriptor names its
				// method wrappers through R_METHODOFF, so include those targets
				// conservatively. This retains every method on a linked type.
				if !weak || rtype == objabi.R_METHODOFF {
					queue = append(queue, target)
				}
				pending = append(pending, pendingObjectRelocation{source: defIndex, relocation: relocation, targetLocal: target})
				continue
			}
			if ref.PackagePath == "" {
				if name, ok := hostPackageSymbolName(packagePath, ref.Name); ok {
					ref.PackagePath, ref.Name = packagePath, name
				}
			}
			imp, _, err := hostImport(host, ref)
			if err != nil {
				return ModuleImage{}, fmt.Errorf("jit: %s relocation to %s: %w", definition.symbol.Name, referenceLabel(ref), err)
			}
			index := addImport(imp)
			pending = append(pending, pendingObjectRelocation{
				source: defIndex, relocation: relocation, targetImport: index, imported: true,
			})
		}
	}
	if len(visitOrder) > maxImageEntries {
		return ModuleImage{}, fmt.Errorf("jit: object link closure has too many symbols")
	}
	for i := range pending {
		relocation := &pending[i]
		rawType := objabi.RelocType(relocation.relocation.Type)
		if rawType&objabi.R_WEAK == 0 || relocation.imported || selected[relocation.targetLocal] {
			continue
		}
		switch objectRelocType(relocation.relocation) {
		case objabi.R_ADDROFF:
			// cmd/link leaves an unresolved weak addroff at its zero value.
			relocation.discard = true
		case objabi.R_ADDR, objabi.R_CALL:
			imp, _, err := hostImport(nil, ObjectReference{PackagePath: "runtime", Name: "unreachableMethod"})
			if err != nil {
				return ModuleImage{}, fmt.Errorf("jit: resolve weak method fallback: %w", err)
			}
			relocation.imported = true
			relocation.targetImport = addImport(imp)
		default:
			return ModuleImage{}, fmt.Errorf("jit: unsupported unresolved weak relocation %s/%d", objectRelocType(relocation.relocation), relocation.relocation.Size)
		}
	}

	sections := map[ImageSectionKind]*imageSectionBuilder{
		ImageText:     {kind: ImageText, align: 16},
		ImageReadOnly: {kind: ImageReadOnly, align: 8},
		ImageTypes:    {kind: ImageTypes, align: 8},
		ImageITabs:    {kind: ImageITabs, align: 8},
		ImageData:     {kind: ImageData, align: 8},
		ImageBSS:      {kind: ImageBSS, align: 8},
	}
	locations := make(map[int]objectLocation, len(visitOrder))
	for _, defIndex := range visitOrder {
		definition := definitions[defIndex]
		kind, section, err := imageSymbolSection(definition.symbol)
		if err != nil {
			return ModuleImage{}, err
		}
		builder := sections[section]
		offset, err := builder.add(definition.symbol)
		if err != nil {
			return ModuleImage{}, fmt.Errorf("jit: lay out %s: %w", definition.symbol.Name, err)
		}
		locations[defIndex] = objectLocation{section: section, offset: offset}
		_ = kind
	}

	// GOT slots live in read-only image memory. The mapper initializes them
	// while every section is writable, then applies final page protections.
	gotTargets := make([]gotTarget, 0)
	gotIndex := make(map[gotTargetKey]int)
	for _, relocation := range pending {
		rtype := objectRelocType(relocation.relocation)
		if rtype != objabi.R_GOTPCREL {
			continue
		}
		key := gotKey(relocation)
		if _, exists := gotIndex[key]; exists {
			continue
		}
		offset, err := sections[ImageReadOnly].addBytes(make([]byte, 8), 8)
		if err != nil {
			return ModuleImage{}, fmt.Errorf("jit: allocate GOT slot: %w", err)
		}
		gotIndex[key] = len(gotTargets)
		gotTargets = append(gotTargets, gotTarget{key: key, offset: offset})
	}

	image := ModuleImage{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
		ABI: "go-object-amd64-validated-v2", PackagePath: packagePath,
		Imports: imports,
	}
	buildID := sha256.Sum256(objectArchive)
	image.BuildID = fmt.Sprintf("%x", buildID[:])
	sectionIndex := make(map[ImageSectionKind]uint32)
	for _, kind := range []ImageSectionKind{ImageText, ImageReadOnly, ImageTypes, ImageITabs, ImageData, ImageBSS} {
		builder := sections[kind]
		if builder.memSize == 0 {
			continue
		}
		index := uint32(len(image.Sections))
		sectionIndex[kind] = index
		section := ImageSection{Kind: kind, Align: builder.align, MemSize: builder.memSize, Data: builder.data, Read: true}
		switch kind {
		case ImageText:
			section.Execute = true
		case ImageData, ImageBSS:
			section.Write = true
		}
		image.Sections = append(image.Sections, section)
	}

	localImageIndex := make(map[int]uint32, len(visitOrder))
	for _, defIndex := range visitOrder {
		definition := definitions[defIndex]
		location := locations[defIndex]
		name := definition.symbol.Name
		if name == "" {
			name = objectSymbolFallback(objects[definition.key.object], definition.symbol)
		}
		kind, _, err := imageSymbolSection(definition.symbol)
		if err != nil {
			return ModuleImage{}, err
		}
		symbol := ImageSymbol{
			PackagePath: packagePath, Name: name, Kind: kind,
			GCTracked: isPointerfulGlobal(definition.symbol.Type),
			Section:   sectionIndex[location.section], Offset: location.offset, Size: uint64(definition.symbol.Size),
		}
		localImageIndex[defIndex] = uint32(len(image.Symbols))
		image.Symbols = append(image.Symbols, symbol)
		if defIndex == entryDef {
			image.Entry = uint32(len(image.Symbols) - 1)
		}
	}
	for _, root := range pendingGlobalRoots {
		dataSymbol, dataOK := localImageIndex[root.dataDef]
		if !dataOK {
			return ModuleImage{}, fmt.Errorf("jit: writable Go global %s was not laid out", definitions[root.dataDef].symbol.Name)
		}
		imageRoot := ImageGlobalRoot{DataSymbol: dataSymbol, TypeImport: root.imported}
		if root.imported {
			imageRoot.TypeSymbol = uint32(root.typeImport)
		} else {
			typeSymbol, typeOK := localImageIndex[root.typeDef]
			if !typeOK {
				return ModuleImage{}, fmt.Errorf("jit: GC type for writable Go global %s was not laid out", definitions[root.dataDef].symbol.Name)
			}
			imageRoot.TypeSymbol = typeSymbol
		}
		image.GlobalRoots = append(image.GlobalRoots, imageRoot)
	}
	for definition, importIndex := range unnamedTypeAliases {
		symbol, ok := localImageIndex[definition]
		if !ok {
			// Alias candidates are collected before archive reachability is
			// known. A candidate omitted from the image has no descriptor or
			// relocation in this module, so it needs no runtime canonicalization.
			continue
		}
		image.TypeAliases = append(image.TypeAliases, ImageTypeAlias{Symbol: symbol, Import: uint32(importIndex)})
	}
	sort.Slice(image.TypeAliases, func(i, j int) bool { return image.TypeAliases[i].Symbol < image.TypeAliases[j].Symbol })
	gotSymbolIndex := make([]uint32, len(gotTargets))
	for i, target := range gotTargets {
		gotSymbolIndex[i] = uint32(len(image.Symbols))
		image.Symbols = append(image.Symbols, ImageSymbol{
			PackagePath: packagePath, Name: fmt.Sprintf("jit.got.slot.%d", i), Kind: ImageVariable,
			Section: sectionIndex[ImageReadOnly], Offset: target.offset, Size: 8,
		})
	}
	sectionOffsets, err := moduleImageSectionOffsets(image.Sections)
	if err != nil {
		return ModuleImage{}, err
	}
	rodataBaseIndex := moduleImageReadOnlyBase(image.Sections)
	for _, defIndex := range visitOrder {
		definition := definitions[defIndex]
		if !objabi.SymKind(definition.symbol.Type).IsText() {
			continue
		}
		function, err := imageFunctionMetadata(objects[definition.key.object], definition.symbol, definition.key.object, definitionByRef, definitions, locations, localImageIndex[defIndex], image.Sections, sectionIndex, sectionOffsets, rodataBaseIndex)
		if err != nil {
			return ModuleImage{}, err
		}
		image.Functions = append(image.Functions, function)
	}

	stubByImport := make(map[int]uint32)
	stubOffsets := make(map[int]uint64)
	for _, relocation := range pending {
		if !relocation.imported || objectRelocType(relocation.relocation) != objabi.R_CALL {
			continue
		}
		if imports[relocation.targetImport].Kind != ImageFunction {
			return ModuleImage{}, fmt.Errorf("jit: call relocation targets non-function import %s.%s", imports[relocation.targetImport].PackagePath, imports[relocation.targetImport].Name)
		}
		if _, ok := stubByImport[relocation.targetImport]; ok {
			continue
		}
		text := sections[ImageText]
		offset, err := text.addBytes([]byte{0x49, 0xbb, 0, 0, 0, 0, 0, 0, 0, 0, 0x41, 0xff, 0xe3}, 16)
		if err != nil {
			return ModuleImage{}, fmt.Errorf("jit: append call trampoline: %w", err)
		}
		stubOffsets[relocation.targetImport] = offset
		stubByImport[relocation.targetImport] = uint32(len(image.Symbols))
		image.Symbols = append(image.Symbols, ImageSymbol{
			PackagePath: packagePath, Name: fmt.Sprintf("jit.trampoline.%d", relocation.targetImport), Kind: ImageFunction,
			Section: sectionIndex[ImageText], Offset: offset, Size: 13,
		})
		image.Functions = append(image.Functions, ImageFunctionMetadata{
			Symbol: uint32(len(image.Symbols) - 1), PCSP: encodePCValueTable(0, 13),
			PCData:   [][]byte{encodePCValueTable(-2, 13), encodePCValueTable(0, 13)},
			FuncData: [][]byte{emptyStackMap(), emptyStackMap()},
		})
	}
	for _, relocation := range pending {
		if relocation.discard {
			continue
		}
		source := definitions[relocation.source].symbol
		location := locations[relocation.source]
		rtype := objabi.RelocType(relocation.relocation.Type)
		rtype &^= objabi.R_WEAK
		if relocation.relocation.Offset < 0 || uint64(relocation.relocation.Offset)+uint64(relocation.relocation.Size) > uint64(source.Size) {
			return ModuleImage{}, fmt.Errorf("jit: relocation in %s is outside its symbol data", source.Name)
		}
		imageRelocKind, err := imageRelocationFor(rtype, relocation.relocation.Size)
		if err != nil {
			return ModuleImage{}, fmt.Errorf("jit: %s: %w", source.Name, err)
		}
		target := uint32(0)
		targetImport := false
		if rtype == objabi.R_GOTPCREL {
			target = gotSymbolIndex[gotIndex[gotKey(relocation)]]
		} else if rtype == objabi.R_TLS_LE {
			target = ^uint32(0)
		} else if relocation.imported {
			if objectRelocType(relocation.relocation) == objabi.R_CALL {
				target = stubByImport[relocation.targetImport]
			} else {
				target, targetImport = uint32(relocation.targetImport), true
			}
		} else {
			target, _ = localImageIndex[relocation.targetLocal]
		}
		image.Relocations = append(image.Relocations, ImageRelocation{
			Section: sectionIndex[location.section],
			Offset:  location.offset + uint64(relocation.relocation.Offset),
			Kind:    imageRelocKind, Target: target, TargetImport: targetImport,
			Addend: relocation.relocation.Addend,
		})
		if rtype == objabi.R_GOTPCREL {
			image.Relocations[len(image.Relocations)-1].Addend = 0
		}
	}
	for _, got := range gotTargets {
		target := uint32(got.key.local)
		targetImport := got.key.remote
		if targetImport {
			target = uint32(got.key.importIndex)
		} else {
			target, _ = localImageIndex[got.key.local]
		}
		image.Relocations = append(image.Relocations, ImageRelocation{
			Section: sectionIndex[ImageReadOnly], Offset: got.offset, Kind: RelocAMD64Abs64,
			Target: target, TargetImport: targetImport, Addend: got.key.addend,
		})
	}
	stubImports := make([]int, 0, len(stubOffsets))
	for importIndex := range stubOffsets {
		stubImports = append(stubImports, importIndex)
	}
	sort.Ints(stubImports)
	for _, importIndex := range stubImports {
		offset := stubOffsets[importIndex]
		image.Relocations = append(image.Relocations, ImageRelocation{
			Section: sectionIndex[ImageText], Offset: offset + 2, Kind: RelocAMD64Abs64,
			Target: uint32(importIndex), TargetImport: true,
		})
	}
	sort.Slice(image.Functions, func(i, j int) bool {
		return image.Symbols[image.Functions[i].Symbol].Offset < image.Symbols[image.Functions[j].Symbol].Offset
	})
	textSection := image.Sections[sectionIndex[ImageText]]
	textSection.MemSize = sections[ImageText].memSize
	textSection.Data = sections[ImageText].data
	image.Sections[sectionIndex[ImageText]] = textSection
	if err := validateModuleImage(image); err != nil {
		return ModuleImage{}, err
	}
	return image, nil
}

func imageFunctionMetadata(object GoObject, symbol ObjectSymbol, objectIndex int, definitionsByRef map[objectKey]int, definitions []objectDefinition, locations map[int]objectLocation, imageSymbol uint32, imageSections []ImageSection, sectionIndex map[ImageSectionKind]uint32, sectionOffsets []uint64, rodataBaseIndex int) (ImageFunctionMetadata, error) {
	function := symbol.Function
	if len(function.InlineTree) != 0 {
		return ImageFunctionMetadata{}, fmt.Errorf("jit: %s contains compiler inline frames; the in-memory runtime metadata format does not yet encode inline trees", symbol.Name)
	}
	if len(function.PCSP) == 0 || len(function.FuncData) < 2 {
		return ImageFunctionMetadata{}, fmt.Errorf("jit: %s is missing compiler PC or GC stack metadata (pcsp=%d pcdata=%d funcdata=%d)", symbol.Name, len(function.PCSP), len(function.PCData), len(function.FuncData))
	}
	checkAux := func(ref goobj.SymRef, label string, allowRelocs bool) error {
		if ref.PkgIdx == goobj.PkgIdxInvalid {
			return nil
		}
		if ref.PkgIdx != goobj.PkgIdxSelf && ref.PkgIdx != goobj.PkgIdxHashed64 && ref.PkgIdx != goobj.PkgIdxHashed && ref.PkgIdx != goobj.PkgIdxNone {
			return fmt.Errorf("jit: %s %s references a non-local object symbol", symbol.Name, label)
		}
		defIndex, ok := definitionsByRef[objectKey{object: objectIndex, ref: ref}]
		if !ok {
			return fmt.Errorf("jit: %s %s references missing object symbol", symbol.Name, label)
		}
		if len(definitions[defIndex].symbol.Relocations) != 0 && !allowRelocs {
			r := definitions[defIndex].symbol.Relocations[0]
			ref, _ := object.ResolveReference(r.Target)
			return fmt.Errorf("jit: %s %s target %s contains %d relocations unsupported in runtime tables (first: %s to %s addend=%d)", symbol.Name, label, definitions[defIndex].symbol.Name, len(definitions[defIndex].symbol.Relocations), objabi.RelocType(r.Type), referenceLabel(ref), r.Addend)
		}
		return nil
	}
	if err := checkAux(function.PCSPRef, "pcsp", false); err != nil {
		return ImageFunctionMetadata{}, err
	}
	if err := checkAux(function.PCFileRef, "pcfile", false); err != nil {
		return ImageFunctionMetadata{}, err
	}
	if err := checkAux(function.PCLineRef, "pcline", false); err != nil {
		return ImageFunctionMetadata{}, err
	}
	for _, ref := range function.PCDataRefs {
		if err := checkAux(ref, "pcdata", false); err != nil {
			return ImageFunctionMetadata{}, err
		}
	}
	for i, ref := range function.FuncDataRefs {
		if err := checkAux(ref, fmt.Sprintf("funcdata[%d]", i), true); err != nil {
			return ImageFunctionMetadata{}, err
		}
	}
	funcdata := cloneByteSlices(function.FuncData)
	for i, ref := range function.FuncDataRefs {
		if ref.PkgIdx == goobj.PkgIdxInvalid {
			continue
		}
		dataDefIndex, ok := definitionsByRef[objectKey{object: objectIndex, ref: ref}]
		if !ok {
			return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] has no local definition", symbol.Name, i)
		}
		for _, relocation := range definitions[dataDefIndex].symbol.Relocations {
			if objabi.RelocType(relocation.Type) != objabi.R_ADDROFF || relocation.Size != 4 {
				return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] has unsupported relocation %s/%d", symbol.Name, i, objabi.RelocType(relocation.Type), relocation.Size)
			}
			if relocation.Offset < 0 || uint64(relocation.Offset)+4 > uint64(len(funcdata[i])) {
				return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] relocation is outside its table", symbol.Name, i)
			}
			targetRef, err := object.ResolveReference(relocation.Target)
			if err != nil || !targetRef.Local {
				return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] relocation has a non-local target", symbol.Name, i)
			}
			targetDefIndex, ok := definitionsByRef[objectKey{object: objectIndex, ref: targetRef.Ref}]
			if !ok {
				return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] relocation target %s is absent", symbol.Name, i, targetRef.Name)
			}
			targetLocation, ok := locations[targetDefIndex]
			if !ok {
				return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] relocation target %s was not laid out", symbol.Name, i, targetRef.Name)
			}
			targetOffset := targetLocation.offset
			if targetLocation.section != ImageText {
				section, ok := sectionIndex[targetLocation.section]
				if !ok || rodataBaseIndex < 0 || int(section) < rodataBaseIndex || int(section) >= len(sectionOffsets) {
					return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] relocation target %s is outside the read-only image", symbol.Name, i, targetRef.Name)
				}
				targetOffset += sectionOffsets[section] - sectionOffsets[rodataBaseIndex]
			}
			value, ok := addSignedOffset(targetOffset, relocation.Addend)
			if !ok || value > uint64(^uint32(0)) {
				return ImageFunctionMetadata{}, fmt.Errorf("jit: %s funcdata[%d] addroff target %s is out of range", symbol.Name, i, targetRef.Name)
			}
			binary.LittleEndian.PutUint32(funcdata[i][relocation.Offset:], uint32(value))
		}
	}
	pcdata := cloneByteSlices(function.PCData)
	for len(pcdata) < 2 {
		index := len(pcdata)
		value := int32(0)
		if index == 0 {
			value = -2 // suppress asynchronous preemption in generated code
		}
		pcdata = append(pcdata, encodePCValueTable(value, symbol.Size))
	}
	return ImageFunctionMetadata{
		Symbol: imageSymbol, ArgsSize: function.ArgsSize, StartLine: function.StartLine,
		FuncID: function.FuncID, FuncFlag: function.FuncFlag,
		PCSP:   append([]byte(nil), function.PCSP...),
		PCFile: append([]byte(nil), function.PCFile...),
		PCLine: append([]byte(nil), function.PCLine...),
		Files:  append([]string(nil), function.Files...),
		PCData: pcdata, FuncData: funcdata,
	}, nil
}

func cloneByteSlices(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i := range in {
		out[i] = append([]byte(nil), in[i]...)
	}
	return out
}

func moduleImageSectionOffsets(sections []ImageSection) ([]uint64, error) {
	pageSize := uint64(syscall.Getpagesize())
	offsets := make([]uint64, len(sections))
	offset := pageSize // reserve the runtime-owned control page
	for i, section := range sections {
		offsets[i] = offset
		if section.MemSize > ^uint64(0)-(pageSize-1) {
			return nil, fmt.Errorf("jit: image section %d mapping size overflows", i)
		}
		size := (section.MemSize + pageSize - 1) &^ (pageSize - 1)
		if offset > ^uint64(0)-size {
			return nil, fmt.Errorf("jit: image section %d mapping offset overflows", i)
		}
		offset += size
	}
	return offsets, nil
}

func moduleImageReadOnlyBase(sections []ImageSection) int {
	for i, section := range sections {
		switch section.Kind {
		case ImageReadOnly, ImageTypes, ImageITabs:
			return i
		}
	}
	return -1
}

func emptyStackMap() []byte {
	return []byte{1, 0, 0, 0, 0, 0, 0, 0}
}

func encodePCValueTable(value int32, pcDelta uint32) []byte {
	var out []byte
	delta := uint32(value + 1)
	zigzag := (delta << 1) ^ uint32(int32(delta)>>31)
	out = appendObjectVarint(out, zigzag)
	out = appendObjectVarint(out, pcDelta)
	return append(out, 0)
}

func appendObjectVarint(out []byte, value uint32) []byte {
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

type imageImportKey struct {
	packagePath string
	name        string
	kind        ImageSymbolKind
	callingABI  uint16
	abiHash     [16]byte
}

func imageSymbolSection(symbol ObjectSymbol) (ImageSymbolKind, ImageSectionKind, error) {
	switch objabi.SymKind(symbol.Type) {
	case objabi.STEXT, objabi.STEXTFIPS:
		return ImageFunction, ImageText, nil
	case objabi.SRODATA, objabi.SRODATAFIPS:
		// Names are not a reliable discriminator here: type-associated
		// namedata symbols also begin with "type:". The compiler's object
		// flags identify actual runtime descriptors and itabs.
		if symbol.Flags2&goobj.SymFlagItab != 0 {
			return ImageITable, ImageITabs, nil
		}
		if symbol.Flags&goobj.SymFlagGoType != 0 {
			return ImageType, ImageReadOnly, nil
		}
		return ImageVariable, ImageReadOnly, nil
	case objabi.SDATA, objabi.SDATAFIPS, objabi.SNOPTRDATA, objabi.SNOPTRDATAFIPS:
		return ImageVariable, ImageData, nil
	case objabi.SBSS, objabi.SNOPTRBSS:
		return ImageVariable, ImageBSS, nil
	default:
		return 0, 0, fmt.Errorf("jit: reachable object symbol %q has unsupported Go symbol type %d", symbol.Name, symbol.Type)
	}
}

func isPointerfulGlobal(symbolType uint8) bool {
	switch objabi.SymKind(symbolType) {
	case objabi.SDATA, objabi.SDATAFIPS, objabi.SBSS:
		return true
	default:
		return false
	}
}

func hostPackageSymbolName(packagePath, objectName string) (string, bool) {
	objectName = strings.TrimPrefix(objectName, "type:")
	objectName = strings.TrimPrefix(objectName, "type.")
	prefix := packagePath + "."
	if !strings.HasPrefix(objectName, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(objectName, prefix)
	// Linker-private records such as .inittask are not package declarations.
	// They have no source-level identity to bind through the host manifest.
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "go:") {
		return "", false
	}
	if strings.HasPrefix(name, "(*") {
		if receiverEnd := strings.Index(name, ")."); receiverEnd > 2 {
			name = name[2:receiverEnd] + name[receiverEnd+1:]
		}
	} else if strings.HasPrefix(name, "*") {
		name = strings.TrimPrefix(name, "*")
	}
	return name, name != ""
}

func (b *imageSectionBuilder) add(symbol ObjectSymbol) (uint64, error) {
	align := uint64(symbol.Align)
	if align == 0 {
		align = 1
	}
	if align&(align-1) != 0 || align > 1<<16 {
		return 0, fmt.Errorf("invalid alignment %d", align)
	}
	if uint64(symbol.Size) > maxModuleImageSize || uint64(len(symbol.Data)) > uint64(symbol.Size) {
		return 0, fmt.Errorf("invalid size %d with %d data bytes", symbol.Size, len(symbol.Data))
	}
	offset, ok := alignImageOffset(b.memSize, align)
	if !ok || offset > maxModuleImageSize || uint64(symbol.Size) > maxModuleImageSize-offset {
		return 0, fmt.Errorf("section size overflows image limit")
	}
	if align > uint64(b.align) {
		b.align = uint32(align)
	}
	if b.kind != ImageBSS {
		if offset > uint64(len(b.data)) {
			b.data = append(b.data, make([]byte, int(offset)-len(b.data))...)
		}
		b.data = append(b.data, symbol.Data...)
		b.data = append(b.data, make([]byte, int(symbol.Size)-len(symbol.Data))...)
	}
	b.memSize = offset + uint64(symbol.Size)
	return offset, nil
}

func (b *imageSectionBuilder) addBytes(data []byte, align uint64) (uint64, error) {
	if align == 0 || align&(align-1) != 0 {
		return 0, fmt.Errorf("invalid trampoline alignment %d", align)
	}
	offset, ok := alignImageOffset(b.memSize, align)
	if !ok || offset > maxModuleImageSize || uint64(len(data)) > maxModuleImageSize-offset {
		return 0, fmt.Errorf("trampoline exceeds image size limit")
	}
	if offset > uint64(len(b.data)) {
		b.data = append(b.data, make([]byte, int(offset)-len(b.data))...)
	}
	b.data = append(b.data, data...)
	b.memSize = offset + uint64(len(data))
	return offset, nil
}

func alignImageOffset(value, alignment uint64) (uint64, bool) {
	mask := alignment - 1
	if value > ^uint64(0)-mask {
		return 0, false
	}
	return (value + mask) &^ mask, true
}

func imageRelocationFor(rtype objabi.RelocType, size uint8) (ImageRelocationKind, error) {
	switch rtype {
	case objabi.R_ADDR:
		if size == 8 {
			return RelocAMD64Abs64, nil
		}
		if size == 4 {
			return RelocAMD64Abs32, nil
		}
	case objabi.R_CALL:
		if size == 4 {
			return RelocAMD64PLT32, nil
		}
	case objabi.R_PCREL:
		if size == 4 {
			return RelocAMD64PC32, nil
		}
	case objabi.R_ADDROFF:
		if size == 4 {
			return RelocAMD64Addroff32, nil
		}
	case objabi.R_METHODOFF:
		if size == 4 {
			return RelocAMD64Addroff32, nil
		}
	case objabi.R_GOTPCREL:
		if size == 4 {
			return RelocAMD64GOTPCREL, nil
		}
	case objabi.R_TLS_LE:
		if size == 4 {
			return RelocAMD64TLSLE32, nil
		}
	case objabi.R_CALLIND:
		if size == 0 {
			return 0, nil
		}
	case objabi.R_USEFIELD, objabi.R_USETYPE, objabi.R_USEIFACE, objabi.R_USEIFACEMETHOD, objabi.R_USENAMEDMETHOD, objabi.R_KEEP:
		if size == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("unsupported Go relocation type %s with width %d", rtype, size)
}

func isMarkerRelocation(rtype objabi.RelocType) bool {
	switch rtype {
	case objabi.R_USEFIELD, objabi.R_USETYPE, objabi.R_USEIFACE, objabi.R_USEIFACEMETHOD, objabi.R_USENAMEDMETHOD, objabi.R_KEEP, objabi.R_CALLIND:
		return true
	default:
		return false
	}
}

func hostImport(metadata *Metadata, ref ObjectReference) (ImageImport, uintptr, error) {
	packagePath, name := ref.PackagePath, ref.Name
	if packagePath == "" && strings.HasPrefix(name, "runtime.") {
		packagePath = "runtime"
	}
	if strings.HasPrefix(name, "type:") {
		name = strings.TrimPrefix(name, "type:")
		if packagePath == "" && strings.HasPrefix(name, "runtime.") {
			packagePath = "runtime"
		}
	}
	if packagePath != "" {
		name = strings.TrimPrefix(name, objabi.PathToPrefix(packagePath)+".")
	}
	// Predeclared type descriptors have no defining package in object
	// references. Resolve them through the running Go runtime.
	if packagePath == "" {
		if imported, address, ok := runtimeTypeImport(name, ref.ABI); ok {
			return imported, address, nil
		}
	}
	if metadata != nil {
		if linked, ok := metadata.LookupLinkerSymbol(packagePath, name, ref.ABI); ok {
			return imageImportForLinked(linked, ref.ABI)
		}
	}
	if packagePath == "runtime" {
		if imported, address, ok := runtimeTypeImport(name, ref.ABI); ok {
			return imported, address, nil
		}
		if address := runtime.ResolveJITVariable(name); address != 0 {
			return ImageImport{PackagePath: packagePath, Name: name, Kind: ImageVariable, ABI: ref.ABI}, address, nil
		}
		if address := runtime.ResolveJITFunction("runtime." + name); address != 0 {
			if !runtimeFunctionABIResolvable(name, ref.ABI) {
				return ImageImport{}, 0, fmt.Errorf("runtime function runtime.%s ABI %d has no exact linker manifest entry", name, ref.ABI)
			}
			return ImageImport{PackagePath: packagePath, Name: name, Kind: ImageFunction, ABI: ref.ABI}, address, nil
		}
		return ImageImport{}, 0, fmt.Errorf("runtime function runtime.%s is not present in the loaded module tables", name)
	}
	if metadata == nil {
		return ImageImport{}, 0, fmt.Errorf("no host metadata is available for %s.%s", packagePath, name)
	}
	return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s with ABI %d is absent from the link manifest", packagePath, name, ref.ABI)
}

func runtimeTypeImport(name string, abi uint16) (ImageImport, uintptr, bool) {
	address, ok := runtimeTypeAddress(name)
	if !ok {
		return ImageImport{}, 0, false
	}
	typ, ok := runtimeTypeFromExpr(strings.TrimPrefix(name, "type:"))
	if !ok || typ == nil {
		return ImageImport{}, 0, false
	}
	return ImageImport{
		PackagePath: "runtime",
		Name:        strings.TrimPrefix(name, "type:"),
		Kind:        ImageType,
		ABI:         abi,
		ABIHash:     linkedABIHash(typ),
	}, address, true
}

// runtimeFunctionABIResolvable lists the narrowly defined runtime entry
// points that can be resolved without a generated linker manifest. Most
// runtime calls use ABIInternal. The compiler's stack-growth call is an
// assembly ABI0 symbol on linux/amd64, so permit that one exact pairing too.
func runtimeFunctionABIResolvable(name string, abi uint16) bool {
	if abi == uint16(obj.ABIInternal) {
		return true
	}
	if abi != uint16(obj.ABI0) {
		return false
	}
	// morestack and morestack_noctxt are assembly ABI0 entry points. The
	// unreachable method is only materialized as an itab fallback address; it
	// is not a callable ABI0 implementation.
	switch name {
	case "morestack", "morestack_noctxt", "unreachableMethod":
		return true
	default:
		return false
	}
}

func imageImportForLinked(linked LinkedSymbol, abi uint16) (ImageImport, uintptr, error) {
	if linked.Address == 0 {
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s has no process address", linked.PackagePath, linked.Name)
	}
	if linked.FromManifest && linked.Type == nil && linked.Kind != ITableLink && linked.Kind != FunctionLink {
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s has no linked Go type identity", linked.PackagePath, linked.Name)
	}
	var kind ImageSymbolKind
	switch linked.Kind {
	case FunctionLink:
		kind = ImageFunction
	case VariableLink:
		kind = ImageVariable
	case TypeLink:
		kind = ImageType
	case ITableLink:
		kind = ImageITable
	default:
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s has unsupported kind %q", linked.PackagePath, linked.Name, linked.Kind)
	}
	if linked.FromManifest && linked.ABI != abi {
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s ABI mismatch: manifest has %d, object expects %d", linked.PackagePath, linked.Name, linked.ABI, abi)
	}
	return ImageImport{
		PackagePath: linked.PackagePath,
		Name:        linked.Name,
		Kind:        kind,
		ABI:         abi,
		ABIHash:     linkedABIHash(linked.Type),
	}, linked.Address, nil
}

func linkedABIHash(typ reflect.Type) [16]byte {
	if typ == nil {
		return [16]byte{}
	}
	identity := appendABITypeIdentity(nil, typ, make(map[reflect.Type]bool))
	digest := sha256.Sum256(identity)
	var abi [16]byte
	copy(abi[:], digest[:16])
	return abi
}

// appendABITypeIdentity writes a target-specific structural type identity.
// reflect.Type.String is intended for display and may use package names rather
// than import paths, so it is not sufficient for binding a JIT relocation to
// the exact type selected by the normal linker.
func appendABITypeIdentity(dst []byte, typ reflect.Type, active map[reflect.Type]bool) []byte {
	if typ == nil {
		return append(dst, 0)
	}
	dst = append(dst, byte(typ.Kind()+1))
	dst = appendABIUint(dst, uint64(typ.Size()))
	dst = appendABIUint(dst, uint64(typ.Align()))
	if typ.Name() != "" {
		dst = append(dst, 'N')
		dst = appendABIString(dst, typ.PkgPath())
		dst = appendABIString(dst, typ.Name())
		return dst
	}
	if active[typ] {
		// Recursive unnamed types are not constructible in Go without a named
		// type at the recursion boundary. Keep a deterministic marker anyway so
		// malformed runtime metadata fails closed rather than recursing forever.
		return append(dst, 'R')
	}
	active[typ] = true
	defer delete(active, typ)
	switch typ.Kind() {
	case reflect.Array:
		dst = appendABIUint(dst, uint64(typ.Len()))
		return appendABITypeIdentity(dst, typ.Elem(), active)
	case reflect.Chan:
		dst = append(dst, byte(typ.ChanDir()))
		return appendABITypeIdentity(dst, typ.Elem(), active)
	case reflect.Func:
		if typ.IsVariadic() {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
		dst = appendABIUint(dst, uint64(typ.NumIn()))
		dst = appendABIUint(dst, uint64(typ.NumOut()))
		for i := 0; i < typ.NumIn(); i++ {
			dst = appendABITypeIdentity(dst, typ.In(i), active)
		}
		for i := 0; i < typ.NumOut(); i++ {
			dst = appendABITypeIdentity(dst, typ.Out(i), active)
		}
		return dst
	case reflect.Interface:
		dst = appendABIUint(dst, uint64(typ.NumMethod()))
		for i := 0; i < typ.NumMethod(); i++ {
			method := typ.Method(i)
			dst = appendABIString(dst, method.Name)
			dst = appendABIString(dst, method.PkgPath)
			dst = appendABITypeIdentity(dst, method.Type, active)
		}
		return dst
	case reflect.Map:
		dst = appendABITypeIdentity(dst, typ.Key(), active)
		return appendABITypeIdentity(dst, typ.Elem(), active)
	case reflect.Pointer, reflect.Slice:
		return appendABITypeIdentity(dst, typ.Elem(), active)
	case reflect.Struct:
		dst = appendABIUint(dst, uint64(typ.NumField()))
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			dst = appendABIString(dst, field.Name)
			dst = appendABIString(dst, field.PkgPath)
			dst = appendABIString(dst, string(field.Tag))
			dst = appendABIUint(dst, uint64(field.Offset))
			if field.Anonymous {
				dst = append(dst, 1)
			} else {
				dst = append(dst, 0)
			}
			dst = appendABITypeIdentity(dst, field.Type, active)
		}
		return dst
	default:
		return dst
	}
}

func appendABIString(dst []byte, value string) []byte {
	dst = appendABIUint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendABIUint(dst []byte, value uint64) []byte {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	return append(dst, encoded[:]...)
}

func referenceLabel(ref ObjectReference) string {
	if ref.PackagePath != "" {
		return ref.PackagePath + "." + strings.TrimPrefix(ref.Name, ref.PackagePath+".")
	}
	return ref.Name
}

func objectSymbolFallback(object GoObject, symbol ObjectSymbol) string {
	if ref, err := object.ResolveReference(symbol.Ref); err == nil && ref.Name != "" {
		return ref.Name
	}
	return fmt.Sprintf("symbol.%d.%d", symbol.Ref.PkgIdx, symbol.Ref.SymIdx)
}
