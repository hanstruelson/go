// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func testModuleImage() ModuleImage {
	return ModuleImage{
		GOOS: "linux", GOARCH: "amd64", GoVersion: "go1.test", ABI: "go-internal-v1",
		BuildID: "test-build", PackagePath: "example/jitmod", Entry: 0,
		Sections: []ImageSection{
			{Kind: ImageText, Align: 16, MemSize: 8, Data: []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90, 0x90, 0xc3}, Read: true, Execute: true},
			{Kind: ImageData, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true, Write: true},
			{Kind: ImagePCLN, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true},
		},
		Symbols:     []ImageSymbol{{PackagePath: "example/jitmod", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 8}},
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
	if fn := runtime.FuncForPC(entry); fn != nil {
		t.Fatalf("unloaded JIT entry is still present in runtime PC tables as %s", fn.Name())
	}
}

func TestObjectRelocationNamesImportedFunction(t *testing.T) {
	var archives map[string][]byte
	for _, metadata := range RegisteredMetadata() {
		archives = make(map[string][]byte, len(metadata.Build.Packages))
		for _, pkg := range metadata.Build.Packages {
			if len(pkg.Archive) != 0 {
				archives[pkg.Path] = pkg.Archive
			}
		}
		if len(archives["fmt"]) != 0 {
			break
		}
	}
	if len(archives["fmt"]) == 0 {
		t.Fatal("host metadata has no ordinary fmt archive")
	}
	compiled, err := Compile(CompileRequest{
		PackagePath:     "jitimage/fmtuse",
		Files:           []SourceFile{{Name: "eval.go", Data: "package fmtuse\nimport \"fmt\"\nfunc Eval() { fmt.Println(\"dynamic\") }\n"}},
		PackageArchives: archives,
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
	if len(got.Imports) != 1 || got.Imports[0] != want.Imports[0] {
		t.Fatalf("module imports mismatch: got %+v", got.Imports)
	}
	if len(got.Relocations) != 1 || got.Relocations[0] != want.Relocations[0] {
		t.Fatalf("module relocations mismatch: got %+v", got.Relocations)
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
}
