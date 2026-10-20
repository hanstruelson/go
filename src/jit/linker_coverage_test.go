// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"encoding/binary"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"cmd/compile/publicinternal/jitmeta"
	"cmd/publicinternal/archive"
	"cmd/publicinternal/goobj"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
)

func TestCanonicalTypeAliasRewritesDirectRelocation(t *testing.T) {
	const canonicalTypeAddress = uintptr(0x12345678)
	image := ModuleImage{
		GOOS: "linux", GOARCH: "amd64", GoVersion: runtime.Version(),
		ABI: "test-amd64", BuildID: "canonical-type-alias", PackagePath: "jit/test",
		Sections: []ImageSection{
			{Kind: ImageText, Align: 16, MemSize: 1, Data: []byte{0xc3}, Read: true, Execute: true},
			{Kind: ImageTypes, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true},
			{Kind: ImageData, Align: 8, MemSize: 8, Data: make([]byte, 8), Read: true, Write: true},
		},
		Symbols: []ImageSymbol{
			{PackagePath: "jit/test", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 1},
			{PackagePath: "jit/test", Name: "typeSlot", Kind: ImageVariable, Section: 2, Size: 8},
			{PackagePath: "jit/test", Name: "type:func()", Kind: ImageType, Section: 1, Size: 8},
		},
		Functions: []ImageFunctionMetadata{{
			Symbol: 0, PCSP: []byte{0}, PCData: [][]byte{{0}, {0}},
			FuncData: [][]byte{{0}, {0}},
		}},
		Imports:     []ImageImport{{PackagePath: "example/host", Name: "func()", Kind: ImageType}},
		TypeAliases: []ImageTypeAlias{{Symbol: 2, Import: 0}},
		Relocations: []ImageRelocation{{Section: 2, Offset: 0, Kind: RelocAMD64Abs64, Target: 2}},
		Entry:       0,
	}
	mapped, err := mapModuleImage(image, func(ImageImport) (uintptr, error) {
		return canonicalTypeAddress, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeMappedImage(mapped); err != nil {
			t.Errorf("release canonical type alias image: %v", err)
		}
	})
	if got := binary.LittleEndian.Uint64(mapped.sections[2]); got != uint64(canonicalTypeAddress) {
		t.Fatalf("direct type relocation = %#x, want canonical host descriptor %#x", got, canonicalTypeAddress)
	}
	if got := mapped.symbols[2]; got == canonicalTypeAddress {
		t.Fatal("image-local type descriptor was discarded; module-relative type offsets need its address")
	}
}

func TestAbsolute32RelocationRejectsAddressTruncation(t *testing.T) {
	image := ModuleImage{
		GOOS: "linux", GOARCH: "amd64", GoVersion: runtime.Version(),
		ABI: "test-amd64", BuildID: "abs32-overflow", PackagePath: "jit/test",
		Sections: []ImageSection{
			{Kind: ImageText, Align: 16, MemSize: 1, Data: []byte{0xc3}, Read: true, Execute: true},
			{Kind: ImageData, Align: 4, MemSize: 4, Data: make([]byte, 4), Read: true, Write: true},
		},
		Symbols: []ImageSymbol{{PackagePath: "jit/test", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 1}},
		Functions: []ImageFunctionMetadata{{
			Symbol: 0, PCSP: []byte{0}, PCData: [][]byte{{0}, {0}}, FuncData: [][]byte{{0}, {0}},
		}},
		Imports:     []ImageImport{{PackagePath: "example/host", Name: "address", Kind: ImageVariable}},
		Relocations: []ImageRelocation{{Section: 1, Offset: 0, Kind: RelocAMD64Abs32, Target: 0, TargetImport: true}},
		Entry:       0,
	}
	if _, err := mapModuleImage(image, func(ImageImport) (uintptr, error) {
		return uintptr(1) << 32, nil
	}); err == nil || !strings.Contains(err.Error(), "does not fit in 32 bits") {
		t.Fatalf("32-bit absolute relocation error = %v, want checked truncation error", err)
	}
}

func TestLinkedSnippetIdentityMatchesHostSnapshot(t *testing.T) {
	image := ModuleImage{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
		PackagePath: "example/app",
	}
	metadata := &Metadata{Build: BuildMetadata{PackagePath: "example/app"}}
	if err := validateLinkedSnippetIdentity(image, metadata); err != nil {
		t.Fatalf("validate matching host identity: %v", err)
	}
	metadata.Build.PackagePath = "example/other"
	if err := validateLinkedSnippetIdentity(image, metadata); err == nil || !strings.Contains(err.Error(), "does not match host package") {
		t.Fatalf("validate mismatched package = %v, want package identity error", err)
	}
	metadata.Build.PackagePath = ""
	image.GoVersion += ".other"
	if err := validateLinkedSnippetIdentity(image, metadata); err == nil || !strings.Contains(err.Error(), "Go version") {
		t.Fatalf("validate mismatched Go version = %v, want version error", err)
	}
}

func TestBuiltinTypeHostImportCarriesStructuralIdentity(t *testing.T) {
	imp, address, err := hostImport(nil, ObjectReference{Name: "type:int"})
	if err != nil {
		t.Fatalf("resolve builtin type import: %v", err)
	}
	typ := reflect.TypeOf(int(0))
	if address == 0 || imp.Kind != ImageType || imp.PackagePath != "runtime" || imp.Name != "int" {
		t.Fatalf("builtin type import = %#v at %#x", imp, address)
	}
	if got, want := imp.ABIHash, linkedABIHash(typ); got != want {
		t.Fatalf("builtin type ABI hash = %x, want %x", got, want)
	}
}

func TestCallersPinsRawJITPC(t *testing.T) {
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
		if len(exports["runtime"].Data) != 0 {
			break
		}
	}
	if len(exports["runtime"].Data) == 0 {
		t.Fatal("host metadata has no runtime export record")
	}
	request := CompileRequest{
		PackagePath:    "jit/coverage/raw-pc",
		PackageExports: exports,
		Files: []SourceFile{{Name: "raw_pc.go", Data: `package p
import "runtime"
func Eval() int {
	pcs := make([]uintptr, 8)
	n := runtime.Callers(0, pcs)
	if n < 2 { return 0 }
	return int(pcs[1])
}
`}},
	}
	optimized, err := Compile(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LinkObjectArchive(optimized.Object, "Eval", nil); err == nil || !strings.Contains(err.Error(), "does not yet encode inline trees") {
		t.Fatalf("link optimized function with inline metadata = %v, want an explicit unsupported-inline error", err)
	}
	request.Options = []string{"-l=1"}
	compiled, err := Compile(request)
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := code.RunInt()
	if err != nil {
		t.Fatalf("run raw-PC fixture: %v", err)
	}
	if pc == 0 || !runtime.JITModuleRegistered(uintptr(pc)) {
		t.Fatalf("runtime.Callers did not return a JIT frame PC: %#x", pc)
	}
	if err := code.Close(); err != nil {
		t.Fatalf("close raw-PC fixture: %v", err)
	}
	code = nil
	runtime.GC()
	if !runtime.JITWaitForRetirements(5e9) {
		t.Fatal("timed out waiting for raw-PC owner decision")
	}
	if !runtime.JITModuleRegistered(uintptr(pc)) {
		t.Fatal("runtime.Callers returned a raw JIT PC without pinning its module")
	}
}

func TestModuleIndexRetiresWithConcurrentReaders(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/index-readers",
		Files:       []SourceFile{{Name: "index.go", Data: "package p\nfunc Eval() {}\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	linkerCoverageIndexPC = code.image.entry
	if !runtime.JITModuleRegistered(linkerCoverageIndexPC) {
		t.Fatal("new module was not published")
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					runtime.JITModuleRegistered(linkerCoverageIndexPC)
					runtime.Gosched()
				}
			}
		}()
	}
	if err := code.Close(); err != nil {
		t.Fatalf("close module with diagnostic readers: %v", err)
	}
	code = nil
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.JITWaitForRetirements(100e6) && !runtime.JITModuleRegistered(linkerCoverageIndexPC) {
			close(stop)
			readers.Wait()
			linkerCoverageIndexPC = 0
			return
		}
		runtime.Gosched()
	}
	close(stop)
	readers.Wait()
	linkerCoverageIndexPC = 0
	t.Fatal("module index did not retire while readers were active")
}

func TestAddroffRelocationRejectsWritableTarget(t *testing.T) {
	image := ModuleImage{
		GOOS: "linux", GOARCH: "amd64", GoVersion: runtime.Version(),
		ABI: "test-amd64", BuildID: "addroff-writable-target", PackagePath: "jit/test",
		Sections: []ImageSection{
			{Kind: ImageText, Align: 16, MemSize: 1, Data: []byte{0xc3}, Read: true, Execute: true},
			{Kind: ImageReadOnly, Align: 4, MemSize: 4, Data: make([]byte, 4), Read: true},
			{Kind: ImageData, Align: 4, MemSize: 4, Data: make([]byte, 4), Read: true, Write: true},
		},
		Symbols: []ImageSymbol{
			{PackagePath: "jit/test", Name: "Eval", Kind: ImageFunction, Section: 0, Size: 1},
			{PackagePath: "jit/test", Name: "writable", Kind: ImageVariable, Section: 2, Size: 4},
		},
		Functions: []ImageFunctionMetadata{{
			Symbol: 0, PCSP: []byte{0}, PCData: [][]byte{{0}, {0}},
			FuncData: [][]byte{{0}, {0}},
		}},
		Relocations: []ImageRelocation{{Section: 1, Offset: 0, Kind: RelocAMD64Addroff32, Target: 1}},
		Entry:       0,
	}
	if _, err := mapModuleImage(image, nil); err == nil || !strings.Contains(err.Error(), "non-metadata section") {
		t.Fatalf("addroff to writable global = %v, want rejection", err)
	}
}

var linkerCoverageDynamicInterface any
var linkerCoverageDynamicStruct any
var linkerCoverageCrossModuleValue any
var linkerCoverageIndexPC uintptr

func TestResolveImageImportChecksTypeIdentity(t *testing.T) {
	linkedType := reflect.TypeOf(func(int) string { return "" })
	metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{{
		PackagePath: "example/host", Name: "Function", Kind: FunctionLink,
		Address: 1, Type: linkedType, FromManifest: true,
	}}}}
	_, err := resolveImageImport(metadata, ImageImport{
		PackagePath: "example/host", Name: "Function", Kind: ImageFunction,
		ABIHash: [16]byte{1},
	})
	if err == nil || !strings.Contains(err.Error(), "type identity") {
		t.Fatalf("resolveImageImport error = %v, want type identity mismatch", err)
	}
}

func TestRuntimeImportPrefersExactManifestABI(t *testing.T) {
	typ := reflect.TypeOf(func() {})
	metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{{
		PackagePath: "runtime", Name: "jitManifestCoverageFunction", Kind: FunctionLink,
		ABI: uint16(obj.ABI0), Address: 0x12345678, Type: typ, FromManifest: true,
	}}}}
	got, err := resolveImageImport(metadata, ImageImport{
		PackagePath: "runtime", Name: "jitManifestCoverageFunction", Kind: ImageFunction,
		ABI: uint16(obj.ABI0), ABIHash: linkedABIHash(typ),
	})
	if err != nil || got != 0x12345678 {
		t.Fatalf("manifest runtime import = %#x, %v; want exact ABI0 address", got, err)
	}
	if _, err := resolveImageImport(metadata, ImageImport{
		PackagePath: "runtime", Name: "jitManifestCoverageFunction", Kind: ImageFunction,
		ABI: uint16(obj.ABIInternal), ABIHash: linkedABIHash(typ),
	}); err == nil {
		t.Fatal("runtime import resolved a symbol with the wrong calling ABI")
	}
}

func TestLinkedABIHashIncludesNestedTypeStructure(t *testing.T) {
	first := reflect.TypeOf(func([]int) struct{ value int } { return struct{ value int }{} })
	second := reflect.TypeOf(func([]int) struct{ value int64 } { return struct{ value int64 }{} })
	if linkedABIHash(first) == linkedABIHash(second) {
		t.Fatal("ABI hash ignored a nested result field type")
	}
	third := reflect.TypeOf(func([]int) struct{ value int } { return struct{ value int }{} })
	if linkedABIHash(first) != linkedABIHash(third) {
		t.Fatal("identical structural types produced different ABI hashes")
	}
}

func TestManifestValidationRequiresExactEvalPackageClosure(t *testing.T) {
	const (
		appPath  = "example/app"
		rootPath = "example/root"
		depPath  = "example/dep"
	)
	appFingerprint := [8]byte{1}
	rootFingerprint := [8]byte{2}
	depFingerprint := [8]byte{3}
	build := BuildMetadata{
		CompilerVersion: runtime.Version(),
		PackagePath:     appPath,
		Files:           []jitmeta.File{{Name: "main.go", Imports: []jitmeta.Import{{SourcePath: rootPath, Path: rootPath}}}},
		Packages: []jitmeta.PackageExport{
			{Path: rootPath, Fingerprint: rootFingerprint, Dependencies: []jitmeta.PackageDependency{{Path: depPath, Name: "dep"}}},
			{Path: depPath, Fingerprint: depFingerprint},
		},
	}
	manifest := []linkerManifestPackage{
		{path: appPath, fingerprint: appFingerprint},
		{path: rootPath, fingerprint: rootFingerprint},
		{path: depPath, fingerprint: depFingerprint},
	}
	if err := validateManifestForBuild(build, manifest); err != nil {
		t.Fatalf("validate complete manifest closure: %v", err)
	}

	withoutDependency := append([]linkerManifestPackage(nil), manifest[:2]...)
	if err := validateManifestForBuild(build, withoutDependency); err == nil || !strings.Contains(err.Error(), depPath) {
		t.Fatalf("validation without transitive dependency = %v, want missing %q error", err, depPath)
	}

	wrongFingerprint := append([]linkerManifestPackage(nil), manifest...)
	wrongFingerprint[2].fingerprint = [8]byte{4}
	if err := validateManifestForBuild(build, wrongFingerprint); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("validation with mismatched package fingerprint = %v, want fingerprint error", err)
	}
}

func TestResolvedLinkReferencesRequireLinkerSelectedAddress(t *testing.T) {
	fnType := reflect.TypeOf(func(int) string { return "" })
	var references = []LinkedSymbol{
		{PackagePath: "example/app", Name: "Run", Kind: FunctionLink, Address: 0x1000, Type: fnType},
		{PackagePath: "example/dep", Name: "Counter", Kind: VariableLink, Address: 0x2000, Type: reflect.TypeOf(int(0))},
		// A Go type alias has no independent linker symbol or address.
		{PackagePath: "example/app", Name: "Alias", Kind: TypeLink, Address: 0x3000, Type: reflect.TypeOf(int(0))},
	}
	manifest := []LinkedSymbol{
		{PackagePath: "example/app", Name: "Run", Kind: FunctionLink, ABI: uint16(obj.ABIInternal), Address: 0x1000, Type: fnType, FromManifest: true},
		{PackagePath: "example/dep", Name: "Counter", Kind: VariableLink, ABI: uint16(obj.ABI0), Address: 0x2000, Type: reflect.TypeOf(int(0)), FromManifest: true},
	}
	if err := validateResolvedLinkReferences(references, manifest); err != nil {
		t.Fatalf("validate exact linked addresses: %v", err)
	}

	wrongAddress := append([]LinkedSymbol(nil), manifest...)
	wrongAddress[0].Address++
	if err := validateResolvedLinkReferences(references, wrongAddress); err == nil || !strings.Contains(err.Error(), "linker-selected address") {
		t.Fatalf("validation with changed function address = %v, want exact-address error", err)
	}

	missingVariable := manifest[:1]
	if err := validateResolvedLinkReferences(references, missingVariable); err == nil || !strings.Contains(err.Error(), "Counter") {
		t.Fatalf("validation without package variable = %v, want missing-symbol error", err)
	}
}

func TestMetadataCannotResolveSymbolsOutsideItsPackageClosure(t *testing.T) {
	allowed := map[string][8]byte{"example/app": {1}, "example/dep": {2}}
	metadata := &Metadata{
		manifestPackages: allowed,
		manifestSymbols: map[linkerSymbolKey]LinkedSymbol{
			{packagePath: "example/dep", name: "Run", abi: uint16(obj.ABIInternal)}: {
				PackagePath: "example/dep", Name: "Run", Kind: FunctionLink, ABI: uint16(obj.ABIInternal), Address: 0x1000, FromManifest: true,
			},
			{packagePath: "example/other", name: "Hidden", abi: uint16(obj.ABIInternal)}: {
				PackagePath: "example/other", Name: "Hidden", Kind: FunctionLink, ABI: uint16(obj.ABIInternal), Address: 0x2000, FromManifest: true,
			},
		},
	}
	if _, ok := metadata.LookupLinkerSymbol("example/dep", "Run", uint16(obj.ABIInternal)); !ok {
		t.Fatal("symbol in the validated package closure was not found")
	}
	if _, ok := metadata.LookupLinkerSymbol("example/other", "Hidden", uint16(obj.ABIInternal)); ok {
		t.Fatal("symbol outside the validated package closure was resolved")
	}
}

func TestObjectRelocationSupportTable(t *testing.T) {
	supported := []struct {
		typ  objabi.RelocType
		size uint8
		kind ImageRelocationKind
	}{
		{objabi.R_ADDR, 8, RelocAMD64Abs64},
		{objabi.R_ADDR, 4, RelocAMD64Abs32},
		{objabi.R_CALL, 4, RelocAMD64PLT32},
		{objabi.R_PCREL, 4, RelocAMD64PC32},
		{objabi.R_ADDROFF, 4, RelocAMD64Addroff32},
		{objabi.R_METHODOFF, 4, RelocAMD64Addroff32},
		{objabi.R_GOTPCREL, 4, RelocAMD64GOTPCREL},
		{objabi.R_TLS_LE, 4, RelocAMD64TLSLE32},
	}
	for _, tc := range supported {
		kind, err := imageRelocationFor(tc.typ, tc.size)
		if err != nil || kind != tc.kind {
			t.Errorf("imageRelocationFor(%s, %d) = %d, %v; want %d", tc.typ, tc.size, kind, err, tc.kind)
		}
	}
	markers := []objabi.RelocType{
		objabi.R_CALLIND, objabi.R_USEFIELD, objabi.R_USETYPE, objabi.R_USEIFACE,
		objabi.R_USEIFACEMETHOD, objabi.R_USENAMEDMETHOD, objabi.R_KEEP,
	}
	for _, typ := range markers {
		if kind, err := imageRelocationFor(typ, 0); err != nil || kind != 0 {
			t.Errorf("imageRelocationFor(%s, 0) = %d, %v; want a zero-width marker", typ, kind, err)
		}
	}
	unsupported := []struct {
		typ  objabi.RelocType
		size uint8
	}{
		{objabi.R_TLS_IE, 4},
		{objabi.R_ADDRARM64, 8},
		{objabi.R_CALL, 8},
		{objabi.R_GOTPCREL, 8},
		{objabi.R_KEEP, 4},
	}
	for _, tc := range unsupported {
		if _, err := imageRelocationFor(tc.typ, tc.size); err == nil {
			t.Errorf("imageRelocationFor(%s, %d) unexpectedly succeeded", tc.typ, tc.size)
		}
	}
}

func TestCloseRejectsActiveRunAndThenRejectsNewRuns(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/close",
		Files:       []SourceFile{{Name: "close.go", Data: "package p\nfunc Eval() {}\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, done, err := code.beginRun()
	if err != nil {
		t.Fatal(err)
	}
	if err := code.Close(); err == nil || !strings.Contains(err.Error(), "Run call") {
		t.Fatalf("Close with an active Run = %v, want active-call error", err)
	}
	done()
	if err := code.Close(); err != nil {
		t.Fatalf("Close after the active Run guard finished: %v", err)
	}
	if _, _, _, _, err := code.beginRun(); err == nil {
		t.Fatal("beginRun succeeded after Close")
	}
	if err := code.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRuntimeTypeAddressResolvesPrelinkedTypeShapes(t *testing.T) {
	cases := []struct {
		expr string
		typ  reflect.Type
	}{
		{expr: "string", typ: reflect.TypeOf("")},
		{expr: "[]string", typ: reflect.TypeOf([]string(nil))},
		{expr: "[4]byte", typ: reflect.TypeOf([4]byte{})},
		{expr: "map[string]int", typ: reflect.TypeOf(map[string]int(nil))},
		{expr: "<-chan *uint8", typ: reflect.TypeOf((<-chan *uint8)(nil))},
		{expr: "func(int, ...string) (bool, error)", typ: reflect.TypeOf((*func(int, ...string) (bool, error))(nil)).Elem()},
		{expr: "interface{}", typ: reflect.TypeOf((*any)(nil)).Elem()},
		{expr: "unsafe.Pointer", typ: reflect.TypeOf(unsafe.Pointer(nil))},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			address, ok := runtimeTypeAddress("type:" + tc.expr)
			if !ok {
				t.Fatalf("runtimeTypeAddress rejected %s", tc.expr)
			}
			want := uintptr((*struct {
				tab  unsafe.Pointer
				data unsafe.Pointer
			})(unsafe.Pointer(&tc.typ)).data)
			if address != want {
				t.Fatalf("runtimeTypeAddress(%s) = %#x, want %#x", tc.expr, address, want)
			}
		})
	}
	for _, expr := range []string{"unknown.Type", "map[[]int]int", "[999999999999999999999999]int"} {
		if _, ok := runtimeTypeAddress("type:" + expr); ok {
			t.Errorf("runtimeTypeAddress accepted unsupported or invalid type %s", expr)
		}
	}
}

func TestCompilerGeneratedPanicCanBeRecoveredAndRawFuncPinsModule(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/panic",
		Files: []SourceFile{{Name: "panic.go", Data: `package p
func Eval() { panic("jit panic") }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// This fixture has no escaping code values, and the raw Func lookup below
	// explicitly pins its runtime metadata for process lifetime.
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	entry := code.image.entry
	if runtime.FuncForPC(entry) == nil {
		t.Fatal("panic entry is missing from runtime function tables")
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		if err := code.Run(); err != nil {
			t.Errorf("run panic fixture: %v", err)
		}
	}()
	if recovered != "jit panic" {
		t.Fatalf("recovered panic = %#v, want %q", recovered, "jit panic")
	}
	if err := code.Close(); err != nil {
		t.Fatalf("Close after recovered panic: %v", err)
	}
	if runtime.FuncForPC(entry) == nil {
		t.Fatal("legacy FuncForPC metadata was not retained after Close")
	}
}

func TestPointerfulJITGlobalIsScannedByGC(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/global-root",
		Files: []SourceFile{{Name: "global.go", Data: `package p
var shared *int
func Eval() int {
	if shared == nil { value := new(int); *value = 73; shared = value }
	return *shared
}
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	image, err := LinkObjectArchive(compiled.Object, "Eval", nil)
	if err != nil {
		t.Fatalf("link pointerful global: %v", err)
	}
	if len(image.GlobalRoots) == 0 {
		t.Fatal("pointerful global has no runtime GC root metadata")
	}
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatalf("load pointerful global: %v", err)
	}
	got, err := code.RunInt()
	if err != nil || got != 73 {
		t.Fatalf("first run = %d, %v; want 73", got, err)
	}
	runtime.GC()
	got, err = code.RunInt()
	if err != nil || got != 73 {
		t.Fatalf("run after GC = %d, %v; want 73", got, err)
	}
	// The type metadata may require a permanent pin; that is separate from
	// keeping pointerful module globals visible to the heap scanner.
	if err := code.Close(); err != nil && !strings.Contains(err.Error(), "cannot safely unload code") {
		t.Fatalf("close pointerful global module: %v", err)
	}
}

func TestCompilerGeneratedLinkerCoverage(t *testing.T) {
	cases := []struct {
		name           string
		source         string
		options        []string
		wantInt        int
		wantIntResult  bool
		wantRelocation ImageRelocationKind
		wantPinned     bool
	}{
		{
			name: "local-call-and-package-global",
			source: `package p
var shared int
var initialized = 9
//go:noinline
func helper(n int) int { return n + 1 }
func Eval() int { shared = helper(4); return shared + initialized - 9 }
`,
			wantInt: 5, wantIntResult: true, wantRelocation: RelocAMD64PLT32,
		},
		{
			name: "global-string-and-slice",
			source: `package p
var shared = []byte("hello")
func Eval() int { return len(shared) }
`,
			wantInt: 5, wantIntResult: true,
		},
		{
			name: "capturing-closure",
			source: `package p
func Eval() int { x := 4; f := func() int { return x }; return f() }
`,
			wantInt: 4, wantIntResult: true,
		},
		{
			name: "interface-method-dispatch",
			source: `package p
type I interface { M() int }
type T int
func (t T) M() int { return int(t) }
func Eval() int { var x I = T(4); return x.M() }
`,
			wantInt: 4, wantIntResult: true, wantRelocation: RelocAMD64Addroff32, wantPinned: true,
		},
		{
			name: "generic-instantiation",
			source: `package p
func identity[T any](x T) T { return x }
func Eval() int { return identity(4) }
`,
			options: []string{"-l=1"}, wantInt: 4, wantIntResult: true, wantRelocation: RelocAMD64PLT32,
		},
		{
			name: "map-runtime-calls-and-types",
			source: `package p
func Eval() int { values := map[string]int{"a": 2}; return values["a"] }
`,
			wantInt: 2, wantIntResult: true, wantRelocation: RelocAMD64Addroff32, wantPinned: true,
		},
		{
			name: "map-insert-delete-and-clear",
			source: `package p
func Eval() int { values := make(map[int]int); values[3] = 12; delete(values, 3); values[4] = 19; clear(values); values[5] = 7; return values[5] }
`,
			wantInt: 7, wantIntResult: true, wantPinned: true,
		},
		{
			name: "map-iteration",
			source: `package p
func Eval() int { values := map[int]int{1: 2, 3: 4}; sum := 0; for key, value := range values { sum += key + value }; return sum }
`,
			wantInt: 10, wantIntResult: true, wantPinned: true,
		},
		{
			name: "pointerful-map-write-barrier",
			source: `package p
type node struct { value int }
func Eval() int { values := make(map[int]*node); values[1] = &node{value: 23}; return values[1].value }
`,
			wantInt: 23, wantIntResult: true, wantPinned: true,
		},
		{
			name: "interface-type-assertion",
			source: `package p
func Eval() int { var value any = 4; return value.(int) }
`,
			wantInt: 4, wantIntResult: true,
		},
		{
			name: "interface-type-switch",
			source: `package p
func Eval() int { var value any = "jit"; switch item := value.(type) { case string: return len(item); case int: return item; default: return 0 } }
`,
			wantInt: 3, wantIntResult: true,
		},
		{
			name: "method-value-and-function-value",
			source: `package p
type value int
func (v value) Add(n int) int { return int(v) + n }
func Eval() int { method := value(10).Add; function := method; return function(15) }
`,
			options: []string{"-l=1"}, wantInt: 25, wantIntResult: true,
		},
		{
			name: "function-value-through-closure",
			source: `package p
func Eval() int { base := 30; add := func(n int) int { return base + n }; call := add; return call(12) }
`,
			wantInt: 42, wantIntResult: true,
		},
		{
			name: "channel-send-and-receive",
			source: `package p
func Eval() int { ch := make(chan int, 1); ch <- 6; return <-ch }
`,
			wantInt: 6, wantIntResult: true, wantPinned: true,
		},
		{
			name: "select-channel-path",
			source: `package p
func Eval() int { ch := make(chan int, 1); ch <- 8; select { case value := <-ch: return value; default: return 0 } }
`,
			wantInt: 8, wantIntResult: true, wantPinned: true,
		},
		{
			name: "goroutine-and-closure",
			source: `package p
func Eval() int { ch := make(chan int, 1); go func() { ch <- 9 }(); return <-ch }
`,
			wantInt: 9, wantIntResult: true, wantPinned: true,
		},
		{
			name: "range-loop-and-slice",
			source: `package p
func Eval() int { sum := 0; for _, value := range []int{1, 2, 3} { sum += value }; return sum }
`,
			wantInt: 6, wantIntResult: true,
		},
		{
			name: "dynamic-string-concatenation",
			source: `package p
func Eval() int { left, right := "jit", "link"; return len(left + right) }
`,
			wantInt: 7, wantIntResult: true,
		},
		{
			name: "slice-growth-and-append",
			source: `package p
func Eval() int { values := make([]int, 1, 1); values[0] = 3; values = append(values, 5); return values[0] + values[1] }
`,
			wantInt: 8, wantIntResult: true,
		},
		{
			name: "slice-copy",
			source: `package p
func Eval() int { source := []byte{2, 4, 6}; destination := make([]byte, len(source)); copy(destination, source); return int(destination[2]) }
`,
			wantInt: 6, wantIntResult: true,
		},
		{
			name: "heap-allocation-and-pointer-store",
			source: `package p
func Eval() int { value := new(int); *value = 13; return *value }
`,
			wantInt: 13, wantIntResult: true,
		},
		{
			name: "pointerful-slice-write-barrier",
			source: `package p
type node struct { next *node; value int }
func Eval() int { values := make([]*node, 0, 1); values = append(values, &node{value: 17}); return values[0].value }
`,
			wantInt: 17, wantIntResult: true, wantPinned: true,
		},
		{
			name: "string-byte-conversion",
			source: `package p
func Eval() int { value := []byte("jit"); return len(string(value)) }
`,
			wantInt: 3, wantIntResult: true,
		},
		{
			name: "integer-to-rune-string-conversion",
			source: `package p
func Eval() int { return len(string(rune(65))) }
`,
			wantInt: 1, wantIntResult: true,
		},
		{
			name: "defer-runtime-path",
			source: `package p
func helper() {}
func Eval() { defer helper() }
`,
		},
		{
			name: "named-result-and-deferred-call",
			source: `package p
func Eval() (result int) { defer func() { result++ }(); return 40 }
`,
			wantInt: 41, wantIntResult: true,
		},
		{
			name: "recursive-local-helper",
			source: `package p
func Eval() int { var sum func(int) int; sum = func(n int) int { if n <= 1 { return n }; return n + sum(n-1) }; return sum(5) }
`,
			wantInt: 15, wantIntResult: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := Compile(CompileRequest{
				PackagePath: "jit/coverage/" + strings.ReplaceAll(tc.name, "-", "/"),
				Files:       []SourceFile{{Name: "coverage.go", Data: tc.source}},
				Options:     tc.options,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Direct Compile output has no escape analysis or owner capture
			// rewriting. Keep the owner through synchronous calls only. Cases
			// requiring unsupported escape or goroutine ownership remain pinned.
			compiled.ownerAware = !tc.wantPinned && tc.name != "goroutine-and-closure"
			image, err := LinkObjectArchive(compiled.Object, "Eval", nil)
			if err != nil {
				t.Fatalf("link compiler output: %v", err)
			}
			if tc.wantRelocation != 0 {
				found := false
				for _, relocation := range image.Relocations {
					if relocation.Kind == tc.wantRelocation {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("linked image has no relocation of kind %d", tc.wantRelocation)
				}
			}
			if tc.name == "interface-method-dispatch" {
				hasType, hasMethod := false, false
				for _, symbol := range image.Symbols {
					hasType = hasType || symbol.Kind == ImageType
					hasMethod = hasMethod || symbol.Kind == ImageFunction && strings.HasSuffix(symbol.Name, ".T.M")
				}
				if !hasType || !hasMethod {
					t.Fatalf("interface image has type=%v method=%v", hasType, hasMethod)
				}
			}

			code, err := (*Metadata)(nil).LoadSnippet(compiled)
			if err != nil {
				t.Fatalf("map image and register runtime metadata: %v", err)
			}
			if tc.wantIntResult {
				got, err := code.RunInt()
				if err != nil {
					t.Fatalf("run linked code: %v", err)
				}
				if got != tc.wantInt {
					t.Fatalf("linked code returned %d, want %d", got, tc.wantInt)
				}
			} else if err := code.Run(); err != nil {
				t.Fatalf("run linked code: %v", err)
			}
			closeErr := code.Close()
			if tc.wantPinned {
				if closeErr == nil {
					t.Fatal("Close unloaded a module that requires a manual lifetime pin")
				}
				if tc.wantIntResult {
					if _, err := code.RunInt(); err != nil {
						t.Fatalf("module was not retained after unload refusal: %v", err)
					}
				}
			} else if closeErr != nil {
				t.Fatalf("Close failed for unloadable module: %v", closeErr)
			}
		})
	}
}

func TestManualPinRetainsCodeUntilExplicitRelease(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/manualpin",
		Files:       []SourceFile{{Name: "pin.go", Data: "package p\nfunc Eval() {}\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := (*Metadata)(nil).LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	entry := code.image.entry
	pin, err := code.Pin()
	if err != nil {
		t.Fatal(err)
	}
	if address, err := pin.PC(); err != nil || address != entry {
		t.Fatalf("pinned PC = %#x, %v; want %#x", address, err, entry)
	}
	if err := code.Close(); err == nil || !strings.Contains(err.Error(), "Pin handle") {
		t.Fatalf("Close with active manual pin = %v, want a pin error", err)
	}
	code = nil
	runtime.GC()
	if !runtime.JITModuleRegistered(entry) {
		t.Fatal("manual Pin did not retain the module")
	}
	pin.Release()
	if _, err := pin.PC(); err == nil {
		t.Fatal("released Pin still returned a PC")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if !runtime.JITModuleRegistered(entry) {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("module remained registered after explicit pin release")
}

var linkerCoverageDynamicFunction any

//go:noinline
func linkerCoverageSetDynamicFunction(value any) {
	linkerCoverageDynamicFunction = value
}

func TestReflectRawFunctionPointerPinsModule(t *testing.T) {
	linkerCoverageDynamicFunction = nil
	t.Cleanup(func() { linkerCoverageDynamicFunction = nil })
	const packagePath = "jit/coverage/reflectpin"
	metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{{
		PackagePath:  packagePath,
		Name:         "setDynamicFunctionSink",
		Kind:         FunctionLink,
		ABI:          uint16(obj.ABIInternal),
		Address:      reflect.ValueOf(linkerCoverageSetDynamicFunction).Pointer(),
		Type:         reflect.TypeOf(linkerCoverageSetDynamicFunction),
		FromManifest: true,
	}}}}
	compiled, err := Compile(CompileRequest{
		PackagePath: packagePath,
		Files: []SourceFile{{Name: "pin.go", Data: `package p
func setDynamicFunctionSink(any)
func Eval() { setDynamicFunctionSink(func() {}) }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := metadata.LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if err := code.Run(); err != nil {
		t.Fatal(err)
	}
	entry := code.image.entry
	function, ok := linkerCoverageDynamicFunction.(func())
	if !ok {
		t.Fatalf("JIT function value has type %T", linkerCoverageDynamicFunction)
	}
	pc := reflect.ValueOf(function).Pointer()
	if pc == 0 || !runtime.JITModuleRegistered(pc) {
		t.Fatalf("reflect.Value.Pointer returned unregistered JIT PC %#x", pc)
	}
	linkerCoverageDynamicFunction = nil
	function = nil
	if err := code.Close(); err != nil {
		t.Fatal(err)
	}
	code = nil
	runtime.GC()
	if !runtime.JITModuleRegistered(entry) {
		t.Fatal("reflect.Value.Pointer did not pin the raw JIT function address")
	}
}

func TestReflectInterfaceDataPinsDynamicTypeModule(t *testing.T) {
	linkerCoverageDynamicInterface = nil
	t.Cleanup(func() { linkerCoverageDynamicInterface = nil })
	const packagePath = "jit/coverage/interface-data-pin"
	metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{{
		PackagePath: packagePath, Name: "dynamicInterfaceSink", Kind: VariableLink,
		Address: uintptr(unsafe.Pointer(&linkerCoverageDynamicInterface)),
		Type:    reflect.TypeOf((*any)(nil)).Elem(), FromManifest: true,
	}}}}
	compiled, err := Compile(CompileRequest{
		PackagePath: packagePath,
		Files: []SourceFile{{Name: "interface_data.go", Data: `package p
var dynamicInterfaceSink any
type dynamicValue struct { Field int }
func Eval() { dynamicInterfaceSink = dynamicValue{Field: 17} }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled.ownerAware = true
	code, err := metadata.LoadSnippet(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if err := code.Run(); err != nil {
		t.Fatalf("run interface-data fixture: %v", err)
	}
	entry := code.image.entry
	interfaceValue := reflect.ValueOf(&linkerCoverageDynamicInterface).Elem()
	if interfaceValue.Kind() != reflect.Interface {
		t.Fatalf("interface storage has reflection kind %v", interfaceValue.Kind())
	}
	words := interfaceValue.InterfaceData()
	if words[0] == 0 {
		t.Fatal("InterfaceData returned a nil dynamic type word")
	}
	interfaceValue = reflect.Value{}
	linkerCoverageDynamicInterface = nil
	if err := code.Close(); err != nil {
		t.Fatal(err)
	}
	code = nil
	runtime.GC()
	if !runtime.JITWaitForRetirements(5e9) {
		t.Fatal("timed out waiting for InterfaceData owner decision")
	}
	if !runtime.JITModuleRegistered(entry) {
		t.Fatal("reflect.Value.InterfaceData exposed a dynamic type without pinning its module")
	}
}

func TestDynamicJITTypeAssertionUsesOwnerScopedItab(t *testing.T) {
	const packagePath = "jit/coverage/dynamicitab"
	linkerCoverageDynamicInterface = nil
	linkerCoverageDynamicStruct = nil
	t.Cleanup(func() {
		linkerCoverageDynamicInterface = nil
		linkerCoverageDynamicStruct = nil
	})
	interfaceType := reflect.TypeOf((*any)(nil)).Elem()
	methodSignatureType := reflect.TypeOf((func() int)(nil))
	methodSignatureAddress := reflect.ValueOf(methodSignatureType).Pointer()
	metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{
		{
			PackagePath:  packagePath,
			Name:         "dynamicInterfaceSink",
			Kind:         VariableLink,
			ABI:          0,
			Address:      uintptr(unsafe.Pointer(&linkerCoverageDynamicInterface)),
			Type:         interfaceType,
			FromManifest: true,
		},
		{
			PackagePath:  packagePath,
			Name:         "dynamicStructSink",
			Kind:         VariableLink,
			ABI:          0,
			Address:      uintptr(unsafe.Pointer(&linkerCoverageDynamicStruct)),
			Type:         interfaceType,
			FromManifest: true,
		},
		{
			PackagePath:  "runtime",
			Name:         "func() int",
			Kind:         TypeLink,
			ABI:          0,
			Address:      methodSignatureAddress,
			TypeAddress:  methodSignatureAddress,
			Type:         methodSignatureType,
			FromManifest: true,
		},
	}}}
	compiled, err := Compile(CompileRequest{
		PackagePath: packagePath,
		Files: []SourceFile{{Name: "dynamic.go", Data: `package p
var dynamicInterfaceSink any
var dynamicStructSink any
type dynamicValue int
func (v dynamicValue) Value() int { return int(v) }
type dynamicStruct struct { Value int ` + "`jit:\"kept\"`" + ` }
func Eval() {
	dynamicInterfaceSink = dynamicValue(37)
	dynamicStructSink = dynamicStruct{}
}
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Compile creates the ordinary standalone object. This fixture exercises
	// the runtime's GC-traced owner edges for a dynamic type and its itab.
	compiled.ownerAware = true
	code, err := metadata.LoadSnippet(compiled)
	if err != nil {
		t.Fatalf("load dynamic type module: %v", err)
	}
	entry := code.image.entry
	if err := code.Run(); err != nil {
		t.Fatalf("run dynamic type module: %v", err)
	}
	var detachedMetadata []string
	var wantDynamicTypeString string
	if err := code.Close(); err != nil {
		t.Fatalf("Close while a dynamic interface value retains its owner: %v", err)
	}
	runtime.GC()
	retainedTypes := func() []reflect.Type {
		dynamicType := reflect.TypeOf(linkerCoverageDynamicInterface)
		if got := dynamicType.NumMethod(); got != 1 {
			t.Fatalf("dynamic reflect type has %d methods, want Value; type=%v", got, dynamicType)
		}
		if dynamicType.Name() != "dynamicValue" {
			t.Fatalf("dynamic reflect type name = %q, want dynamicValue", dynamicType.Name())
		}
		method, ok := dynamicType.MethodByName("Value")
		if !ok {
			t.Fatal("reflect.Type.MethodByName did not find the JIT method")
		}
		dynamicStructType := reflect.TypeOf(linkerCoverageDynamicStruct)
		field, ok := dynamicStructType.FieldByName("Value")
		if !ok {
			t.Fatal("reflect.Type.FieldByName did not find the JIT struct field")
		}
		detachedMetadata = []string{
			dynamicType.Name(), dynamicType.String(), method.Name,
			field.Name, string(field.Tag), field.Tag.Get("jit"),
		}
		wantDynamicTypeString = strings.Clone(detachedMetadata[1])
		result := method.Func.Call([]reflect.Value{reflect.ValueOf(linkerCoverageDynamicInterface)})
		if len(result) != 1 || result[0].Int() != 37 {
			t.Fatalf("reflective JIT method call returned %v, want 37", result)
		}
		target, ok := linkerCoverageDynamicInterface.(interface{ Value() int })
		if !ok {
			t.Fatalf("dynamic interface value has type %T, want an implementation of Value (method type %v)", linkerCoverageDynamicInterface, method.Type)
		}
		for i := 0; i < 4; i++ {
			if got := target.Value(); got != 37 {
				t.Fatalf("dynamic method dispatch %d returned %d, want 37", i, got)
			}
		}
		compositeTypes := []reflect.Type{
			reflect.PointerTo(dynamicType),
			reflect.SliceOf(dynamicType),
			reflect.ArrayOf(2, dynamicType),
			reflect.MapOf(reflect.TypeOf(0), dynamicType),
			reflect.ChanOf(reflect.BothDir, dynamicType),
			reflect.FuncOf([]reflect.Type{dynamicType}, []reflect.Type{dynamicType}, false),
			reflect.StructOf([]reflect.StructField{{Name: "Value", Type: dynamicType}}),
		}
		linkerCoverageDynamicInterface = nil
		linkerCoverageDynamicStruct = nil
		target = nil
		runtime.GC()
		if !runtime.JITModuleRegistered(entry) {
			t.Fatal("reflect.Type did not retain its JIT type descriptor after the interface value was released")
		}
		if got := dynamicType.Name(); got != "dynamicValue" {
			t.Fatalf("reflect.Type became invalid after GC: Name() = %q", got)
		}
		return append([]reflect.Type{dynamicType}, compositeTypes...)
	}()
	if len(retainedTypes) != 8 || retainedTypes[0].Name() != "dynamicValue" {
		t.Fatalf("dynamic and composite reflection types = %v", retainedTypes)
	}
	for _, typ := range retainedTypes {
		if typ.String() == "" {
			t.Fatal("JIT-dependent composite type has an empty string representation")
		}
	}
	code = nil
	retainedTypes = nil
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if !runtime.JITModuleRegistered(entry) {
			want := []string{"dynamicValue", wantDynamicTypeString, "Value", "Value", `jit:"kept"`, "kept"}
			if len(detachedMetadata) != len(want) {
				t.Fatalf("detached reflect metadata length = %d, want %d", len(detachedMetadata), len(want))
			}
			for i := range want {
				if detachedMetadata[i] != want[i] {
					t.Fatalf("detached reflect metadata[%d] = %q, want %q", i, detachedMetadata[i], want[i])
				}
			}
			return
		}
		runtime.Gosched()
	}
	t.Fatal("owner-local itab cache retained the dynamic type module after its interface value was released")
}

func TestCrossModuleItabCacheDoesNotRetainInterfaceModule(t *testing.T) {
	const concretePackage = "jit/coverage/crossitab/concrete"
	const interfacePackage = "jit/coverage/crossitab/interface"
	linkerCoverageCrossModuleValue = nil
	t.Cleanup(func() { linkerCoverageCrossModuleValue = nil })
	methodSignatureType := reflect.TypeOf((func() int)(nil))
	methodSignatureAddress := reflect.ValueOf(methodSignatureType).Pointer()
	metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{
		{
			PackagePath:  concretePackage,
			Name:         "crossModuleValueSink",
			Kind:         VariableLink,
			Address:      uintptr(unsafe.Pointer(&linkerCoverageCrossModuleValue)),
			Type:         reflect.TypeOf((*any)(nil)).Elem(),
			FromManifest: true,
		},
		{
			PackagePath:  "runtime",
			Name:         "func() int",
			Kind:         TypeLink,
			Address:      methodSignatureAddress,
			TypeAddress:  methodSignatureAddress,
			Type:         methodSignatureType,
			FromManifest: true,
		},
	}}}
	concrete, err := Compile(CompileRequest{
		PackagePath: concretePackage,
		Files: []SourceFile{{Name: "concrete.go", Data: `package p
var crossModuleValueSink any
type crossConcrete int
func (v crossConcrete) Value() int { return int(v) }
func Eval() { crossModuleValueSink = crossConcrete(37) }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	concrete.ownerAware = true
	concreteCode, err := metadata.LoadSnippet(concrete)
	if err != nil {
		t.Fatalf("load concrete module: %v", err)
	}
	concreteEntry := concreteCode.image.entry
	if err := concreteCode.Run(); err != nil {
		t.Fatalf("run concrete module: %v", err)
	}
	if _, ok := linkerCoverageCrossModuleValue.(interface{ Value() int }); !ok {
		t.Fatalf("concrete module stored %T, which does not implement Value", linkerCoverageCrossModuleValue)
	}
	if err := concreteCode.Close(); err != nil {
		t.Fatalf("close concrete module: %v", err)
	}
	concreteCode = nil

	iface, err := Compile(CompileRequest{
		PackagePath: interfacePackage,
		Files: []SourceFile{{Name: "interface.go", Data: `package p
type crossInterface interface { Value() int }
func Eval() { var value any = 0; _, _ = value.(crossInterface) }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	linkedInterface, err := LinkObjectArchive(iface.Object, "Eval", metadata)
	if err != nil {
		t.Fatalf("link interface module: %v", err)
	}
	interfaceSymbol := -1
	for i, symbol := range linkedInterface.Symbols {
		if symbol.Kind == ImageType && strings.Contains(symbol.Name, "crossInterface") {
			interfaceSymbol = i
			break
		}
	}
	if interfaceSymbol < 0 {
		t.Fatal("linked interface image has no crossInterface type descriptor")
	}
	iface.ownerAware = true
	interfaceCode, err := metadata.LoadSnippet(iface)
	if err != nil {
		t.Fatalf("load interface module: %v", err)
	}
	interfaceEntry := interfaceCode.image.entry
	if err := interfaceCode.Run(); err != nil {
		t.Fatalf("run interface module: %v", err)
	}
	interfaceAddress := interfaceCode.image.symbols[interfaceSymbol]
	// The runtime representation of reflect.Type is an interface containing
	// *rtype. Reuse that itab with the interface descriptor loaded above.
	interfaceType := reflect.TypeOf(0)
	typeWords := *(*[2]unsafe.Pointer)(unsafe.Pointer(&interfaceType))
	typeWords[1] = unsafe.Pointer(interfaceAddress)
	interfaceType = *(*reflect.Type)(unsafe.Pointer(&typeWords))
	if interfaceType.Kind() != reflect.Interface || interfaceType.NumMethod() != 1 {
		t.Fatalf("mapped interface descriptor has unexpected reflection shape %v", interfaceType)
	}
	concreteType := reflect.TypeOf(linkerCoverageCrossModuleValue)
	concreteValue := reflect.ValueOf(linkerCoverageCrossModuleValue)
	if !concreteType.Implements(interfaceType) {
		t.Fatalf("dynamic concrete type %v does not implement %v", concreteType, interfaceType)
	}
	converted := concreteValue.Convert(interfaceType).Interface()
	if got := reflect.ValueOf(converted).MethodByName("Value").Call(nil)[0].Int(); got != 37 {
		t.Fatalf("cross-module interface method returned %d, want 37", got)
	}
	runtime.KeepAlive(interfaceType)
	converted = nil
	interfaceType = nil
	typeWords[1] = nil
	if err := interfaceCode.Close(); err != nil {
		t.Fatalf("close interface module: %v", err)
	}
	interfaceCode = nil
	retainedConcreteValue := linkerCoverageCrossModuleValue
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.KeepAlive(retainedConcreteValue)
		if !runtime.JITModuleRegistered(interfaceEntry) {
			if !runtime.JITModuleRegistered(concreteEntry) {
				t.Fatalf("concrete module %x retired while its package interface value remains live (interface entry %x)", concreteEntry, interfaceEntry)
			}
			linkerCoverageCrossModuleValue = nil
			retainedConcreteValue = nil
			for releaseDeadline := time.Now().Add(5 * time.Second); time.Now().Before(releaseDeadline); {
				runtime.GC()
				if !runtime.JITModuleRegistered(concreteEntry) {
					return
				}
				runtime.Gosched()
			}
			t.Fatal("concrete module remained registered after its final interface value was dropped")
		}
		runtime.Gosched()
	}
	t.Fatal("cross-module itab cache retained the interface module after its interface value was released")
}

func TestReflectCreatedStructuralTypeCacheIsSharedAcrossModules(t *testing.T) {
	linkerCoverageDynamicInterface = nil
	t.Cleanup(func() { linkerCoverageDynamicInterface = nil })
	makeModule := func(packagePath string) (*Code, uintptr, reflect.Type) {
		metadata := &Metadata{Link: LinkMetadata{Symbols: []LinkedSymbol{{
			PackagePath:  packagePath,
			Name:         "dynamicInterfaceSink",
			Kind:         VariableLink,
			Address:      uintptr(unsafe.Pointer(&linkerCoverageDynamicInterface)),
			Type:         reflect.TypeOf((*any)(nil)).Elem(),
			FromManifest: true,
		}}}}
		compiled, err := Compile(CompileRequest{
			PackagePath: packagePath,
			Files: []SourceFile{{Name: "anonymous.go", Data: `package p
var dynamicInterfaceSink any
func Eval() { dynamicInterfaceSink = struct { Value int }{Value: 17} }
`}},
		})
		if err != nil {
			t.Fatalf("compile %s: %v", packagePath, err)
		}
		compiled.ownerAware = true
		code, err := metadata.LoadSnippet(compiled)
		if err != nil {
			t.Fatalf("load %s: %v", packagePath, err)
		}
		if err := code.Run(); err != nil {
			t.Fatalf("run %s: %v", packagePath, err)
		}
		return code, code.image.entry, reflect.TypeOf(linkerCoverageDynamicInterface)
	}

	firstCode, firstEntry, firstType := makeModule("jit/coverage/anoncache/first")
	linkerCoverageDynamicInterface = nil
	if err := firstCode.Close(); err != nil {
		t.Fatalf("close first module: %v", err)
	}
	firstCode = nil

	secondCode, secondEntry, secondType := makeModule("jit/coverage/anoncache/second")
	firstPointer := reflect.PointerTo(firstType)
	secondPointer := reflect.PointerTo(secondType)
	if firstPointer != secondPointer {
		t.Fatalf("reflect.PointerTo did not canonicalize equivalent dynamic types: %p != %p", firstPointer, secondPointer)
	}
	linkerCoverageDynamicInterface = nil
	if err := secondCode.Close(); err != nil {
		t.Fatalf("close second module: %v", err)
	}
	secondCode = nil
	secondType = nil
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if !runtime.JITModuleRegistered(secondEntry) {
			if !runtime.JITModuleRegistered(firstEntry) {
				t.Fatal("canonical reflect type did not retain its source module")
			}
			runtime.KeepAlive(firstPointer)
			return
		}
		runtime.Gosched()
	}
	runtime.KeepAlive(firstPointer)
	t.Fatal("discarded structural type candidate retained its second JIT module")
}

func TestObjectLinkerBuildsGOTSlots(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/got",
		Files: []SourceFile{{Name: "got.go", Data: `package p
//go:noinline
func helper(n int) int { return n + 1 }
func Eval() int { return helper(4) }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := archive.ParseBytes(compiled.Object, false)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, entry := range parsed.Entries {
		if entry.Type != archive.EntryGoObj {
			continue
		}
		data, err := parsed.ObjectBytes(entry)
		if err != nil {
			t.Fatal(err)
		}
		reader := goobj.NewReaderFromBytes(data, true)
		if reader == nil {
			t.Fatalf("invalid object member %q", entry.Name)
		}
		for symbol := 0; symbol < reader.NSym() && !changed; symbol++ {
			for relocation := 0; relocation < reader.NReloc(uint32(symbol)); relocation++ {
				reloc := reader.Reloc(uint32(symbol), relocation)
				if reloc.Type() != uint16(objabi.R_CALL) || reloc.Siz() != 4 {
					continue
				}
				reloc.SetType(uint16(objabi.R_GOTPCREL))
				changed = true
				break
			}
		}
	}
	if !changed {
		t.Fatal("compiler output had no four-byte call relocation to adapt into a GOT fixture")
	}

	image, err := LinkObjectArchive(compiled.Object, "Eval", nil)
	if err != nil {
		t.Fatalf("link GOT fixture: %v", err)
	}
	encoded, err := MarshalModuleImage(image)
	if err != nil {
		t.Fatalf("serialize GOT image: %v", err)
	}
	image, err = UnmarshalModuleImage(encoded)
	if err != nil {
		t.Fatalf("deserialize GOT image: %v", err)
	}
	readOnly := -1
	for i, section := range image.Sections {
		if section.Kind == ImageReadOnly {
			readOnly = i
			break
		}
	}
	if readOnly < 0 {
		t.Fatal("GOT fixture has no read-only section")
	}
	gotReloc, slotReloc := false, false
	for _, relocation := range image.Relocations {
		gotReloc = gotReloc || relocation.Kind == RelocAMD64GOTPCREL
		if int(relocation.Section) == readOnly && relocation.Kind == RelocAMD64Abs64 {
			slotReloc = true
		}
	}
	if !gotReloc || !slotReloc {
		t.Fatalf("GOT link output has displacement=%v pointer-slot=%v relocations", gotReloc, slotReloc)
	}
}

func TestObjectLinkerKeepsWeakTargetsOptional(t *testing.T) {
	compiled, err := Compile(CompileRequest{
		PackagePath: "jit/coverage/weak",
		Files: []SourceFile{{Name: "weak.go", Data: `package p
func onlyHere() {}
var callback = onlyHere
func Eval() { callback() }
`}},
	})
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := archive.ParseBytes(compiled.Object, false)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, entry := range parsed.Entries {
		if entry.Type != archive.EntryGoObj {
			continue
		}
		data, err := parsed.ObjectBytes(entry)
		if err != nil {
			t.Fatal(err)
		}
		reader := goobj.NewReaderFromBytes(data, true)
		if reader == nil {
			t.Fatalf("invalid object member %q", entry.Name)
		}
		object := GoObject{Name: entry.Name, Reader: reader}
		for symbol := 0; symbol < reader.NSym() && !changed; symbol++ {
			for relocation := 0; relocation < reader.NReloc(uint32(symbol)); relocation++ {
				reloc := reader.Reloc(uint32(symbol), relocation)
				if reloc.Type() != uint16(objabi.R_ADDR) || reloc.Siz() != 8 {
					continue
				}
				ref, err := object.ResolveReference(reloc.Sym())
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasSuffix(ref.Name, ".onlyHere·f") {
					continue
				}
				weakAddrType := int16(objabi.R_WEAKADDR)
				reloc.SetType(uint16(weakAddrType))
				changed = true
				break
			}
		}
	}
	if !changed {
		t.Fatal("compiler output had no function-value relocation for onlyHere")
	}

	image, err := LinkObjectArchive(compiled.Object, "Eval", nil)
	if err != nil {
		t.Fatalf("link weak-reference fixture: %v", err)
	}
	for _, symbol := range image.Symbols {
		if strings.HasSuffix(symbol.Name, ".onlyHere") {
			t.Fatalf("weak function target was incorrectly made reachable: %+v", image.Symbols)
		}
	}
	foundFallback := false
	for _, symbol := range image.Imports {
		if symbol.PackagePath == "runtime" && symbol.Name == "unreachableMethod" && symbol.Kind == ImageFunction {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Fatal("unresolved weak function did not target runtime.unreachableMethod")
	}
}
