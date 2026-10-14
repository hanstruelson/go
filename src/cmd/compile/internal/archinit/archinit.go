// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package archinit provides the architecture initializers used by cmd/compile.
package archinit

import (
	"cmd/compile/internal/amd64"
	"cmd/compile/internal/arm"
	"cmd/compile/internal/arm64"
	"cmd/compile/internal/loong64"
	"cmd/compile/internal/mips"
	"cmd/compile/internal/mips64"
	"cmd/compile/internal/ppc64"
	"cmd/compile/internal/riscv64"
	"cmd/compile/internal/s390x"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/wasm"
	"cmd/compile/internal/x86"
)

var initializers = map[string]func(*ssagen.ArchInfo){
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

// Lookup returns the compiler architecture initializer for arch.
func Lookup(arch string) (func(*ssagen.ArchInfo), bool) {
	init, ok := initializers[arch]
	return init, ok
}
