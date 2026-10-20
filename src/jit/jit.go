// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package jit provides runtime Go source parsing and compilation, serialized
// package and link metadata, and an in-memory amd64 Linux object linker. Eval
// compiles a uniquely named function with the importing package's identity,
// links its imports to already-loaded package symbols, captures visible local
// variables at direct compiler-rewritten call sites, registers runtime PC
// file/line and GC metadata, executes it, and returns a Code handle for
// explicit unload. See doc/jit-in-memory-linker-plan.md for the supported
// capture subset and remaining linker/runtime limits.
package jit

import (
	"context"
	"fmt"
	"go/token"
	"runtime"
	"strconv"
	"strings"

	"cmd/compile/publicinternal/jitmeta"
	"cmd/compile/publicinternal/syntax"
)

// MemoryLimitError reports a JIT allocation rejected by the configured byte
// limit. Runtime and executable mappings remain charged until they are
// successfully released.
type MemoryLimitError = runtime.JITMemoryLimitError

// SetMemoryLimit configures the budget for executable mappings, per-module
// runtime tables, published JIT module indexes, and GC owner side tables. Zero
// means unlimited. Lowering the limit does not invalidate live modules; future
// allocations trigger a collection and fail if live or pinned modules still
// exceed it.
func SetMemoryLimit(bytes uint64) { runtime.SetJITMemoryLimit(bytes) }

// MemoryStats returns the currently charged JIT mapping, runtime table, and
// owner side-table bytes and the configured limit. It includes temporary
// index snapshots while readers drain, but excludes Go heap objects retained
// by JIT owners.
func MemoryStats() (used, limit uint64) { return runtime.JITMemoryStats() }

// Reclaim runs a complete garbage collection and waits for all JIT modules
// whose owners became unreachable to finish reader draining and unmapping.
// Live and pinned modules remain charged. Cancellation stops waiting but does
// not cancel safe retirement already queued by the runtime.
func Reclaim(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("jit: Reclaim requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.GC()
	runtime.JITPruneCaches()
	for !runtime.JITWaitForRetirements(10e6) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return nil
}

// File is a syntax tree produced by the compiler's parser.
type File = syntax.File

// Stmt is a Go statement in a syntax tree produced by the compiler's parser.
type Stmt = syntax.Stmt

// PackageContext identifies the package and file import scope in which a
// statement snippet is interpreted. It supplies parser context only; it does
// not type-check names or bind them to live package objects.
type PackageContext struct {
	PackageName string
	Imports     []Import
}

// Import records an import spelling and its resolved package path. Alias is
// empty for an unaliased import, and is "." or "_" for those import forms.
type Import = jitmeta.Import

// Parse parses one complete Go source file. The source must include a package
// clause. Syntax errors are returned without terminating the process.
func Parse(source string) (*File, error) {
	return syntax.Parse(syntax.NewFileBase("jit.go"), strings.NewReader(source), nil, nil, 0)
}

// ParseStatements parses source as the body of a synthetic function and
// returns its statements. source must contain Go statements, without a package
// clause or import declarations. Identifiers such as fmt are parsed as names;
// they are not resolved against the importing package.
//
// Positions in the returned statements refer to jit-snippet.go and use line
// numbers from source.
func ParseStatements(source string) ([]Stmt, error) {
	return ParseStatementsInContext(PackageContext{PackageName: "jit"}, source)
}

// ParseStatementsInContext parses source as statements in ctx's package and
// file import scope. The source must not include a package clause or imports;
// those are supplied by ctx. This makes the parser input match the host file's
// package/import context, but parsing alone still does not resolve identifiers
// or make imported objects available to generated code.
func ParseStatementsInContext(ctx PackageContext, source string) ([]Stmt, error) {
	if !token.IsIdentifier(ctx.PackageName) || ctx.PackageName == "_" || token.Lookup(ctx.PackageName).IsKeyword() {
		return nil, fmt.Errorf("jit: invalid package name %q", ctx.PackageName)
	}
	var wrapped strings.Builder
	fmt.Fprintf(&wrapped, "package %s\n", ctx.PackageName)
	if len(ctx.Imports) != 0 {
		wrapped.WriteString("import (\n")
		for _, imp := range ctx.Imports {
			if imp.SourcePath == "" {
				return nil, fmt.Errorf("jit: package context has an import with no source path")
			}
			if imp.Alias != "" && imp.Alias != "." && imp.Alias != "_" &&
				(!token.IsIdentifier(imp.Alias) || token.Lookup(imp.Alias).IsKeyword()) {
				return nil, fmt.Errorf("jit: invalid import alias %q", imp.Alias)
			}
			if imp.Alias != "" {
				wrapped.WriteString(imp.Alias)
				wrapped.WriteByte(' ')
			}
			wrapped.WriteString(strconv.Quote(imp.SourcePath))
			wrapped.WriteByte('\n')
		}
		wrapped.WriteString(")\n")
	}
	wrapped.WriteString("func _jit(){\n//line jit-snippet.go:1\n")
	wrapped.WriteString(source)
	wrapped.WriteString("\n}")
	file, err := syntax.Parse(
		syntax.NewFileBase("jit-snippet.go"),
		strings.NewReader(wrapped.String()),
		nil,
		nil,
		syntax.CheckBranches,
	)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("jit: parser did not produce the synthetic function")
	}
	var fn *syntax.FuncDecl
	for _, decl := range file.DeclList {
		switch candidate := decl.(type) {
		case *syntax.ImportDecl:
			// Imports are provided by the caller's package context.
		case *syntax.FuncDecl:
			if candidate.Name.Value != "_jit" {
				return nil, fmt.Errorf("jit: parser produced an unexpected top-level function")
			}
			if fn != nil {
				return nil, fmt.Errorf("jit: parser produced multiple synthetic functions")
			}
			fn = candidate
		default:
			return nil, fmt.Errorf("jit: parser produced an unexpected top-level declaration")
		}
	}
	if fn == nil || fn.Body == nil {
		return nil, fmt.Errorf("jit: parser did not produce the synthetic function body")
	}
	return fn.Body.List, nil
}
