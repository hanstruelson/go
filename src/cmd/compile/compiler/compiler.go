// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package compiler exposes an in-process source compilation API to other Go
// toolchain packages. A request compiles for the architecture of this
// toolchain build and returns the Go object archive plus diagnostics.
package compiler

import (
	"cmd/compile/internal/archinit"
	"cmd/compile/internal/gc"
	"cmd/compile/internal/noder"
	"errors"
	"fmt"
	"internal/buildcfg"
)

// SourceFile is a Go source file supplied directly as text. Name is used for
// diagnostics and source positions; Data contains the source code.
type SourceFile struct {
	Name string
	Data string
}

// Request describes one package compilation. Options are cmd/compile flags in
// -name=value form. The API manages output and rejects options that write
// process-global profiles, mutate the environment, or terminate the process.
type Request struct {
	PackagePath string
	Files       []SourceFile
	Options     []string
}

// Result contains the compiled Go object archive and diagnostics emitted by
// the compiler.
type Result struct {
	Object       []byte
	Diagnostics  string
	Errors       int
	SyntaxErrors int
}

// Compile parses and compiles the request's source files in-process. Calls are
// serialized because the compiler still uses process-global adapters for
// command-line state, diagnostics, and output.
func Compile(req Request) (Result, error) {
	archInit, ok := archinit.Lookup(buildcfg.GOARCH)
	if !ok {
		return Result{}, fmt.Errorf("compiler does not support architecture %q", buildcfg.GOARCH)
	}

	files := make([]noder.SourceFile, len(req.Files))
	for i, file := range req.Files {
		files[i] = noder.SourceFile{Name: file.Name, Data: file.Data}
	}
	compiled, err := gc.Compile(gc.Request{
		ArchInit:    archInit,
		PackagePath: req.PackagePath,
		Files:       files,
		Options:     req.Options,
	})
	result := Result{
		Object:       compiled.Object,
		Diagnostics:  compiled.Diagnostics,
		Errors:       compiled.Errors,
		SyntaxErrors: compiled.SyntaxErrors,
	}
	if err == nil {
		return result, nil
	}
	var compileErr *gc.CompileError
	if errors.As(err, &compileErr) {
		return result, &CompileError{Status: compileErr.Status, Diagnostics: compileErr.Diagnostics}
	}
	return result, err
}

// CompileError reports a compiler failure without terminating the process.
type CompileError struct {
	Status      int
	Diagnostics string
}

func (e *CompileError) Error() string {
	if e.Diagnostics != "" {
		return e.Diagnostics
	}
	return fmt.Sprintf("compiler exited with status %d", e.Status)
}
