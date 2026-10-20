// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

func loadLinkedSnippet(metadata *Metadata, compiled CompiledPackage) (*mappedImage, error) {
	if compiled.objectHash == [32]byte{} || sha256.Sum256(compiled.Object) != compiled.objectHash {
		return nil, fmt.Errorf("jit: compiler object changed after capture/effect analysis")
	}
	if compiled.EntryName != compiled.analyzedEntryName {
		return nil, fmt.Errorf("jit: compiler entry name changed after owner analysis")
	}
	hostEffectPinReason := compiled.effects.hostPinReason(metadata)
	entryName := compiled.EntryName
	if entryName == "" {
		entryName = "Eval"
	}
	image, err := LinkObjectArchive(compiled.Object, entryName, metadata)
	if err != nil {
		return nil, err
	}
	if err := validateLinkedSnippetIdentity(image, metadata); err != nil {
		return nil, err
	}
	guard := runtime.JITBeginTypeIdentityUpdate()
	defer guard.Release()
	mapped, err := mapModuleImageWithTypeIdentityGuard(image, func(symbol ImageImport) (uintptr, error) {
		return resolveImageImport(metadata, symbol)
	}, guard)
	if err != nil {
		return mapped, err
	}
	textIndex := -1
	rodataStart := uintptr(0)
	hasTypeMetadata := false
	canonicalTypes := make(map[uint32]uint32, len(image.TypeAliases))
	for _, alias := range image.TypeAliases {
		canonicalTypes[alias.Symbol] = alias.Import
	}
	for i, section := range image.Sections {
		if section.Kind == ImageText {
			textIndex = i
		}
		if rodataStart == 0 && (section.Kind == ImageReadOnly || section.Kind == ImageTypes || section.Kind == ImageITabs) {
			rodataStart = uintptrPointer(mapped.sections[i])
		}
	}
	for symbolIndex, symbol := range image.Symbols {
		switch symbol.Kind {
		case ImageType:
			hasTypeMetadata = true
			if symbol.Size < uint64(runtime.JITTypeDescriptorSize()) {
				return releaseFailedImage(mapped, fmt.Errorf("jit: type descriptor %q is smaller than the runtime ABI", symbol.Name))
			}
			if runtime.JITTypeUsesGCMaskOnDemand(unsafe.Pointer(mapped.symbols[symbolIndex])) {
				return releaseFailedImage(mapped, fmt.Errorf("jit: type descriptor %q requires a mutable on-demand GC mask", symbol.Name))
			}
		case ImageITable:
			hasTypeMetadata = true
		}
	}
	if textIndex < 0 {
		return releaseFailedImage(mapped, fmt.Errorf("jit: linked image has no text section"))
	}
	functions := make([]runtime.JITFunctionMetadata, len(image.Functions))
	for i, function := range image.Functions {
		symbol := image.Symbols[function.Symbol]
		functions[i] = runtime.JITFunctionMetadata{
			Offset: uint32(symbol.Offset), Size: uint32(symbol.Size), Name: symbol.Name,
			ArgsSize: function.ArgsSize, StartLine: function.StartLine,
			FuncID: function.FuncID, FuncFlag: function.FuncFlag,
			PCSP: function.PCSP, PCFile: function.PCFile, PCLine: function.PCLine,
			Files: function.Files, PCData: function.PCData, FuncData: function.FuncData,
		}
	}
	globals := make([]runtime.JITGlobalMetadata, len(image.GlobalRoots))
	for i, root := range image.GlobalRoots {
		dataSymbol := image.Symbols[root.DataSymbol]
		typeAddress := uintptr(0)
		if root.TypeImport {
			typeAddress = mapped.imports[root.TypeSymbol]
		} else {
			if importIndex, canonical := canonicalTypes[root.TypeSymbol]; canonical {
				typeAddress = mapped.imports[importIndex]
			} else {
				typeAddress = mapped.symbols[root.TypeSymbol]
			}
		}
		globals[i] = runtime.JITGlobalMetadata{
			Address: mapped.symbols[root.DataSymbol], Size: uintptr(dataSymbol.Size), Type: unsafe.Pointer(typeAddress),
		}
	}
	text := mapped.sections[textIndex]
	typesStart, typesEnd := uintptr(0), uintptr(0)
	if hasTypeMetadata {
		if rodataStart == 0 {
			return releaseFailedImage(mapped, fmt.Errorf("jit: type metadata has no read-only image base"))
		}
		typesStart = rodataStart
		typesEnd = rodataStart
		for i, section := range image.Sections {
			switch section.Kind {
			case ImageReadOnly, ImageTypes, ImageITabs:
				sectionStart := uintptrPointer(mapped.sections[i])
				if uintptr(section.MemSize) > ^uintptr(0)-sectionStart {
					return releaseFailedImage(mapped, fmt.Errorf("jit: read-only section %d address range overflows", i))
				}
				if sectionEnd := sectionStart + uintptr(section.MemSize); sectionEnd > typesEnd {
					typesEnd = sectionEnd
				}
			}
		}
		if typesEnd <= typesStart {
			return releaseFailedImage(mapped, fmt.Errorf("jit: type metadata has an empty read-only image range"))
		}
	}
	mapping, ok := mapped.runtimeMapping.(*runtime.JITMapping)
	if !ok {
		return releaseFailedImage(mapped, fmt.Errorf("jit: runtime mapping token has unexpected implementation %T", mapped.runtimeMapping))
	}
	register := func() (*runtime.JITModuleOwner, error) {
		return runtime.RegisterJITModule(guard, uintptrPointer(text), uintptr(image.Sections[textIndex].MemSize), rodataStart, typesStart, typesEnd-typesStart, mapping, mapped.control, functions, globals, mapped.typeMappings, mapped.typeDescriptors, compiled.captures)
	}
	owner, err := register()
	if isJITMemoryLimitError(err) {
		runtime.GC()
		if runtime.JITWaitForRetirements(5e9) {
			owner, err = register()
		}
	}
	if err != nil {
		return releaseFailedImage(mapped, fmt.Errorf("jit: register generated runtime tables: %w", err))
	}
	mapped.runtimeOwner = owner
	mapped.ownerAware = compiled.ownerAware
	if !compiled.ownerAware {
		runtime.PinJITModule(owner)
		mapped.pinnedReason = "compiled entry has no generated GC owner propagation"
	}
	if compiled.pinRequired || compiled.effects.requiresPin() || hostEffectPinReason != "" {
		runtime.PinJITModule(owner)
		if mapped.pinnedReason == "" {
			if compiled.effects.requiresPin() {
				mapped.pinnedReason = compiled.effects.pinReason()
			} else {
				mapped.pinnedReason = hostEffectPinReason
			}
		}
	}
	return mapped, nil
}

func validateLinkedSnippetIdentity(image ModuleImage, metadata *Metadata) error {
	if image.GOOS != runtime.GOOS || image.GOARCH != runtime.GOARCH {
		return fmt.Errorf("jit: linked snippet target %s/%s does not match runtime %s/%s", image.GOOS, image.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	if image.GoVersion != runtime.Version() {
		return fmt.Errorf("jit: linked snippet Go version %q does not match runtime %q", image.GoVersion, runtime.Version())
	}
	if metadata != nil && metadata.Build.PackagePath != "" && image.PackagePath != metadata.Build.PackagePath {
		return fmt.Errorf("jit: linked snippet package %q does not match host package %q", image.PackagePath, metadata.Build.PackagePath)
	}
	return nil
}

func releaseFailedImage(mapped *mappedImage, cause error) (*mappedImage, error) {
	if err := closeMappedImage(mapped); err != nil {
		return mapped, errors.Join(cause, err)
	}
	return nil, cause
}

func resolveImageImport(metadata *Metadata, symbol ImageImport) (uintptr, error) {
	if metadata != nil {
		linked, ok := metadata.LookupLinkerSymbol(symbol.PackagePath, symbol.Name, symbol.ABI)
		if ok {
			if linked.Address == 0 {
				return 0, fmt.Errorf("host symbol %s.%s has no process address", symbol.PackagePath, symbol.Name)
			}
			if linked.FromManifest && linked.Type == nil && linked.Kind != ITableLink && linked.Kind != FunctionLink {
				return 0, fmt.Errorf("host symbol %s.%s has no linked Go type identity", symbol.PackagePath, symbol.Name)
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
				return 0, fmt.Errorf("host symbol %s.%s has invalid kind %q", symbol.PackagePath, symbol.Name, linked.Kind)
			}
			if kind != symbol.Kind {
				return 0, fmt.Errorf("host symbol %s.%s kind mismatch: metadata has %d, object expects %d", symbol.PackagePath, symbol.Name, kind, symbol.Kind)
			}
			if linked.FromManifest && linked.ABI != symbol.ABI {
				return 0, fmt.Errorf("host symbol %s.%s ABI mismatch: manifest has %d, object expects %d", symbol.PackagePath, symbol.Name, linked.ABI, symbol.ABI)
			}
			if linked.Type == nil {
				if symbol.ABIHash != ([16]byte{}) {
					return 0, fmt.Errorf("host symbol %s.%s has a type hash but no reflected type metadata", symbol.PackagePath, symbol.Name)
				}
			} else if linkedABIHash(linked.Type) != symbol.ABIHash {
				return 0, fmt.Errorf("host symbol %s.%s type identity does not match its serialized link metadata", symbol.PackagePath, symbol.Name)
			}
			return linked.Address, nil
		}
	}
	// Runtime aliases that are not linker manifest entries are resolved through
	// runtime registries. The selected linker manifest above remains preferred
	// so function relocations keep their exact ABI identity.
	if symbol.PackagePath == "runtime" {
		if symbol.Kind == ImageType {
			if address, ok := runtimeTypeAddress(symbol.Name); ok {
				return address, nil
			}
		}
		if symbol.Kind == ImageVariable {
			if address := runtime.ResolveJITVariable(symbol.Name); address != 0 {
				return address, nil
			}
		}
		if symbol.Kind == ImageFunction {
			if !runtimeFunctionABIResolvable(symbol.Name, symbol.ABI) {
				return 0, fmt.Errorf("runtime function runtime.%s ABI %d has no exact linker manifest entry", symbol.Name, symbol.ABI)
			}
			if address := runtime.ResolveJITFunction("runtime." + symbol.Name); address != 0 {
				return address, nil
			}
			return 0, fmt.Errorf("runtime function runtime.%s is absent from the runtime tables", symbol.Name)
		}
	}
	return 0, fmt.Errorf("host symbol %s.%s with ABI %d is absent from the link manifest", symbol.PackagePath, symbol.Name, symbol.ABI)
}
