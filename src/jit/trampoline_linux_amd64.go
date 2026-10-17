// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

// callEntry invokes the entry address of a generated func() entry registered
// with the Go runtime's function and stack tables.
func callEntry(entry uintptr)

// callEntryInt invokes a generated func() int entry and returns AX.
func callEntryInt(entry uintptr) uintptr
