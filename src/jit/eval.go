// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"fmt"
	"runtime"
)

// Eval parses, type-checks, compiles, links, and executes source at runtime in
// the importing file's package/import context. Package-level imports are
// resolved to the functions and variables already linked into the process.
// Direct source calls are rewritten by cmd/compile to carry exact package,
// file, and visible local-variable addresses. Calls made through a function
// value use the runtime caller fallback and cannot capture caller locals. It
// returns the loaded code handle; call Close only when the
// generated code and every function value, callback, or goroutine it created
// can no longer be used. If an error is returned with a non-nil handle, retain
// it and call Close to retry cleanup or manage the loaded code. If execution
// panics, the panic propagates. Owner-aware modules become eligible for GC
// retirement after the panic unwinds unless an escaped value still retains
// them; modules requiring a pin remain pinned. Use CompileSnippet and
// LoadSnippet before Run when the caller needs to retain and manage the Code
// handle across panic recovery.
func Eval(source string) (*Code, error) {
	_, filename, _, ok := runtime.Caller(1)
	if !ok {
		return nil, fmt.Errorf("jit: cannot identify Eval call site")
	}
	var selected *Metadata
	for _, metadata := range RegisteredMetadata() {
		if _, err := metadata.ContextForFile(filename); err != nil {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("jit: call site %q matches more than one package metadata bundle", filename)
		}
		selected = metadata
	}
	if selected == nil {
		return nil, fmt.Errorf("jit: no embedded package metadata matches Eval call site %q", filename)
	}
	return evalWithMetadata(selected, filename, source)
}

// EvalAt is the compatibility compiler entry point for calls emitted by older
// compiler objects. New direct Eval calls use EvalAtCaptured.
func EvalAt(packagePath, fileName, source string) (*Code, error) {
	metadata, ok := MetadataFor(packagePath)
	if !ok {
		return nil, fmt.Errorf("jit: no embedded metadata for Eval caller package %q", packagePath)
	}
	return evalWithMetadata(metadata, fileName, source)
}

// EvalAtCaptured is the compiler support entry point for direct calls to
// Eval. In addition to the exact package/file context, the compiler passes
// addresses for the caller's in-scope variables. The generated snippet uses
// those same storage locations for reads and writes.
func EvalAtCaptured(packagePath, fileName, source string, captures ...Capture) (*Code, error) {
	// Keep the runtime accessor linked into the executable. Generated snippets
	// call it through their owner parameter, which the static linker cannot see.
	runtime.KeepAlive(runtime.JITCaptureAddr)
	metadata, ok := MetadataFor(packagePath)
	if !ok {
		return nil, fmt.Errorf("jit: no embedded metadata for Eval caller package %q", packagePath)
	}
	return evalWithMetadata(metadata, fileName, source, captures...)
}

func evalWithMetadata(metadata *Metadata, fileName, source string, captures ...Capture) (*Code, error) {
	compiled, err := metadata.CompileSnippet(fileName, source, captures...)
	if err != nil {
		return nil, err
	}
	code, err := metadata.LoadSnippet(compiled)
	if err != nil {
		return code, err
	}
	if err := code.Run(); err != nil {
		return code, err
	}
	return code, nil
}
