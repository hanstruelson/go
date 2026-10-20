// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && amd64

package jit

import "runtime"

// callEntry invokes the entry address of a generated func() entry registered
// with the Go runtime's function and stack tables.
func callEntry(entry uintptr)

// callEntryInt invokes a generated func() int entry and returns AX.
func callEntryInt(entry uintptr) uintptr

// callEntryOwned invokes a generated func(*runtime.JITModuleOwner) entry.
func callEntryOwned(entry uintptr, owner *runtime.JITModuleOwner)

// callEntryOwnedInt invokes a generated func(*runtime.JITModuleOwner) int
// entry and returns AX.
func callEntryOwnedInt(entry uintptr, owner *runtime.JITModuleOwner) uintptr
