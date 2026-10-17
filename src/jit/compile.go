// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"errors"
	"runtime"

	"cmd/compile/publicinternal/gc"
	"cmd/compile/publicinternal/noder"
)

// SourceFile is a Go source file supplied to Compile.
type SourceFile = noder.SourceFile

// CompileRequest describes a complete Go package to compile. Each file must
// include a package clause. PackagePath identifies the package to the Go
// compiler. Options are compiler options in -name=value form; importing other
// packages requires the corresponding compiler import configuration.
type CompileRequest struct {
	PackagePath     string
	Files           []SourceFile
	Options         []string
	PackageArchives map[string][]byte
	ImportMap       map[string]string
}

// CompiledPackage contains the Go compiler's object archive and diagnostics.
// Object is not an executable and cannot be called directly.
type CompiledPackage struct {
	Object      []byte
	Diagnostics string
}

// Compile runs the Go compiler frontend and machine-code backend in this
// process and returns the resulting object archive. The generated code is not
// loaded or executed by this method; use LoadSnippet or Eval for that.
func Compile(req CompileRequest) (CompiledPackage, error) {
	if jitArchInit == nil {
		return CompiledPackage{}, errors.New("jit: compiler backend is unavailable for this architecture")
	}
	result, err := gc.Compile(gc.Request{
		ArchInit:        jitArchInit,
		GOARCH:          runtime.GOARCH,
		PackagePath:     req.PackagePath,
		Files:           req.Files,
		Options:         req.Options,
		PackageArchives: req.PackageArchives,
		ImportMap:       req.ImportMap,
	})
	if err != nil {
		return CompiledPackage{}, err
	}
	return CompiledPackage{Object: result.Object, Diagnostics: result.Diagnostics}, nil
}
