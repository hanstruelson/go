// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc_test

import (
	"bytes"
	"cmd/compile/publicinternal/amd64"
	armcompile "cmd/compile/publicinternal/arm"
	arm64compile "cmd/compile/publicinternal/arm64"
	"cmd/compile/publicinternal/gc"
	loong64compile "cmd/compile/publicinternal/loong64"
	mipscompile "cmd/compile/publicinternal/mips"
	mips64compile "cmd/compile/publicinternal/mips64"
	"cmd/compile/publicinternal/noder"
	ppc64compile "cmd/compile/publicinternal/ppc64"
	riscv64compile "cmd/compile/publicinternal/riscv64"
	s390xcompile "cmd/compile/publicinternal/s390x"
	"cmd/compile/publicinternal/ssagen"
	"cmd/compile/publicinternal/syntax"
	wasmcompile "cmd/compile/publicinternal/wasm"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestPublicInternalParserParsesAndPrintsGo(t *testing.T) {
	const source = "package p\nfunc F() int { return 42 }\n"
	file, err := syntax.Parse(syntax.NewFileBase("input.go"), strings.NewReader(source), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var printed bytes.Buffer
	if _, err := syntax.Fprint(&printed, file, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed.String(), "func F() int") || !strings.Contains(printed.String(), "return 42") {
		t.Fatalf("printed syntax tree does not contain source declarations:\n%s", printed.String())
	}
}

func TestCompileContainsArchitectureInitializationFailures(t *testing.T) {
	request := func(init func(*ssagen.ArchInfo)) gc.Request {
		return gc.Request{
			ArchInit:    init,
			GOARCH:      "amd64",
			PackagePath: "example/initfailure",
			Files:       []noder.SourceFile{{Name: "input.go", Data: "package p\nfunc F() int { return 42 }\n"}},
		}
	}

	_, err := gc.Compile(request(func(arch *ssagen.ArchInfo) {
		arch.InitError = fmt.Errorf("unsupported test architecture setting")
	}))
	var compileErr *gc.CompileError
	if !errors.As(err, &compileErr) {
		t.Fatalf("Compile error = %v, want *gc.CompileError", err)
	}
	if compileErr.Status != 1 || !strings.Contains(compileErr.Diagnostics, "unsupported test architecture setting") {
		t.Fatalf("Compile error = %#v, want status 1 and initialization diagnostic", compileErr)
	}

	_, err = gc.Compile(request(func(*ssagen.ArchInfo) {
		panic("test architecture initializer panic")
	}))
	if !errors.As(err, &compileErr) {
		t.Fatalf("Compile error = %v, want *gc.CompileError", err)
	}
	if compileErr.Status != 2 || !strings.Contains(compileErr.Diagnostics, "test architecture initializer panic") {
		t.Fatalf("Compile error = %#v, want recovered initialization panic", compileErr)
	}

	result, err := gc.Compile(request(amd64.Init))
	if err != nil {
		t.Fatalf("Compile after initialization failures: %v", err)
	}
	if len(result.Object) == 0 {
		t.Fatal("Compile after initialization failures returned an empty object")
	}
}

func TestCompileIgnoresProcessSSAEnvironment(t *testing.T) {
	dumpDir := t.TempDir()
	t.Setenv("GOSSAFUNC", "F")
	t.Setenv("GOSSADIR", dumpDir)

	_, err := gc.Compile(gc.Request{
		ArchInit:    amd64.Init,
		GOARCH:      "amd64",
		PackagePath: "example/no-ssadump",
		Files:       []noder.SourceFile{{Name: "input.go", Data: "package p\nfunc F() int { return 42 }\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("reusable compilation wrote SSA debug output from process environment: %v", entries)
	}
}

func TestCompileReturnsMalformedHashDebugOptionError(t *testing.T) {
	request := gc.Request{
		ArchInit:    amd64.Init,
		GOARCH:      "amd64",
		PackagePath: "example/bad-hash-debug",
		Files:       []noder.SourceFile{{Name: "input.go", Data: "package p\nfunc F() int { return 42 }\n"}},
		Options:     []string{"-d=gossahash=not-a-valid-selector"},
	}
	_, err := gc.Compile(request)
	var compileErr *gc.CompileError
	if !errors.As(err, &compileErr) {
		t.Fatalf("Compile error = %v, want *CompileError", err)
	}
	if !strings.Contains(compileErr.Diagnostics, "gossahash: invalid pattern syntax") {
		t.Fatalf("Compile diagnostics = %q, want malformed gossahash selector", compileErr.Diagnostics)
	}

	request.Options = nil
	if result, err := gc.Compile(request); err != nil {
		t.Fatalf("Compile after malformed option: %v", err)
	} else if len(result.Object) == 0 {
		t.Fatal("Compile after malformed option returned an empty object")
	}
}

func TestCompileRejectsProcessLevelSSADebugOutput(t *testing.T) {
	_, err := gc.Compile(gc.Request{
		ArchInit:    amd64.Init,
		GOARCH:      "amd64",
		PackagePath: "example/ssa-debug-output",
		Files:       []noder.SourceFile{{Name: "input.go", Data: "package p\nfunc F() int { return 42 }\n"}},
		Options:     []string{"-d=ssa/regalloc/debug=1"},
	})
	if err == nil || !strings.Contains(err.Error(), "process-level SSA debug output") {
		t.Fatalf("Compile error = %v, want process-level SSA output rejection", err)
	}
}

func TestCompileReturnsTypeErrorsAndCanContinue(t *testing.T) {
	request := gc.Request{
		ArchInit:    amd64.Init,
		GOARCH:      "amd64",
		PackagePath: "example/type-error",
		Files:       []noder.SourceFile{{Name: "input.go", Data: "package p\nfunc F() int { return \"wrong\" }\n"}},
	}
	_, err := gc.Compile(request)
	var compileErr *gc.CompileError
	if !errors.As(err, &compileErr) {
		t.Fatalf("Compile error = %v, want *CompileError", err)
	}
	if compileErr.Status != 2 || !strings.Contains(compileErr.Diagnostics, "cannot use") {
		t.Fatalf("Compile error = %#v, want a returned type error", compileErr)
	}

	request.PackagePath = "example/type-error-recovery"
	request.Files[0].Data = "package p\nfunc F() int { return 42 }\n"
	if result, err := gc.Compile(request); err != nil {
		t.Fatalf("Compile after type error: %v", err)
	} else if len(result.Object) == 0 {
		t.Fatal("Compile after type error returned an empty object")
	}
}

func TestCompileCapturesInlineDiagnostics(t *testing.T) {
	result, err := gc.Compile(gc.Request{
		ArchInit:    amd64.Init,
		GOARCH:      "amd64",
		PackagePath: "example/inline-diagnostics",
		Files:       []noder.SourceFile{{Name: "input.go", Data: "package p\nfunc F() int { return 42 }\n"}},
		Options:     []string{"-m=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Diagnostics, "can inline F") {
		t.Fatalf("Compile diagnostics = %q, want inline optimization report", result.Diagnostics)
	}
}

func TestCompileUsesIndependentConcurrentRequestContexts(t *testing.T) {
	targets := []struct {
		name    string
		goarch  string
		ptrSize int
		init    func(*ssagen.ArchInfo)
		code    string
	}{
		{name: "amd64", goarch: "amd64", ptrSize: 8, init: amd64.Init, code: "func F(m map[string]int) (n int) { defer func() { n++ }(); return m[\"answer\"] }\n"},
		{name: "arm", goarch: "arm", ptrSize: 4, init: armcompile.Init, code: "func F() int { return 42 }\n"},
		{name: "arm64", goarch: "arm64", ptrSize: 8, init: arm64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "loong64", goarch: "loong64", ptrSize: 8, init: loong64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "mips", goarch: "mips", ptrSize: 4, init: mipscompile.Init, code: "func F() int { return 42 }\n"},
		{name: "mipsle", goarch: "mipsle", ptrSize: 4, init: mipscompile.Init, code: "func F() int { return 42 }\n"},
		{name: "mips64", goarch: "mips64", ptrSize: 8, init: mips64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "mips64le", goarch: "mips64le", ptrSize: 8, init: mips64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "ppc64", goarch: "ppc64", ptrSize: 8, init: ppc64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "ppc64le", goarch: "ppc64le", ptrSize: 8, init: ppc64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "riscv64", goarch: "riscv64", ptrSize: 8, init: riscv64compile.Init, code: "func F() int { return 42 }\n"},
		{name: "s390x", goarch: "s390x", ptrSize: 8, init: s390xcompile.Init, code: "func F() int { return 42 }\n"},
		{name: "wasm", goarch: "wasm", ptrSize: 8, init: wasmcompile.Init, code: "//go:wasmimport env imported\nfunc imported()\nfunc F() { imported() }\n"},
	}
	const requestsPerTarget = 2
	requests := len(targets) * requestsPerTarget
	oldArgs := append([]string(nil), os.Args...)
	oldStdout, oldStderr := os.Stdout, os.Stderr
	oldFlagSet := flag.CommandLine
	oldUsage := flag.Usage
	oldLogWriter := log.Writer()

	var wg sync.WaitGroup
	errCh := make(chan error, requests)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := targets[i/requestsPerTarget]
			result, err := gc.Compile(gc.Request{
				ArchInit:    target.init,
				GOARCH:      target.goarch,
				PackagePath: fmt.Sprintf("example/concurrent/%s%d", target.name, i),
				Files: []noder.SourceFile{{
					Name: "input.go",
					Data: fmt.Sprintf("package p\nimport \"unsafe\"\nconst packageConstant = 42\ntype packageLayout struct { a int; b uintptr }\nvar packageValue = packageLayout{a: packageConstant, b: uintptr(packageConstant)}\nvar _ [%d-unsafe.Sizeof(uintptr(0))]byte\nvar _ [%d-unsafe.Sizeof([]byte{})]byte\nvar _ [%d-unsafe.Sizeof(string(\"\"))]byte\n", target.ptrSize, target.ptrSize*3, target.ptrSize*2) + target.code,
				}},
			})
			if err == nil && len(result.Object) == 0 {
				err = fmt.Errorf("request %d returned an empty object", i)
			}
			if err == nil && result.Diagnostics != "" {
				err = fmt.Errorf("request %d read process-global diagnostic flags: %s", i, result.Diagnostics)
			}
			if err == nil && !bytes.Contains(result.Object, []byte("go object "+runtime.GOOS+" "+target.goarch+" ")) {
				err = fmt.Errorf("request %d (%s) emitted an object header for the wrong target", i, target.name)
			}
			if err != nil {
				errCh <- fmt.Errorf("request %d (%s): %w", i, target.name, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if !reflect.DeepEqual(oldArgs, os.Args) {
		t.Errorf("Compile changed os.Args")
	}
	if oldStdout != os.Stdout || oldStderr != os.Stderr {
		t.Errorf("Compile changed process standard streams")
	}
	if oldFlagSet != flag.CommandLine || reflect.ValueOf(oldUsage).Pointer() != reflect.ValueOf(flag.Usage).Pointer() {
		t.Errorf("Compile changed process flag state")
	}
	if oldLogWriter != log.Writer() {
		t.Errorf("Compile changed process log output")
	}
}
