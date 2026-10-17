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
// It returns the loaded code handle; call Close when the generated code and
// any function values or goroutines it created can no longer be used.
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
	compiled, err := selected.CompileSnippet(filename, source)
	if err != nil {
		return nil, err
	}
	code, err := selected.LoadSnippet(compiled)
	if err != nil {
		return nil, err
	}
	keepLoaded := false
	defer func() {
		if !keepLoaded {
			_ = code.Close()
		}
	}()
	if err := code.Run(); err != nil {
		return nil, err
	}
	keepLoaded = true
	return code, nil
}
