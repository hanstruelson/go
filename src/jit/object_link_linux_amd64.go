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
	"unsafe"

	"cmd/publicinternal/goobj"
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
	importAddresses := make([]uintptr, 0)
	importIndex := make(map[imageImportKey]int)
	var pending []pendingObjectRelocation
	for cursor := 0; cursor < len(queue); cursor++ {
		defIndex := queue[cursor]
		if selected[defIndex] {
			continue
		}
		selected[defIndex] = true
		visitOrder = append(visitOrder, defIndex)
		definition := definitions[defIndex]
		object := objects[definition.key.object]
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
			ref, err := object.ResolveReference(relocation.Target)
			if err != nil {
				return ModuleImage{}, fmt.Errorf("jit: %s relocation at offset %d: %w", definition.symbol.Name, relocation.Offset, err)
			}
			rtype := objabi.RelocType(relocation.Type)
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
					return ModuleImage{}, fmt.Errorf("jit: %s relocation references missing local symbol %s", definition.symbol.Name, ref.Name)
				}
				queue = append(queue, target)
				pending = append(pending, pendingObjectRelocation{source: defIndex, relocation: relocation, targetLocal: target})
				continue
			}
			imp, address, err := hostImport(host, ref)
			if err != nil {
				return ModuleImage{}, fmt.Errorf("jit: %s relocation to %s: %w", definition.symbol.Name, referenceLabel(ref), err)
			}
			key := imageImportKey{packagePath: imp.PackagePath, name: imp.Name, kind: imp.Kind, abi: imp.ABIHash}
			index, ok := importIndex[key]
			if !ok {
				index = len(imports)
				imports = append(imports, imp)
				importAddresses = append(importAddresses, address)
				importIndex[key] = index
			}
			pending = append(pending, pendingObjectRelocation{
				source: defIndex, relocation: relocation, targetImport: index, imported: true,
			})
		}
	}
	if len(visitOrder) > maxImageEntries {
		return ModuleImage{}, fmt.Errorf("jit: object link closure has too many symbols")
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

	image := ModuleImage{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
		ABI: "go-object-amd64-subset-v1", PackagePath: packagePath,
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
			Section: sectionIndex[location.section], Offset: location.offset, Size: uint64(definition.symbol.Size),
		}
		localImageIndex[defIndex] = uint32(len(image.Symbols))
		image.Symbols = append(image.Symbols, symbol)
		if defIndex == entryDef {
			image.Entry = uint32(len(image.Symbols) - 1)
		}
	}
	for _, defIndex := range visitOrder {
		definition := definitions[defIndex]
		if !objabi.SymKind(definition.symbol.Type).IsText() {
			continue
		}
		function, err := imageFunctionMetadata(objects[definition.key.object], definition.symbol, definition.key.object, definitionByRef, definitions, locations, localImageIndex[defIndex])
		if err != nil {
			return ModuleImage{}, err
		}
		image.Functions = append(image.Functions, function)
	}

	stubByImport := make(map[int]uint32)
	stubOffsets := make(map[int]uint64)
	for _, relocation := range pending {
		if !relocation.imported || objabi.RelocType(relocation.relocation.Type) != objabi.R_CALL {
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
		source := definitions[relocation.source].symbol
		location := locations[relocation.source]
		if relocation.relocation.Offset < 0 || uint64(relocation.relocation.Offset)+uint64(relocation.relocation.Size) > uint64(source.Size) {
			return ModuleImage{}, fmt.Errorf("jit: relocation in %s is outside its symbol data", source.Name)
		}
		imageRelocKind, err := imageRelocationFor(objabi.RelocType(relocation.relocation.Type), relocation.relocation.Size)
		if err != nil {
			return ModuleImage{}, fmt.Errorf("jit: %s: %w", source.Name, err)
		}
		target := uint32(0)
		targetImport := false
		if relocation.imported {
			if objabi.RelocType(relocation.relocation.Type) == objabi.R_CALL {
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
	}
	for importIndex, offset := range stubOffsets {
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

func imageFunctionMetadata(object GoObject, symbol ObjectSymbol, objectIndex int, definitionsByRef map[objectKey]int, definitions []objectDefinition, locations map[int]objectLocation, imageSymbol uint32) (ImageFunctionMetadata, error) {
	function := symbol.Function
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
			value, ok := addSignedOffset(targetLocation.offset, relocation.Addend)
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
	abi         [16]byte
}

func imageSymbolSection(symbol ObjectSymbol) (ImageSymbolKind, ImageSectionKind, error) {
	name := symbol.Name
	switch objabi.SymKind(symbol.Type) {
	case objabi.STEXT, objabi.STEXTFIPS:
		return ImageFunction, ImageText, nil
	case objabi.SRODATA, objabi.SRODATAFIPS:
		if strings.HasPrefix(name, "type:") || strings.HasPrefix(name, "type.") {
			return ImageType, ImageReadOnly, nil
		}
		if strings.HasPrefix(name, "go:itab.") {
			return ImageITable, ImageITabs, nil
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
	case objabi.R_USEFIELD, objabi.R_USETYPE, objabi.R_USEIFACE, objabi.R_USEIFACEMETHOD, objabi.R_USENAMEDMETHOD, objabi.R_KEEP:
		if size == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("unsupported Go relocation type %s with width %d", rtype, size)
}

func isMarkerRelocation(rtype objabi.RelocType) bool {
	switch rtype {
	case objabi.R_USEFIELD, objabi.R_USETYPE, objabi.R_USEIFACE, objabi.R_USEIFACEMETHOD, objabi.R_USENAMEDMETHOD, objabi.R_KEEP:
		return true
	default:
		return false
	}
}

func hostImport(metadata *Metadata, ref ObjectReference) (ImageImport, uintptr, error) {
	packagePath, name := ref.PackagePath, ref.Name
	if packagePath != "" {
		name = strings.TrimPrefix(name, packagePath+".")
	} else if strings.HasPrefix(name, "runtime.") {
		packagePath, name = "runtime", strings.TrimPrefix(name, "runtime.")
	}
	if packagePath == "" && strings.HasPrefix(name, "type:") {
		packagePath = "runtime"
	}
	if packagePath == "runtime" {
		if address, ok := runtimeTypeAddress(name); ok {
			return ImageImport{PackagePath: packagePath, Name: name, Kind: ImageType}, address, nil
		}
		if address := runtime.ResolveJITFunction("runtime." + name); address != 0 {
			return ImageImport{PackagePath: packagePath, Name: name, Kind: ImageFunction}, address, nil
		}
		return ImageImport{}, 0, fmt.Errorf("runtime function runtime.%s is not present in the loaded module tables", name)
	}
	if metadata == nil {
		return ImageImport{}, 0, fmt.Errorf("no host metadata is available for %s.%s", packagePath, name)
	}
	linked, ok := metadata.LookupSymbol(packagePath, name)
	if !ok {
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s is absent from the link manifest", packagePath, name)
	}
	if linked.Address == 0 {
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s has no process address", packagePath, name)
	}
	var kind ImageSymbolKind
	switch linked.Kind {
	case FunctionLink:
		kind = ImageFunction
	case VariableLink:
		kind = ImageVariable
	case TypeLink:
		kind = ImageType
	default:
		return ImageImport{}, 0, fmt.Errorf("host symbol %s.%s has unsupported kind %q", packagePath, name, linked.Kind)
	}
	var abi [16]byte
	if linked.Type != nil {
		digest := sha256.Sum256([]byte(linked.Type.PkgPath() + ":" + linked.Type.String()))
		copy(abi[:], digest[:16])
	}
	return ImageImport{PackagePath: packagePath, Name: name, Kind: kind, ABIHash: abi}, linked.Address, nil
}

func runtimeTypeAddress(name string) (uintptr, bool) {
	var typ reflect.Type
	switch strings.TrimPrefix(name, "type:") {
	case "string":
		typ = reflect.TypeOf("")
	case "bool":
		typ = reflect.TypeOf(false)
	case "int":
		typ = reflect.TypeOf(int(0))
	case "int8":
		typ = reflect.TypeOf(int8(0))
	case "int16":
		typ = reflect.TypeOf(int16(0))
	case "int32":
		typ = reflect.TypeOf(int32(0))
	case "int64":
		typ = reflect.TypeOf(int64(0))
	case "uint":
		typ = reflect.TypeOf(uint(0))
	case "uint8":
		typ = reflect.TypeOf(uint8(0))
	case "uint16":
		typ = reflect.TypeOf(uint16(0))
	case "uint32":
		typ = reflect.TypeOf(uint32(0))
	case "uint64":
		typ = reflect.TypeOf(uint64(0))
	case "float32":
		typ = reflect.TypeOf(float32(0))
	case "float64":
		typ = reflect.TypeOf(float64(0))
	case "complex64":
		typ = reflect.TypeOf(complex64(0))
	case "complex128":
		typ = reflect.TypeOf(complex128(0))
	default:
		return 0, false
	}
	type runtimeTypeInterface struct {
		tab  unsafe.Pointer
		data unsafe.Pointer
	}
	return uintptr((*runtimeTypeInterface)(unsafe.Pointer(&typ)).data), true
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
