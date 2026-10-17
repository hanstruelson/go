// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !386 && !amd64 && !arm && !arm64 && !loong64 && !mips && !mipsle && !mips64 && !mips64le && !ppc64 && !ppc64le && !riscv64 && !s390x && !wasm

package jit

import "cmd/compile/publicinternal/ssagen"

var jitArchInit func(*ssagen.ArchInfo)
