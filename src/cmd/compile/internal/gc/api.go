// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/logopt"
	"cmd/compile/internal/noder"
	"cmd/compile/internal/ssacompile"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/typecheck"
	"cmd/internal/dwarf"
	"cmd/internal/obj"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var compileRequestMu sync.Mutex

// Request describes one package compilation. Each source file has a stable
// name for diagnostics and source positions. Options are ordinary cmd/compile
// flags in -name=value form, excluding process-output, profiling, and
// environment-changing flags.
type Request struct {
	ArchInit    func(*ssagen.ArchInfo)
	PackagePath string
	Files       []noder.SourceFile
	Options     []string
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
// archive. Requests run one at a time while legacy command helpers still use
// process-level base and flag adapters. Each request gets fresh target, IR,
// type-universe, frontend, and linker contexts.
func Compile(req Request) (result Result, err error) {
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

	compileRequestMu.Lock()
	defer compileRequestMu.Unlock()

	tmpDir, err := os.MkdirTemp("", "go-compile-request-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(tmpDir)

	outputPath := filepath.Join(tmpDir, "out.o")
	capture, err := os.Create(filepath.Join(tmpDir, "compiler.log"))
	if err != nil {
		return result, err
	}
	defer capture.Close()

	sourceFiles, fileNames, err := requestSourceFiles(req.Files)
	if err != nil {
		return result, err
	}

	oldArgs := os.Args
	oldStdout, oldStderr := os.Stdout, os.Stderr
	oldFlagSet := flag.CommandLine
	oldUsage := flag.Usage
	oldLogWriter := log.Writer()
	defer func() {
		os.Args = oldArgs
		os.Stdout, os.Stderr = oldStdout, oldStderr
		flag.CommandLine = oldFlagSet
		flag.Usage = oldUsage
		log.SetOutput(oldLogWriter)
	}()
	oldDwarfLogging := dwarf.LoggingEnabled()
	defer dwarf.EnableLogging(oldDwarfLogging)
	restoreFIPSDebugHash := obj.SaveFIPSDebugHash()
	defer restoreFIPSDebugHash()
	oldTraceIndent := typecheck.SwapTraceIndent(nil)
	defer typecheck.SwapTraceIndent(oldTraceIndent)
	logoptSession := logopt.NewSession()
	defer logoptSession.Close()
	ssaSession := ssacompile.NewSession()
	defer ssaSession.Close()

	os.Stdout, os.Stderr = capture, capture
	log.SetOutput(capture)
	flag.CommandLine = flag.NewFlagSet("compile", flag.ContinueOnError)
	flag.CommandLine.SetOutput(capture)
	os.Args = makeCompileArgs(req, fileNames, outputPath)

	session := base.NewSession()
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		mainWithSources(req.ArchInit, sourceFiles)
	}()
	func() {
		defer func() {
			if p := recover(); p != nil && panicValue == nil {
				panicValue = p
			}
		}()
		session.Close()
	}()
	result.Errors = session.Errors
	result.SyntaxErrors = session.SyntaxErrors

	if err := capture.Sync(); err != nil {
		return result, err
	}
	if _, err := capture.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	diagnostics, err := io.ReadAll(capture)
	if err != nil {
		return result, err
	}
	result.Diagnostics = strings.ToValidUTF8(string(diagnostics), "�")

	if panicValue != nil || result.Errors != 0 {
		status := 2
		if exit, ok := panicValue.(base.ExitError); ok {
			status = int(exit)
		}
		return result, &CompileError{Status: status, Diagnostics: result.Diagnostics}
	}

	result.Object, err = os.ReadFile(outputPath)
	if err != nil {
		return result, fmt.Errorf("read compiler object: %w", err)
	}
	return result, nil
}

func requestSourceFiles(files []noder.SourceFile) ([]noder.SourceFile, []string, error) {
	sources := append([]noder.SourceFile(nil), files...)
	names := make([]string, len(files))
	seen := make(map[string]bool, len(files))
	for i := range sources {
		name := sources[i].Name
		if name == "" {
			name = fmt.Sprintf("input%d.go", i+1)
		}
		if strings.HasPrefix(name, "-") {
			return nil, nil, fmt.Errorf("source name %q starts with '-'", name)
		}
		if strings.HasPrefix(name, "@") {
			return nil, nil, fmt.Errorf("source name %q starts with '@'", name)
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("duplicate source name %q", name)
		}
		seen[name] = true
		sources[i].Name = name
		names[i] = name
	}
	return sources, names, nil
}

func makeCompileArgs(req Request, files []string, output string) []string {
	args := append([]string(nil), req.Options...)
	args = append(args, "-p", req.PackagePath, "-o", output)
	args = append(args, files...)
	return append([]string{"compile"}, args...)
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
		case "V":
			return fmt.Errorf("compile option %q terminates the process and is unavailable in the request API", option)
		case "d":
			for _, setting := range strings.Split(value, ",") {
				key, _, _ := strings.Cut(setting, "=:")
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
