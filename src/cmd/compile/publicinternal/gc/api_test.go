// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc_test

import (
	"bytes"
	"cmd/compile/publicinternal/amd64"
	armcompile "cmd/compile/publicinternal/arm"
	arm64compile "cmd/compile/publicinternal/arm64"
	"cmd/compile/publicinternal/base"
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
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
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

func TestCompileUsesIndependentConcurrentRequestContexts(t *testing.T) {
	targets := []struct {
		name string
		init func(*ssagen.ArchInfo)
		code string
	}{
		{"amd64", amd64.Init, "package p\nfunc F(m map[string]int) (n int) { defer func() { n++ }(); return m[\"answer\"] }\n"},
		{"arm", armcompile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"arm64", arm64compile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"loong64", loong64compile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"mips", mipscompile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"mips64", mips64compile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"ppc64", ppc64compile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"riscv64", riscv64compile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"s390x", s390xcompile.Init, "package p\nfunc F() int { return 42 }\n"},
		{"wasm", wasmcompile.Init, "package p\nfunc F() int { return 42 }\n"},
	}
	const requestsPerTarget = 2
	requests := len(targets) * requestsPerTarget
	oldArgs := append([]string(nil), os.Args...)
	oldStdout, oldStderr := os.Stdout, os.Stderr
	oldFlagSet := flag.CommandLine
	oldUsage := flag.Usage
	oldLogWriter := log.Writer()
	oldCtxt, oldFlags, oldDebug, oldPos := base.Ctxt, base.Flag, base.Debug, base.Pos

	var wg sync.WaitGroup
	errCh := make(chan error, requests)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := targets[i/requestsPerTarget]
			result, err := gc.Compile(gc.Request{
				ArchInit:    target.init,
				PackagePath: fmt.Sprintf("example/concurrent/%s%d", target.name, i),
				Files: []noder.SourceFile{{
					Name: "input.go",
					Data: target.code,
				}},
			})
			if err == nil && len(result.Object) == 0 {
				err = fmt.Errorf("request %d returned an empty object", i)
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
	if oldCtxt != base.Ctxt || !reflect.DeepEqual(oldFlags, base.Flag) || !reflect.DeepEqual(oldDebug, base.Debug) || oldPos != base.Pos {
		t.Errorf("Compile changed legacy base compiler state")
	}
}
