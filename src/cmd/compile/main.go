// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"cmd/compile/publicinternal/amd64"
	"cmd/compile/publicinternal/arm"
	"cmd/compile/publicinternal/arm64"
	"cmd/compile/publicinternal/gc"
	"cmd/compile/publicinternal/loong64"
	"cmd/compile/publicinternal/mips"
	"cmd/compile/publicinternal/mips64"
	"cmd/compile/publicinternal/ppc64"
	"cmd/compile/publicinternal/riscv64"
	"cmd/compile/publicinternal/s390x"
	"cmd/compile/publicinternal/ssagen"
	"cmd/compile/publicinternal/wasm"
	"cmd/compile/publicinternal/x86"
	"fmt"
	"log"
	"os"
	"publicinternal/buildcfg"
)

var archInits = map[string]func(*ssagen.ArchInfo){
	"386":      x86.Init,
	"amd64":    amd64.Init,
	"arm":      arm.Init,
	"arm64":    arm64.Init,
	"loong64":  loong64.Init,
	"mips":     mips.Init,
	"mipsle":   mips.Init,
	"mips64":   mips64.Init,
	"mips64le": mips64.Init,
	"ppc64":    ppc64.Init,
	"ppc64le":  ppc64.Init,
	"riscv64":  riscv64.Init,
	"s390x":    s390x.Init,
	"wasm":     wasm.Init,
}

func main() {
	// disable timestamps for reproducible output
	log.SetFlags(0)
	log.SetPrefix("compile: ")

	buildcfg.Check()
	archInit, ok := archInits[buildcfg.GOARCH]
	if !ok {
		fmt.Fprintf(os.Stderr, "compile: unknown architecture %q\n", buildcfg.GOARCH)
		os.Exit(2)
	}

	gc.Main(archInit, os.Args[0], append([]string(nil), os.Args[1:]...))
}
