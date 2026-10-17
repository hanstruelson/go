// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build mips64 || mips64le

package jit

import "cmd/compile/publicinternal/mips64"

var jitArchInit = mips64.Init
