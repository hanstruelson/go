// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"crypto/sha256"
	"errors"
	"runtime"

	"cmd/compile/publicinternal/gc"
	"cmd/compile/publicinternal/noder"
)

// SourceFile is a Go source file supplied to Compile.
type SourceFile = noder.SourceFile

// PackageExportData is a code-free import record consumed directly by the
// in-process compiler's package importer.
type PackageExportData = gc.PackageExportData

// CompileRequest describes a complete Go package to compile. Each file must
// include a package clause. PackagePath identifies the package to the Go
// compiler. Options are compiler options in -name=value form; importing other
// packages requires the corresponding compiler import configuration.
type CompileRequest struct {
	PackagePath      string
	Files            []SourceFile
	Options          []string
	PackageExports   map[string]PackageExportData
	ImportMap        map[string]string
	ArchFeaturesJSON string
	ownerAware       bool
	pinRequired      bool
}

// CompiledPackage contains the Go compiler's object archive and diagnostics.
// Object is not an executable and cannot be called directly. LoadSnippet
// rejects edits made after compilation because its owner and effect metadata
// are valid only for the exact object archive returned here.
type CompiledPackage struct {
	Object      []byte
	Diagnostics string
	// EntryName is set by CompileSnippet to the uniquely named entry function
	// in Object. Direct calls to Compile leave it empty.
	EntryName string
	// ownerAware reports that CompileSnippet rewrote the entry ABI and every
	// function literal to retain the runtime owner.
	ownerAware bool
	// pinRequired marks source constructs that may expose mapping addresses
	// through APIs the runtime cannot represent with an owner edge.
	pinRequired bool
	// captures are caller storage locations retained by the runtime owner.
	captures []runtime.JITCaptureBinding
	// effects summarizes direct runtime-source effects and records candidate
	// host call identities. LoadSnippet resolves these against the linker's
	// per-symbol transitive summaries before deciding whether the owner can be
	// reclaimed.
	effects effectSummary
	// compilerEffects is the union of source-IR effects classified during
	// compilation. Unlike syntax-only guesses, it distinguishes interface
	// dispatch from arbitrary function-value calls.
	compilerEffects uint8
	// objectHash and analyzedEntryName bind the owner/effect analysis to the
	// exact public object bytes and entry name returned by compilation.
	objectHash        [32]byte
	analyzedEntryName string
}

// Compile runs the Go compiler frontend and machine-code backend in this
// process and returns the resulting object archive. The generated code is not
// loaded or executed by this method; use LoadSnippet or Eval for that.
func Compile(req CompileRequest) (CompiledPackage, error) {
	return compileWithJITCallSite(req, "")
}

func compileWithJITCallSite(req CompileRequest, callSiteFile string) (CompiledPackage, error) {
	if jitArchInit == nil {
		return CompiledPackage{}, errors.New("jit: compiler backend is unavailable for this architecture")
	}
	result, err := gc.Compile(gc.Request{
		ArchInit:         jitArchInit,
		GOARCH:           runtime.GOARCH,
		PackagePath:      req.PackagePath,
		Files:            req.Files,
		Options:          req.Options,
		PackageExports:   req.PackageExports,
		ImportMap:        req.ImportMap,
		ArchFeaturesJSON: req.ArchFeaturesJSON,
		JITCallSiteFile:  callSiteFile,
	})
	if err != nil {
		return CompiledPackage{}, err
	}
	return CompiledPackage{
		Object: result.Object, Diagnostics: result.Diagnostics, ownerAware: req.ownerAware,
		pinRequired: req.pinRequired, compilerEffects: result.JITEffects,
		objectHash: sha256.Sum256(result.Object),
	}, nil
}
