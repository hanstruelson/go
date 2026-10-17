// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

import (
	"fmt"
	"runtime"
)

func loadLinkedSnippet(metadata *Metadata, compiled CompiledPackage) (*mappedImage, error) {
	image, err := LinkObjectArchive(compiled.Object, "Eval", metadata)
	if err != nil {
		return nil, err
	}
	mapped, err := mapModuleImage(image, func(symbol ImageImport) (uintptr, error) {
		return resolveImageImport(metadata, symbol)
	})
	if err != nil {
		return nil, err
	}
	textIndex := -1
	rodataStart := uintptr(0)
	for i, section := range image.Sections {
		if section.Kind == ImageText {
			textIndex = i
		}
		if section.Kind == ImageReadOnly {
			rodataStart = uintptrPointer(mapped.sections[i])
		}
	}
	if textIndex < 0 {
		_ = closeMappedImage(mapped)
		return nil, fmt.Errorf("jit: linked image has no text section")
	}
	functions := make([]runtime.JITFunctionMetadata, len(image.Functions))
	for i, function := range image.Functions {
		symbol := image.Symbols[function.Symbol]
		functions[i] = runtime.JITFunctionMetadata{
			Offset: uint32(symbol.Offset), Size: uint32(symbol.Size), Name: symbol.Name,
			ArgsSize: function.ArgsSize, StartLine: function.StartLine,
			FuncID: function.FuncID, FuncFlag: function.FuncFlag,
			PCSP: function.PCSP, PCData: function.PCData, FuncData: function.FuncData,
		}
	}
	text := mapped.sections[textIndex]
	module, err := runtime.RegisterJITModule(uintptrPointer(text), uintptr(image.Sections[textIndex].MemSize), rodataStart, functions)
	if err != nil {
		_ = closeMappedImage(mapped)
		return nil, fmt.Errorf("jit: register generated runtime tables: %w", err)
	}
	mapped.runtimeModule = module
	return mapped, nil
}

func resolveImageImport(metadata *Metadata, symbol ImageImport) (uintptr, error) {
	if symbol.PackagePath == "runtime" {
		if symbol.Kind == ImageType {
			if address, ok := runtimeTypeAddress(symbol.Name); ok {
				return address, nil
			}
		}
		if symbol.Kind == ImageFunction {
			if address := runtime.ResolveJITFunction("runtime." + symbol.Name); address != 0 {
				return address, nil
			}
			return 0, fmt.Errorf("runtime function runtime.%s is absent from the runtime tables", symbol.Name)
		}
	}
	linked, ok := metadata.LookupSymbol(symbol.PackagePath, symbol.Name)
	if !ok {
		return 0, fmt.Errorf("host symbol %s.%s is absent from the link manifest", symbol.PackagePath, symbol.Name)
	}
	if linked.Address == 0 {
		return 0, fmt.Errorf("host symbol %s.%s has no process address", symbol.PackagePath, symbol.Name)
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
		return 0, fmt.Errorf("host symbol %s.%s has invalid kind %q", symbol.PackagePath, symbol.Name, linked.Kind)
	}
	if kind != symbol.Kind {
		return 0, fmt.Errorf("host symbol %s.%s kind mismatch: metadata has %d, object expects %d", symbol.PackagePath, symbol.Name, kind, symbol.Kind)
	}
	return linked.Address, nil
}
