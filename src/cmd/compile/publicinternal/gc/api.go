// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"bytes"
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/noder"
	"cmd/compile/publicinternal/ssagen"
	"cmd/publicinternal/bio"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Request describes one package compilation. Each source file has a stable
// name for diagnostics and source positions. Options are ordinary cmd/compile
// flags in -name=value form, excluding process-output, profiling,
// environment-changing, and dynamic-linking flags.
type Request struct {
	ArchInit func(*ssagen.ArchInfo)
	// GOARCH selects a target architecture variant when ArchInit supports more
	// than one architecture (for example mips/mipsle). If empty, Compile uses
	// the executing process architecture. It must match the architecture
	// selected by ArchInit.
	GOARCH      string
	PackagePath string
	Files       []noder.SourceFile
	Options     []string
	// PackageArchives supplies compiler archives by resolved import path.
	// ImportMap translates source import paths to those resolved paths.
	PackageArchives map[string][]byte
	ImportMap       map[string]string
	objectOutput    *bio.Writer
}

// Result contains the compiler object archive and diagnostics emitted while
// compiling the request.
type Result struct {
	Object       []byte
	Diagnostics  string
	Errors       int
	SyntaxErrors int
}

// CompileError reports a failed request without terminating the process.
type CompileError struct {
	Status      int
	Diagnostics string
}

func (e *CompileError) Error() string {
	if msg := strings.TrimSpace(e.Diagnostics); msg != "" {
		return msg
	}
	return fmt.Sprintf("compiler exited with status %d", e.Status)
}

// Compile parses and compiles named source strings and returns the Go object
// archive. The request path parses options into target-owned state and does
// not install command-line flags, arguments, standard streams, or base globals.
// Each request gets fresh target, IR, type-universe, frontend, and linker
// contexts.
func Compile(req Request) (result Result, err error) {
	if req.GOARCH == "" {
		req.GOARCH = runtime.GOARCH
	}
	if req.ArchInit == nil {
		return result, errors.New("compile request has no architecture initializer")
	}
	if req.PackagePath == "" {
		return result, errors.New("compile request has no package path")
	}
	if len(req.Files) == 0 {
		return result, errors.New("compile request has no source files")
	}
	if err := validateRequestOptions(req.Options); err != nil {
		return result, err
	}

	sourceFiles, err := requestSourceFiles(req.Files)
	if err != nil {
		return result, err
	}

	var capture bytes.Buffer
	var target *ir.Package
	var panicValue any
	req.objectOutput = bio.NewBufferWriter()
	func() {
		defer func() { panicValue = recover() }()
		mainWithConfig(req.ArchInit, sourceFiles, true, &req, "", &capture, func(t *ir.Package) { target = t }, nil)
	}()
	if target != nil && target.Diagnostics != nil {
		result.Errors = target.Diagnostics.Errors()
		result.SyntaxErrors = target.Diagnostics.SyntaxErrors()
	}

	result.Diagnostics = strings.ToValidUTF8(capture.String(), "�")

	if panicValue != nil || result.Errors != 0 {
		status := 2
		if exit, ok := panicValue.(base.ExitError); ok {
			status = int(exit)
		}
		if result.Diagnostics == "" && panicValue != nil {
			result.Diagnostics = fmt.Sprintf("compiler initialization failed: %v\n", panicValue)
		}
		return result, &CompileError{Status: status, Diagnostics: result.Diagnostics}
	}

	result.Object, err = req.objectOutput.Bytes()
	if err != nil {
		return result, fmt.Errorf("read in-memory compiler object: %w", err)
	}
	return result, nil
}

func requestSourceFiles(files []noder.SourceFile) ([]noder.SourceFile, error) {
	sources := append([]noder.SourceFile(nil), files...)
	seen := make(map[string]bool, len(files))
	for i := range sources {
		name := sources[i].Name
		if name == "" {
			name = fmt.Sprintf("input%d.go", i+1)
		}
		if strings.HasPrefix(name, "-") {
			return nil, fmt.Errorf("source name %q starts with '-'", name)
		}
		if strings.HasPrefix(name, "@") {
			return nil, fmt.Errorf("source name %q starts with '@'", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate source name %q", name)
		}
		seen[name] = true
		sources[i].Name = name
	}
	return sources, nil
}

func validateRequestOptions(options []string) error {
	for _, option := range options {
		if !strings.HasPrefix(option, "-") || strings.HasPrefix(option, "--") || !strings.Contains(option, "=") {
			return fmt.Errorf("compile option %q must use -name=value form", option)
		}
		name := strings.TrimPrefix(option, "-")
		value := ""
		if before, _, ok := strings.Cut(name, "="); ok {
			name, value = before, name[len(before)+1:]
		}
		switch name {
		case "p", "o", "env", "asmhdr", "bench", "cpuprofile", "memprofile", "memprofilerate", "blockprofile", "mutexprofile", "traceprofile", "trace", "json", "linkobj", "exportfd":
			return fmt.Errorf("compile option %q is managed by the request API", option)
		case "shared", "linkshared":
			return fmt.Errorf("compile option %q is unavailable in reusable compilation", option)
		case "V":
			return fmt.Errorf("compile option %q terminates the process and is unavailable in the request API", option)
		case "d":
			for _, setting := range strings.Split(value, ",") {
				key, _, _ := strings.Cut(setting, "=:")
				if strings.HasPrefix(key, "ssa/") {
					return fmt.Errorf("compile option %q writes process-level SSA debug output and is unavailable in the request API", option)
				}
				if key == "help" {
					return fmt.Errorf("compile option %q terminates the process and is unavailable in the request API", option)
				}
				if key == "astdump" || key == "dumpinlfuncprops" || key == "html" || strings.HasPrefix(key, "ssa/") && strings.HasSuffix(key, "/dump") {
					return fmt.Errorf("compile option %q writes files outside the request result", option)
				}
			}
		}
	}
	return nil
}
