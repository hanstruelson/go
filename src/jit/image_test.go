// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"
)

func TestJITMemoryLimitRejectsOverBudgetImage(t *testing.T) {
	_, oldLimit := MemoryStats()
	SetMemoryLimit(1)
	defer SetMemoryLimit(oldLimit)
	_, err := mapModuleImage(testModuleImage(), func(ImageImport) (uintptr, error) {
		return uintptr(unsafe.Pointer(new(uint64))), nil
	})
	var limitError MemoryLimitError
	if !errors.As(err, &limitError) {
		t.Fatalf("mapModuleImage error = %v, want a JIT memory limit error", err)
	}
	if limitError.Limit() != 1 || limitError.Requested() == 0 {
		t.Fatalf("memory limit error reports limit=%d requested=%d, want limit 1 and a positive request", limitError.Limit(), limitError.Requested())
	}
}

var heapJITPointerRoot *unsafe.Pointer

func init() {
	// Sampled memory-profile stacks store raw PCs for process lifetime. Disable
	// sampling so profile records do not intentionally pin fixture modules.
	runtime.MemProfileRate = 0
}

func testModuleImage() ModuleImage {
	return ModuleImage{
		GOOS: "linux", GOARCH: "amd64", GoVersion: "go1.test", ABI: "go-internal-v1",
		BuildID: "test-build", PackagePath: "example/jitmod", Entry: 0,
		Sections: []ImageSection{
			{Kind: ImageText, Align: 16, MemSize: 8, Data: []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90, 0x90, 0xc3}, Read: true, Execute: true},
			{Kind: ImageData, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true, Write: true},
			{Kind: ImagePCLN, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true},
		},
		Symbols: []ImageSymbol{{PackagePath: "example/jitmod", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 8}},
		Functions: []ImageFunctionMetadata{{
			Symbol: 0, PCSP: []byte{0, 8, 0}, PCData: [][]byte{{0, 8, 0}, {0, 8, 0}}, FuncData: [][]byte{{}, {}},
		}},
		Imports:     []ImageImport{{PackagePath: "jitcounter", Name: "Value", Kind: ImageVariable}},
		Relocations: []ImageRelocation{{Section: 1, Offset: 0, Kind: RelocAMD64Abs64, Target: 0, TargetImport: true}},
	}
}

func TestMapModuleImageRelocatesToHostStorage(t *testing.T) {
	image := testModuleImage()
	var shared uint64
	sharedAddress := uintptr(unsafe.Pointer(&shared))
	mapped, err := mapModuleImage(image, func(symbol ImageImport) (uintptr, error) {
		if symbol.PackagePath != "jitcounter" || symbol.Name != "Value" || symbol.Kind != ImageVariable {
			t.Fatalf("unexpected import: %+v", symbol)
		}
		return sharedAddress, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapped.entry == 0 {
		t.Fatal("mapped image has a nil entry")
	}
	if got := binary.LittleEndian.Uint64(mapped.sections[1]); got != uint64(sharedAddress) {
		t.Fatalf("absolute relocation resolved to %#x, want host storage %#x", got, sharedAddress)
	}
}

func TestLinuxAMD64TLSLocalExecRelocationLoadsG(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("Linux/amd64 TLS relocation")
	}
	image := ModuleImage{
		GOOS: "linux", GOARCH: "amd64", GoVersion: runtime.Version(),
		ABI: "test-amd64", BuildID: "tls-le", PackagePath: "jit/test/tls",
		Sections: []ImageSection{{
			Kind: ImageText, Align: 16, MemSize: 10,
			// MOVQ FS:-8, AX; RET. The displacement is supplied by the
			// internal linker's Linux/amd64 local-exec TLS rule.
			Data: []byte{0x64, 0x48, 0x8b, 0x04, 0x25, 0, 0, 0, 0, 0xc3},
			Read: true, Execute: true,
		}},
		Symbols: []ImageSymbol{{PackagePath: "jit/test/tls", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 10}},
		Functions: []ImageFunctionMetadata{{
			Symbol: 0, PCSP: []byte{0}, PCData: [][]byte{{0}, {0}},
			FuncData: [][]byte{{}, {}},
		}},
		Relocations: []ImageRelocation{{
			Section: 0, Offset: 5, Kind: RelocAMD64TLSLE32, Target: ^uint32(0),
		}},
		Entry: 0,
	}
	mapped, err := mapModuleImage(image, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mapped.runtimeMapping.Unmap(); err != nil {
			t.Errorf("unmap TLS fixture: %v", err)
		}
	}()
	if got := int32(binary.LittleEndian.Uint32(mapped.sections[0][5:9])); got != -8 {
		t.Fatalf("TLS local-exec displacement = %d, want -8", got)
	}
	if got := callEntryInt(mapped.entry); got == 0 {
		t.Fatal("Linux/amd64 TLS local-exec relocation did not load the current goroutine")
	}
}

func TestMapModuleImageResolvesGOTPCRELThroughImageSlot(t *testing.T) {
	var shared uint64
	sharedAddress := uintptr(unsafe.Pointer(&shared))
	for _, tc := range []struct {
		name string
		kind ImageRelocationKind
	}{{"GOTPCREL", RelocAMD64GOTPCREL}, {"GOTPCRELX", RelocAMD64GOTPCRELX}} {
		t.Run(tc.name, func(t *testing.T) {
			image := ModuleImage{
				GOOS: "linux", GOARCH: "amd64", GoVersion: "go1.test", ABI: "go-internal-v1",
				BuildID: "got-test", PackagePath: "example/jitgot", Entry: 0,
				Sections: []ImageSection{
					{Kind: ImageText, Align: 16, MemSize: 8, Data: []byte{0x48, 0x8b, 0x05, 0, 0, 0, 0, 0xc3}, Read: true, Execute: true},
					{Kind: ImageReadOnly, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true},
				},
				Symbols: []ImageSymbol{
					{PackagePath: "example/jitgot", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 8},
					{PackagePath: "example/jitgot", Name: "jit.got.slot.0", Kind: ImageVariable, Section: 1, Size: 8},
				},
				Functions: []ImageFunctionMetadata{{
					Symbol: 0, PCSP: []byte{0}, PCData: [][]byte{{0}, {0}},
					FuncData: [][]byte{nil, nil},
				}},
				Imports: []ImageImport{{PackagePath: "jitcounter", Name: "Value", Kind: ImageVariable}},
				Relocations: []ImageRelocation{
					{Section: 1, Offset: 0, Kind: RelocAMD64Abs64, Target: 0, TargetImport: true},
					{Section: 0, Offset: 3, Kind: tc.kind, Target: 1},
				},
			}
			mapped, err := mapModuleImage(image, func(symbol ImageImport) (uintptr, error) {
				if symbol.PackagePath != "jitcounter" || symbol.Name != "Value" {
					t.Fatalf("unexpected GOT import: %+v", symbol)
				}
				return sharedAddress, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := mapped.runtimeMapping.Unmap(); err != nil {
					t.Errorf("unmap image: %v", err)
				}
			}()
			if got := binary.LittleEndian.Uint64(mapped.sections[1]); got != uint64(sharedAddress) {
				t.Fatalf("GOT slot contains %#x, want host storage %#x", got, sharedAddress)
			}
			delta := int32(binary.LittleEndian.Uint32(mapped.sections[0][3:7]))
			gotAddress := uintptr(int64(uintptrPointer(mapped.sections[0])+7) + int64(delta))
			wantAddress := uintptrPointer(mapped.sections[1])
			if gotAddress != wantAddress {
				t.Fatalf("GOTPCREL displacement resolves to %#x, want slot %#x", gotAddress, wantAddress)
			}
		})
	}
}

func TestParseObjectArchiveReadsCompilerOutputFromMemory(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jitimage/testpkg",
		Files:       []SourceFile{{Name: "eval.go", Data: "package testpkg\nfunc Eval() {}\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := ParseObjectArchive(compiled.Object)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) == 0 {
		t.Fatal("compiler archive contained no Go objects")
	}
	found := false
	for _, object := range objects {
		symbols, err := object.Symbols()
		if err != nil {
			t.Fatal(err)
		}
		if len(symbols) == 0 {
			continue
		}
		for _, symbol := range symbols {
			if symbol.Name != "" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("compiler objects contained no symbols")
	}
}

func TestLoadAndRunSelfContainedFunctionFromMemory(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jitimage/leaf",
		Files:       []SourceFile{{Name: "eval.go", Data: "package leaf\nfunc Eval() int { return 42 }\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// This fixture has no escaping function values or JIT data. Model the
	// owner-bearing entry ABI so Close may release the caller's handle.
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	entry := code.image.entry
	if fn := runtime.FuncForPC(entry); fn == nil {
		t.Fatal("registered JIT entry is missing from runtime PC tables")
	}
	got, err := code.RunInt()
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("in-memory function returned %d, want 42", got)
	}
	if err := code.Close(); err != nil {
		t.Fatal(err)
	}
	if fn := runtime.FuncForPC(entry); fn == nil {
		t.Fatal("FuncForPC did not retain the metadata handle returned before Close")
	}
}

func TestOwnerAwareModuleRetiresAfterGC(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jitimage/reclaim",
		Files:       []SourceFile{{Name: "eval.go", Data: "package reclaim\nfunc Eval(_ uintptr) int { return 42 }\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// This fixture models the owner-bearing entry ABI. The body has no nested
	// closures, raw PCs, or runtime type data that could outlive Code.Run.
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	entry := code.image.entry
	owner := weak.Make(code.image.runtimeOwner)
	if got, err := code.RunInt(); err != nil || got != 42 {
		t.Fatalf("run owner-aware entry: got %d, err %v", got, err)
	}
	if !runtime.JITModuleRegistered(entry) {
		t.Fatal("JIT entry was not published before Close")
	}
	if err := code.Close(); err != nil {
		t.Fatal(err)
	}
	code = nil
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if !runtime.JITModuleRegistered(entry) {
			if live := owner.Value(); live != nil {
				t.Fatal("module metadata was unpublished while its GC owner remained live")
			}
			return
		}
		runtime.Gosched()
	}
	t.Fatal("owner-aware module did not retire after forced GC")
}

func TestCodePCKeepsModuleAliveAfterCodeClose(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jitimage/codepc",
		Files:       []SourceFile{{Name: "eval.go", Data: "package codepc\nfunc Eval() {}\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	owner := weak.Make(code.image.runtimeOwner)
	pc, err := code.PC()
	if err != nil {
		t.Fatal(err)
	}
	entry := pc.Address()
	if err := code.Close(); err != nil {
		t.Fatal(err)
	}
	code = nil
	runtime.GC()
	if !runtime.JITModuleRegistered(entry) || owner.Value() == nil {
		t.Fatal("CodePC did not keep its module owner alive")
	}
	runtime.KeepAlive(pc)
	pc = CodePC{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if !runtime.JITModuleRegistered(entry) {
			if owner.Value() != nil {
				t.Fatal("CodePC owner remained live after dropping the handle")
			}
			return
		}
		runtime.Gosched()
	}
	t.Fatal("CodePC module did not retire after dropping the handle")
}

func TestHeapPointerIntoJITMappingRetainsOwner(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jitimage/pointerowner",
		Files: []SourceFile{{Name: "eval.go", Data: `package pointerowner
import "unsafe"
func Eval(_ uintptr) unsafe.Pointer { return unsafe.Pointer(unsafe.StringData("jit-owner-edge")) }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	var entry, mapBase uintptr
	var mapSize int
	var owner weak.Pointer[runtime.JITModuleOwner]
	var pointer unsafe.Pointer
	func() {
		code, err := (*Metadata)(nil).LoadSnippet(compiled)
		if err != nil {
			t.Fatal(err)
		}
		entry = code.image.entry
		mapBase = code.image.control
		mapSize = len(code.image.mapping)
		owner = weak.Make(code.image.runtimeOwner)
		pointer, err = code.RunPointer()
		if err != nil {
			t.Fatal(err)
		}
		if err := code.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if uintptr(pointer) < mapBase || uintptr(pointer) >= mapBase+uintptr(mapSize) {
		t.Fatalf("returned pointer %p is outside JIT mapping [%#x,%#x)", pointer, mapBase, mapBase+uintptr(mapSize))
	}
	root := new(unsafe.Pointer)
	*root = pointer
	// Keep the slot itself in the Go heap so this exercises heap-object
	// scanning, even if the compiler could otherwise stack-allocate it.
	heapJITPointerRoot = root
	pointer = nil
	runtime.GC()
	if !runtime.JITModuleRegistered(entry) {
		t.Fatal("typed heap pointer into JIT mapping did not retain its module")
	}
	if live := owner.Value(); live == nil {
		t.Fatal("typed heap pointer did not retain the JIT owner")
	}
	*root = nil
	heapJITPointerRoot = nil
	root = nil
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if !runtime.JITModuleRegistered(entry) {
			if live := owner.Value(); live != nil {
				t.Fatal("JIT owner remained live after dropping its only mapped pointer")
			}
			return
		}
		runtime.Gosched()
	}
	t.Fatal("JIT module did not retire after its mapped pointer was dropped")
}

func TestLinkObjectArchiveLinksLocalFunctionAndPackageVariable(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jitlink/coverage",
		Files: []SourceFile{{Name: "eval.go", Data: `package coverage

var shared int

//go:noinline
func helper(n int) int { return n * 2 }

func Eval() int {
	shared = helper(21)
	return shared
}
`}},
		Options: []string{"-l=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	image, err := LinkObjectArchive(compiled.Object, "Eval", nil)
	if err != nil {
		t.Fatal(err)
	}

	helperIndex, sharedIndex := -1, -1
	for i, symbol := range image.Symbols {
		if strings.HasSuffix(symbol.Name, ".helper") && symbol.Kind == ImageFunction {
			helperIndex = i
		}
		if strings.HasSuffix(symbol.Name, ".shared") && symbol.Kind == ImageVariable {
			sharedIndex = i
			if image.Sections[symbol.Section].Kind != ImageBSS && image.Sections[symbol.Section].Kind != ImageData {
				t.Fatalf("package variable is in unexpected section %v", image.Sections[symbol.Section].Kind)
			}
		}
	}
	if helperIndex < 0 || sharedIndex < 0 {
		t.Fatalf("linked image omitted local helper or package variable: helper=%d shared=%d", helperIndex, sharedIndex)
	}
	hasLocalCall, hasSharedDataReference := false, false
	for _, relocation := range image.Relocations {
		if relocation.TargetImport {
			continue
		}
		if int(relocation.Target) == helperIndex && relocation.Kind == RelocAMD64PLT32 {
			hasLocalCall = true
		}
		if int(relocation.Target) == sharedIndex {
			hasSharedDataReference = true
		}
	}
	if !hasLocalCall || !hasSharedDataReference {
		t.Fatalf("linked image lacks local call/data relocations: call=%v data=%v", hasLocalCall, hasSharedDataReference)
	}
	if len(image.Functions) == 0 || len(image.Functions[0].PCFile) == 0 || len(image.Functions[0].PCLine) == 0 || len(image.Functions[0].Files) == 0 {
		t.Fatal("linked image omitted source location metadata")
	}

	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := code.RunInt(); err != nil {
		_ = code.Close()
		t.Fatal(err)
	} else if got != 42 {
		_ = code.Close()
		t.Fatalf("linked function returned %d, want 42", got)
	}
	if err := code.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestObjectRelocationNamesImportedFunction(t *testing.T) {
	var exports map[string]PackageExportData
	for _, metadata := range RegisteredMetadata() {
		exports = make(map[string]PackageExportData, len(metadata.Build.Packages))
		for _, pkg := range metadata.Build.Packages {
			exports[pkg.Path] = PackageExportData{
				PackagePath:       pkg.Path,
				PackageName:       pkg.Name,
				ObjectHeader:      pkg.ObjectHeader,
				ExportDataVersion: pkg.ExportDataVersion,
				Fingerprint:       pkg.Fingerprint,
				Data:              pkg.Data,
			}
		}
		if len(exports["fmt"].Data) != 0 {
			break
		}
	}
	if len(exports["fmt"].Data) == 0 {
		t.Fatal("host metadata has no fmt export record")
	}
	compiled, err := Compile(CompileRequest{
		PackagePath:    "jitimage/fmtuse",
		Files:          []SourceFile{{Name: "eval.go", Data: "package fmtuse\nimport \"fmt\"\nfunc Eval() { fmt.Println(\"dynamic\") }\n"}},
		PackageExports: exports,
	})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := ParseObjectArchive(compiled.Object)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, object := range objects {
		symbols, err := object.Symbols()
		if err != nil {
			t.Fatal(err)
		}
		for _, symbol := range symbols {
			if !strings.HasSuffix(symbol.Name, ".Eval") {
				continue
			}
			for _, relocation := range symbol.Relocations {
				ref, err := object.ResolveReference(relocation.Target)
				if err != nil {
					t.Fatal(err)
				}
				if ref.PackagePath == "fmt" && (ref.Name == "Fprintln" || strings.HasSuffix(ref.Name, ".Fprintln")) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("Eval object had no resolvable relocation to fmt.Fprintln")
	}
}

func TestModuleImageRoundTrip(t *testing.T) {
	want := testModuleImage()
	baseSections := want.Sections
	want.Sections = []ImageSection{baseSections[0], {
		Kind: ImageTypes, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true,
	}, baseSections[1], baseSections[2]}
	want.Relocations = append([]ImageRelocation(nil), want.Relocations...)
	for i := range want.Relocations {
		if want.Relocations[i].Section > 0 {
			want.Relocations[i].Section++
		}
	}
	want.Symbols = append(want.Symbols, ImageSymbol{
		PackagePath: "example/jitmod", Name: "type:func() int", Kind: ImageType, Section: 1, Size: 8,
	})
	want.Imports = append(want.Imports, ImageImport{
		PackagePath: "runtime", Name: "func() int", Kind: ImageType,
	})
	want.TypeAliases = []ImageTypeAlias{{Symbol: 1, Import: 1}}
	data, err := MarshalModuleImage(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalModuleImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.GOOS != want.GOOS || got.GOARCH != want.GOARCH || got.PackagePath != want.PackagePath || got.Entry != want.Entry {
		t.Fatalf("module identity mismatch: got %+v", got)
	}
	if len(got.Sections) != len(want.Sections) || !bytes.Equal(got.Sections[0].Data, want.Sections[0].Data) {
		t.Fatalf("module sections mismatch: got %+v", got.Sections)
	}
	if len(got.Imports) != 2 || got.Imports[0] != want.Imports[0] || got.Imports[1] != want.Imports[1] {
		t.Fatalf("module imports mismatch: got %+v", got.Imports)
	}
	if len(got.TypeAliases) != 1 || got.TypeAliases[0] != want.TypeAliases[0] {
		t.Fatalf("module type aliases mismatch: got %+v", got.TypeAliases)
	}
	if len(got.Relocations) != 1 || got.Relocations[0] != want.Relocations[0] {
		t.Fatalf("module relocations mismatch: got %+v", got.Relocations)
	}
}

func TestModuleImageGlobalRootRoundTrip(t *testing.T) {
	want := testModuleImage()
	want.Symbols = append(want.Symbols, ImageSymbol{
		PackagePath: "example/jitmod", Name: "shared", Kind: ImageVariable,
		GCTracked: true, Section: 1, Offset: 0, Size: 8,
	})
	want.Imports = append(want.Imports, ImageImport{
		PackagePath: "runtime", Name: "type:*int", Kind: ImageType,
	})
	want.GlobalRoots = []ImageGlobalRoot{{DataSymbol: 1, TypeSymbol: 1, TypeImport: true}}
	encoded, err := MarshalModuleImage(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalModuleImage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GlobalRoots) != 1 || got.GlobalRoots[0] != want.GlobalRoots[0] {
		t.Fatalf("global root metadata = %+v, want %+v", got.GlobalRoots, want.GlobalRoots)
	}
	if !got.Symbols[1].GCTracked {
		t.Fatal("global symbol lost its GC tracking marker")
	}

	invalid := want
	invalid.GlobalRoots = nil
	if err := validateModuleImage(invalid); err == nil {
		t.Fatal("module image accepted pointerful writable data without a GC root")
	}
}

func TestModuleImageRejectsCorruptOrInvalidData(t *testing.T) {
	data, err := MarshalModuleImage(testModuleImage())
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := UnmarshalModuleImage(corrupt); err == nil {
		t.Fatal("UnmarshalModuleImage accepted a checksum mismatch")
	}

	invalid := testModuleImage()
	invalid.Relocations[0].Offset = 4
	if _, err := MarshalModuleImage(invalid); err == nil {
		t.Fatal("MarshalModuleImage accepted an out-of-bounds relocation")
	}

	invalid = testModuleImage()
	invalid.Entry = 99
	if _, err := MarshalModuleImage(invalid); err == nil {
		t.Fatal("MarshalModuleImage accepted an invalid entry index")
	}

	invalid = testModuleImage()
	invalid.Functions = nil
	if _, err := MarshalModuleImage(invalid); err == nil {
		t.Fatal("MarshalModuleImage accepted text without function runtime metadata")
	}
}
