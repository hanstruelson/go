// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements support functionality for iimport.go.

package testimporter

import (
	"cmd/compile/publicinternal/types2"
	"go/token"
	"publicinternal/pkgbits"
)

func assert(p bool) {
	if !p {
		panic("test importer invariant violated")
	}
}

const deltaNewFile = -64 // see cmd/compile/publicinternal/gc/bexport.go

// Synthesize a token.Pos
type fakeFileSet struct {
	fset  *token.FileSet
	files map[string]*token.File
}

type anyType struct{}

func (t anyType) Underlying() types2.Type { return t }
func (t anyType) String() string          { return "any" }

// See cmd/compile/publicinternal/noder.derivedInfo.
type derivedInfo struct {
	idx    pkgbits.Index
	needed bool
}

// See cmd/compile/publicinternal/noder.typeInfo.
type typeInfo struct {
	idx     pkgbits.Index
	derived bool
}
