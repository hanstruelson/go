// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux || !amd64

package jit

import "fmt"

func loadLinkedSnippet(_ *Metadata, _ CompiledPackage) (*mappedImage, error) {
	return nil, fmt.Errorf("jit: runtime linking currently requires linux/amd64")
}

func closeMappedImage(_ *mappedImage) error {
	return fmt.Errorf("jit: runtime image unloading currently requires linux/amd64")
}

func callEntry(uintptr)            { panic("jit: runtime entry calls require linux/amd64") }
func callEntryInt(uintptr) uintptr { panic("jit: runtime entry calls require linux/amd64") }
