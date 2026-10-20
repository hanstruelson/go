// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

var nextSnippetFunctionID atomic.Uint64
var nextPinID atomic.Uint64

var pinRoots = struct {
	sync.Mutex
	codes map[uint64]*Code
}{codes: make(map[uint64]*Code)}

// Code is a mapped in-memory snippet registered with the Go runtime's PC and
// stack-map tables while loaded. Closing drops this handle; owner-aware code
// and escaped closures retain the mapping through a GC-visible runtime owner.
type Code struct {
	mu      sync.Mutex
	cond    *sync.Cond
	image   *mappedImage
	active  int
	pins    uint64
	closing bool
	closed  bool
}

// Pin keeps a Code mapping available while the caller holds or registers a
// raw program counter. Release it only after every external use of every PC
// obtained from the pin has stopped. Pin is an explicit lifetime handle; it
// does not make arbitrary copies of a PC visible to the garbage collector.
type Pin struct {
	mu       sync.Mutex
	code     *Code
	id       uint64
	released bool
}

// CodePC is an owner-bearing program counter. Keep the value alive while using
// Address; do not copy the returned integer into storage that can outlive it.
// Use Code.Pin when a raw address must outlive ordinary Go references.
type CodePC struct {
	pc    uintptr
	owner *runtime.JITModuleOwner
}

// Address returns this function's entry PC. Keep the CodePC live for the full
// duration of any call or metadata lookup using the result. Since Go may end
// a local's lifetime after its last syntactic use, callers that use the raw
// address after this method returns should use runtime.KeepAlive on the
// CodePC after the call or lookup.
func (p CodePC) Address() uintptr {
	pc := p.pc
	runtime.KeepAlive(p.owner)
	return pc
}

// LoadSnippet links a compiler object against the host symbols recorded in m
// and loads the resulting Go module into the process. If cleanup of a partial
// mapping fails, it returns both a non-nil Code handle and the error; the
// caller must retain that handle and retry Close.
func (m *Metadata) LoadSnippet(compiled CompiledPackage) (*Code, error) {
	image, err := loadLinkedSnippet(m, compiled)
	if err != nil {
		if image == nil {
			return nil, err
		}
		// The mapper can fail while releasing a partially constructed image.
		// Return its handle with the error so callers can retry Close instead of
		// losing the runtime-accounted mapping token.
		code := &Code{image: image}
		code.cond = sync.NewCond(&code.mu)
		return code, err
	}
	code := &Code{image: image}
	code.cond = sync.NewCond(&code.mu)
	return code, nil
}

// Run calls the linked snippet through the host architecture's trampoline.
func (c *Code) Run() error {
	entry, owner, ownerAware, done, err := c.beginRun()
	if err != nil {
		return err
	}
	defer done()
	if ownerAware {
		callEntryOwned(entry, owner)
	} else {
		callEntry(entry)
	}
	return nil
}

// RunInt calls a func() int entry and returns its result.
func (c *Code) RunInt() (int, error) {
	entry, owner, ownerAware, done, err := c.beginRun()
	if err != nil {
		return 0, err
	}
	defer done()
	if ownerAware {
		return int(callEntryOwnedInt(entry, owner)), nil
	}
	return int(callEntryInt(entry)), nil
}

// RunPointer calls a func() unsafe.Pointer entry and returns its pointer
// result. A returned pointer into the generated image keeps the module alive
// through the runtime GC's JIT address barrier. Converting it to uintptr drops
// that automatic lifetime edge; use Pin before retaining such an address as an
// integer or passing it to foreign code.
func (c *Code) RunPointer() (unsafe.Pointer, error) {
	entry, owner, ownerAware, done, err := c.beginRun()
	if err != nil {
		return nil, err
	}
	defer done()
	var address uintptr
	if ownerAware {
		address = callEntryOwnedInt(entry, owner)
	} else {
		address = callEntryInt(entry)
	}
	return unsafe.Pointer(address), nil
}

// Pin retains this code until Pin.Release. Use it before exposing a raw
// program counter to an integer, unsafe storage, or foreign code. Ordinary
// calls through Code.Run and Code.RunInt do not need a pin.
func (c *Code) Pin() (*Pin, error) {
	if c == nil {
		return nil, fmt.Errorf("jit: code is not loaded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing || c.image == nil || c.image.entry == 0 {
		return nil, fmt.Errorf("jit: code is not loaded")
	}
	if c.pins == ^uint64(0) {
		return nil, fmt.Errorf("jit: pin count exhausted")
	}
	id, ok := allocatePinID()
	if !ok {
		return nil, fmt.Errorf("jit: pin identifier space exhausted")
	}
	c.pins++
	pinRoots.Lock()
	pinRoots.codes[id] = c
	pinRoots.Unlock()
	return &Pin{code: c, id: id}, nil
}

// ActivePins reports the number of pins that have not been released. It is a
// diagnostic count and does not account for raw addresses copied without a
// Pin or prove that external users have stopped executing them.
func (c *Code) ActivePins() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pins
}

// PC returns an owner-bearing handle to the loaded entry point. The returned
// CodePC keeps the executable mapping and runtime metadata alive after Code is
// closed. Use Pin when the address must be retained as an integer or by code
// outside Go's garbage-collected object graph.
func (c *Code) PC() (CodePC, error) {
	if c == nil {
		return CodePC{}, fmt.Errorf("jit: code is not loaded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing || c.image == nil || c.image.entry == 0 || c.image.runtimeOwner == nil {
		return CodePC{}, fmt.Errorf("jit: code is not loaded")
	}
	return CodePC{pc: c.image.entry, owner: c.image.runtimeOwner}, nil
}

// PC returns the entry address while this pin is active. The caller must not
// use the returned address concurrently with or after Release. Release does
// not wait for foreign execution; the caller must deregister and quiesce all
// external uses first.
func (p *Pin) PC() (uintptr, error) {
	if p == nil {
		return 0, fmt.Errorf("jit: pin is released")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released || p.code == nil {
		return 0, fmt.Errorf("jit: pin is released")
	}
	c := p.code
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.image == nil || c.image.entry == 0 {
		return 0, fmt.Errorf("jit: code is not loaded")
	}
	return c.image.entry, nil
}

// Release drops this pin. It is idempotent. Before calling it, the caller
// must stop all foreign or raw-PC execution and remove every copied address;
// the runtime cannot discover those references. Other active Pin handles
// continue to block Code.Close. A Code reference by itself does not prevent an
// explicit Code.Close.
func (p *Pin) Release() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return
	}
	c := p.code
	id := p.id
	p.code = nil
	p.id = 0
	p.released = true
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.pins <= 0 {
		c.mu.Unlock()
		panic("jit: pin count underflow")
	}
	c.pins--
	c.mu.Unlock()
	pinRoots.Lock()
	delete(pinRoots.codes, id)
	pinRoots.Unlock()
}

func allocatePinID() (uint64, bool) {
	for {
		current := nextPinID.Load()
		if current == ^uint64(0) {
			return 0, false
		}
		next := current + 1
		if nextPinID.CompareAndSwap(current, next) {
			return next, true
		}
	}
}

func (c *Code) beginRun() (uintptr, *runtime.JITModuleOwner, bool, func(), error) {
	if c == nil {
		return 0, nil, false, nil, fmt.Errorf("jit: code is not loaded")
	}
	c.mu.Lock()
	if c.closed || c.closing || c.image == nil || c.image.entry == 0 {
		c.mu.Unlock()
		return 0, nil, false, nil, fmt.Errorf("jit: code is not loaded")
	}
	entry := c.image.entry
	owner := c.image.runtimeOwner
	ownerAware := c.image.ownerAware
	c.active++
	c.mu.Unlock()
	return entry, owner, ownerAware, func() {
		c.mu.Lock()
		c.active--
		if c.active == 0 && c.cond != nil {
			c.cond.Broadcast()
		}
		c.mu.Unlock()
	}, nil
}

// Close releases the caller's handle and lets the Go garbage collector retire
// owner-aware code after its active frames and escaped closures are gone. It
// does not synchronously unmap executable memory. It rejects active Run calls
// and explicit Pin handles. Heap objects allocated with JIT-defined types and
// dynamic itab backing storage carry GC-traced owner edges. Object code
// compiled without owner propagation or requiring manual lifetime tracking
// remains pinned for process lifetime.
func (c *Code) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.cond == nil {
		c.cond = sync.NewCond(&c.mu)
	}
	for c.closing && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.active != 0 {
		active := c.active
		c.mu.Unlock()
		return fmt.Errorf("jit: cannot close code while %d Run call(s) are active", active)
	}
	if c.pins != 0 {
		pins := c.pins
		c.mu.Unlock()
		return fmt.Errorf("jit: cannot close code while %d Pin handle(s) are active", pins)
	}
	if c.image != nil && c.image.pinnedReason != "" {
		reason := c.image.pinnedReason
		c.mu.Unlock()
		return fmt.Errorf("jit: cannot safely unload code: %s", reason)
	}
	c.closing = true
	if c.image != nil && c.image.runtimeOwner == nil {
		if err := closeMappedImage(c.image); err != nil {
			c.closing = false
			c.cond.Broadcast()
			c.mu.Unlock()
			return err
		}
	}
	c.image = nil
	c.closed = true
	c.closing = false
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

// Release is an explicit alias for Close. Retirement remains asynchronous and
// follows the owner edges retained by generated code and function values.
func (c *Code) Release() error { return c.Close() }

// CompileSnippet parses and type-checks statements against the selected file's
// import scope and the host package's saved declarations, then compiles them
// as a uniquely named function with the host package path. References to
// existing package declarations are linked to their already-loaded host
// symbols. Captures are supplied by compiler-rewritten direct Eval calls;
// callers using CompileSnippet directly cannot provide lexical locals.
func (m *Metadata) CompileSnippet(fileName, source string, captures ...Capture) (CompiledPackage, error) {
	if m.Build.Race || m.Build.MSan || m.Build.ASan || m.Build.Coverage {
		mode := "instrumented"
		switch {
		case m.Build.Race:
			mode = "race"
		case m.Build.MSan:
			mode = "MSan"
		case m.Build.ASan:
			mode = "ASan"
		case m.Build.Coverage:
			mode = "coverage"
		}
		return CompiledPackage{}, fmt.Errorf("jit: runtime compilation is disabled for %s-instrumented executables until that mode is propagated to generated code", mode)
	}
	ctx, err := m.ContextForFile(fileName)
	if err != nil {
		return CompiledPackage{}, err
	}
	captureByName := make(map[string]Capture, len(captures))
	captureNames := make(map[string]bool, len(captures))
	captureBases := make(map[string]string, len(captures))
	for i, capture := range captures {
		if _, exists := captureByName[capture.name]; exists {
			return CompiledPackage{}, fmt.Errorf("jit: duplicate lexical capture %q", capture.name)
		}
		captureByName[capture.name] = capture
		captureNames[capture.name] = true
		if capture.kind == captureValue {
			captureBases[capture.name] = fmt.Sprintf("_jit_capture_%d", i)
		}
	}
	if _, err := ParseStatementsInContext(ctx, source); err != nil {
		return CompiledPackage{}, err
	}

	entryName, err := nextSnippetFunctionName(m)
	if err != nil {
		return CompiledPackage{}, err
	}
	reservedNames := make(map[string]bool)
	for _, declaration := range m.Build.Declarations {
		for _, name := range declaration.Names {
			reservedNames[name] = true
		}
	}
	for _, capture := range captures {
		reservedNames[capture.name] = true
	}
	runtimeAlias, runtimeDot, runtimePath, hasRuntimeImport := snippetRuntimeImport(ctx, entryName)
	ownerName, keepAliveName, rewrittenSource, captureLocals, captureUses, pinRequired, err := rewriteSnippetOwnerCapture(source, entryName, reservedNames, captureBases, captureNames)
	if err != nil {
		return CompiledPackage{}, err
	}
	// Unsupported lexical names must not make unrelated Eval calls fail. Keep
	// only the variables referenced by the snippet, while preserving supported
	// constants and aliases as local declarations so they can satisfy one
	// another's type expressions. The compiler marks aliases whose identity or
	// dependencies cannot be recreated as unsupported, and those are included
	// only when the snippet names them.
	var activeCaptures []Capture
	for _, capture := range captures {
		used := captureUses[capture.name]
		if capture.unsupported != "" {
			if used {
				if capture.kind == captureValue && capture.runtimeType != nil && strings.Contains(capture.unsupported, "generic parameter") {
					typeExpr, imports, err := runtimeCaptureTypeExpression(m, ctx, captures, capture)
					if err != nil {
						return CompiledPackage{}, fmt.Errorf("jit: cannot capture %q: %w", capture.name, err)
					}
					capture.typeExpr = typeExpr
					capture.imports = imports
					capture.unsupported = ""
				} else {
					if err := capture.validate(); err != nil {
						return CompiledPackage{}, err
					}
				}
			} else {
				continue
			}
		}
		if capture.kind == captureValue && !used {
			continue
		}
		if err := capture.validate(); err != nil {
			return CompiledPackage{}, err
		}
		activeCaptures = append(activeCaptures, capture)
	}
	capturePointers := make([]unsafe.Pointer, 0, len(activeCaptures))
	captureImportAliases := make(map[string]string)
	for _, capture := range activeCaptures {
		if capture.kind == captureValue {
			capturePointers = append(capturePointers, capture.address)
		}
		for _, imp := range capture.imports {
			if previous, exists := captureImportAliases[imp.path]; exists && previous != imp.alias {
				return CompiledPackage{}, fmt.Errorf("jit: conflicting capture aliases %q and %q for type package %q", previous, imp.alias, imp.path)
			}
			captureImportAliases[imp.path] = imp.alias
		}
	}
	if alias, ok := captureImportAliases["runtime"]; ok {
		if alias == "." {
			runtimeAlias, runtimeDot = "", true
		} else {
			for _, imp := range ctx.Imports {
				if imp.Path == "runtime" && imp.Alias == "_" {
					runtimeAlias = alias
					break
				}
			}
		}
	}
	usedImports, err := snippetImports(rewrittenSource, ctx, m.Build.Packages)
	if err != nil {
		return CompiledPackage{}, err
	}
	for path := range captureImportAliases {
		usedImports[path] = true
	}
	for _, imp := range ctx.Imports {
		if usedImports[imp.Path] && imp.Path == "unsafe" {
			pinRequired = true
		}
	}
	usedImports["runtime"] = true
	var file strings.Builder
	fmt.Fprintf(&file, "package %s\n", ctx.PackageName)
	if len(usedImports) != 0 {
		file.WriteString("import (\n")
		emittedRuntime := false
		emittedPaths := make(map[string]bool, len(usedImports))
		for _, imp := range ctx.Imports {
			if !usedImports[imp.Path] {
				continue
			}
			if emittedPaths[imp.Path] {
				continue
			}
			alias := imp.Alias
			if alias == "_" {
				if typeAlias := captureImportAliases[imp.Path]; typeAlias != "" {
					alias = typeAlias
				}
			}
			if imp.Path == "runtime" && alias == "_" {
				file.WriteString(runtimeAlias)
			} else if alias != "" {
				file.WriteString(alias)
			}
			if alias != "" || imp.Path == "runtime" {
				file.WriteByte(' ')
			}
			file.WriteString(strconv.Quote(imp.SourcePath))
			file.WriteByte('\n')
			emittedPaths[imp.Path] = true
			if imp.Path == "runtime" {
				emittedRuntime = true
			}
		}
		for path, alias := range captureImportAliases {
			if emittedPaths[path] {
				continue
			}
			fmt.Fprintf(&file, "%s %s\n", alias, strconv.Quote(path))
			emittedPaths[path] = true
		}
		if !emittedRuntime {
			file.WriteString(runtimeAlias)
			file.WriteByte(' ')
			file.WriteString(strconv.Quote(runtimePath))
			file.WriteByte('\n')
		}
		file.WriteString(")\n")
	}
	runtimeType := "JITModuleOwner"
	runtimeKeepAlive := "KeepAlive"
	if !runtimeDot {
		runtimeType = runtimeAlias + "." + runtimeType
		runtimeKeepAlive = runtimeAlias + "." + runtimeKeepAlive
	}
	fmt.Fprintf(&file, "func %s(%s *%s) { %s(%s) }\n", keepAliveName, ownerName, runtimeType, runtimeKeepAlive, ownerName)
	fmt.Fprintf(&file, "func %s(%s *%s) {\n", entryName, ownerName, runtimeType)
	fmt.Fprintf(&file, "defer %s(%s)\n", keepAliveName, ownerName)
	runtimeCaptureAddr := "JITCaptureAddr"
	if !runtimeDot {
		runtimeCaptureAddr = runtimeAlias + "." + runtimeCaptureAddr
	}
	for _, capture := range activeCaptures {
		switch capture.kind {
		case captureConstant:
			if capture.typed {
				fmt.Fprintf(&file, "const %s %s = %s\n", capture.name, capture.typeExpr, capture.literal)
			} else {
				fmt.Fprintf(&file, "const %s = %s\n", capture.name, capture.literal)
			}
		case captureTypeAlias:
			file.WriteString(capture.declaration)
			file.WriteByte('\n')
		}
	}
	captureIndex := 0
	for _, capture := range activeCaptures {
		if capture.kind != captureValue {
			continue
		}
		fmt.Fprintf(&file, "%s := (*%s)(%s(%s, %d))\n", captureLocals[capture.name], capture.typeExpr, runtimeCaptureAddr, ownerName, captureIndex)
		fmt.Fprintf(&file, "_ = %s\n", captureLocals[capture.name])
		captureIndex++
	}
	file.WriteString("{\n")
	file.WriteString("//line jit-snippet.go:1\n")
	file.WriteString(rewrittenSource)
	file.WriteString("\n}\n}\n")
	scopeFiles, scopeImports, err := packageDeclarationFiles(m)
	if err != nil {
		return CompiledPackage{}, err
	}

	packageExports := make(map[string]PackageExportData, len(m.Build.Packages))
	for _, pkg := range m.Build.Packages {
		packageExports[pkg.Path] = PackageExportData{
			PackagePath:       pkg.Path,
			PackageName:       pkg.Name,
			ObjectHeader:      pkg.ObjectHeader,
			ExportDataVersion: pkg.ExportDataVersion,
			Fingerprint:       pkg.Fingerprint,
			Data:              pkg.Data,
		}
	}
	for path, used := range usedImports {
		if used && len(packageExports[path].Data) == 0 {
			return CompiledPackage{}, fmt.Errorf("jit: no embedded compiler export data for imported package %q", path)
		}
	}
	importMap := make(map[string]string, len(usedImports))
	for sourcePath, importPath := range scopeImports {
		importMap[sourcePath] = importPath
	}
	for _, imp := range ctx.Imports {
		if usedImports[imp.Path] {
			importMap[imp.SourcePath] = imp.Path
		}
	}
	for path := range captureImportAliases {
		if _, exists := importMap[path]; !exists {
			importMap[path] = path
		}
	}
	if !hasRuntimeImport {
		importMap[runtimePath] = "runtime"
	}
	files := append(scopeFiles, SourceFile{Name: "jit_eval.go", Data: file.String()})
	options := []string{"-l=1"}
	if m.Build.GoVersion != "" {
		options = append(options, "-lang="+m.Build.GoVersion)
	}
	if m.Build.Checkptr != 0 {
		options = append(options, fmt.Sprintf("-d=checkptr=%d", m.Build.Checkptr))
	}
	compiled, err := compileWithJITCallSite(CompileRequest{
		// Keep the package identity from the Eval call site. A distinct
		// generated function name separates this module's symbols from other
		// evaluations without inventing a second package path.
		PackagePath:      m.Build.PackagePath,
		Files:            files,
		PackageExports:   packageExports,
		ImportMap:        importMap,
		Options:          options,
		ArchFeaturesJSON: m.Build.ArchFeatures,
		ownerAware:       true,
		pinRequired:      pinRequired,
		// The custom in-memory linker consumes ordinary compiler object files;
		// it does not ask cmd/link to produce a plugin module.
	}, fileName)
	if err != nil {
		return CompiledPackage{}, err
	}
	compiled.EntryName = entryName
	compiled.ownerAware = true
	compiled.captures = capturePointers
	return compiled, nil
}

func rewriteSnippetOwnerCapture(source, entryName string, reserved map[string]bool, captureBases map[string]string, candidateCaptures map[string]bool) (ownerName, keepAliveName, rewritten string, captureLocals map[string]string, captureUses map[string]bool, pinRequired bool, err error) {
	prefix := "package jit_snippet\nfunc _jit_snippet() {\n"
	wrapped := prefix + source + "\n}\n"
	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, "jit_snippet.go", wrapped, parser.AllErrors)
	if parseErr != nil {
		return "", "", "", nil, nil, false, fmt.Errorf("jit: parse snippet for owner capture: %w", parseErr)
	}
	used := make(map[string]bool, len(reserved))
	for name := range reserved {
		used[name] = true
	}
	type sourceEdit struct {
		start, end int
		text       string
		kind       uint8
	}
	var edits []sourceEdit
	selectorNames := make(map[*ast.Ident]bool)
	ambiguousKeys := make(map[*ast.Ident]bool)
	literalKeys := make(map[*ast.Ident]bool)
	nonReferences := make(map[*ast.Ident]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			selectorNames[node.Sel] = true
		case *ast.Field:
			for _, name := range node.Names {
				nonReferences[name] = true
			}
		case *ast.FuncDecl:
			nonReferences[node.Name] = true
		case *ast.TypeSpec:
			nonReferences[node.Name] = true
		case *ast.ValueSpec:
			for _, name := range node.Names {
				nonReferences[name] = true
			}
		case *ast.LabeledStmt:
			nonReferences[node.Label] = true
		case *ast.BranchStmt:
			if node.Label != nil {
				nonReferences[node.Label] = true
			}
		case *ast.CompositeLit:
			_, isMap := node.Type.(*ast.MapType)
			for _, element := range node.Elts {
				keyValue, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				ident, ok := keyValue.Key.(*ast.Ident)
				if ok && ident.Obj == nil && !isMap {
					literalKeys[ident] = true
					if captureBases[ident.Name] != "" {
						ambiguousKeys[ident] = true
					}
				}
			}
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			used[ident.Name] = true
		}
		return true
	})
	captureLocals = make(map[string]string, len(captureBases))
	captureUses = make(map[string]bool)
	variableNames := make([]string, 0, len(captureBases))
	for name := range captureBases {
		variableNames = append(variableNames, name)
	}
	sort.Strings(variableNames)
	for _, name := range variableNames {
		local := uniqueSnippetName(entryName+captureBases[name], used)
		used[local] = true
		captureLocals[name] = local
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			used[node.Name] = true
			if _, isSelectorName := selectorNames[node]; !isSelectorName &&
				!nonReferences[node] && node.Obj == nil {
				if candidateCaptures[node.Name] && !literalKeys[node] {
					captureUses[node.Name] = true
				}
				if local := captureLocals[node.Name]; local != "" {
					start := fset.Position(node.Pos()).Offset - len(prefix)
					end := fset.Position(node.End()).Offset - len(prefix)
					if start < 0 || end < start || end > len(source) {
						err = fmt.Errorf("jit: captured identifier position is outside snippet source")
						return false
					}
					edits = append(edits, sourceEdit{start: start, end: end, text: "(*" + local + ")", kind: 3})
				}
			}
		case *ast.KeyValueExpr:
			if ident, ok := node.Key.(*ast.Ident); ok && ambiguousKeys[ident] {
				err = fmt.Errorf("jit: captured name %q occurs as an ambiguous composite literal key", ident.Name)
				return false
			}
		case *ast.FuncLit:
			pos := fset.Position(node.Body.Lbrace).Offset + 1 - len(prefix)
			if pos < 0 || pos > len(source) {
				err = fmt.Errorf("jit: function literal position is outside snippet source")
				return false
			}
			edits = append(edits, sourceEdit{start: pos, end: pos, kind: 1})
		}
		return true
	})
	if err != nil {
		return "", "", "", nil, nil, false, err
	}
	ownerName = uniqueSnippetName(entryName+"_owner", used)
	used[ownerName] = true
	keepAliveName = uniqueSnippetName(entryName+"_keepAlive", used)
	used[keepAliveName] = true
	for i := range edits {
		switch edits[i].kind {
		case 1:
			edits[i].text = "defer " + keepAliveName + "(" + ownerName + ")\n"
		case 3:
			// The replacement was assembled when the free identifier was found.
		}
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end > edits[j].end
		}
		return edits[i].start > edits[j].start
	})
	for _, edit := range edits {
		source = source[:edit.start] + edit.text + source[edit.end:]
	}
	return ownerName, keepAliveName, source, captureLocals, captureUses, pinRequired, nil
}

func uniqueSnippetName(base string, used map[string]bool) string {
	name := base
	for suffix := 0; used[name]; suffix++ {
		name = fmt.Sprintf("%s_%d", base, suffix+1)
	}
	return name
}

func snippetRuntimeImport(ctx PackageContext, entryName string) (alias string, dot bool, sourcePath string, exists bool) {
	uniqueAlias := func(base string) string {
		used := make(map[string]bool, len(ctx.Imports))
		for _, imp := range ctx.Imports {
			if imp.Alias != "" && imp.Alias != "." && imp.Alias != "_" {
				used[imp.Alias] = true
			}
		}
		return uniqueSnippetName(base, used)
	}
	for _, imp := range ctx.Imports {
		if imp.Path != "runtime" {
			continue
		}
		sourcePath = imp.SourcePath
		switch imp.Alias {
		case ".":
			return "", true, sourcePath, true
		case "_":
			return uniqueAlias(entryName + "_runtime"), false, sourcePath, true
		case "":
			return "runtime", false, sourcePath, true
		default:
			return imp.Alias, false, sourcePath, true
		}
	}
	return uniqueAlias(entryName + "_runtime"), false, "runtime", false
}

// packageDeclarationFiles emits bodyless declarations in their original file
// import scopes. These declarations give the in-process compiler the same
// package namespace as the Eval call site; the linker later binds any
// referenced package symbol to the already-linked host symbol.
func packageDeclarationFiles(metadata *Metadata) ([]SourceFile, map[string]string, error) {
	importsByPath := make(map[string]string, len(metadata.Build.Packages))
	for _, pkg := range metadata.Build.Packages {
		importsByPath[pkg.Path] = pkg.Name
	}
	linkedNames := make(map[string]map[string]bool)
	for _, symbol := range metadata.Link.Symbols {
		if strings.Contains(symbol.Name, ".") {
			continue
		}
		if linkedNames[symbol.PackagePath] == nil {
			linkedNames[symbol.PackagePath] = make(map[string]bool)
		}
		linkedNames[symbol.PackagePath][symbol.Name] = true
	}
	declaredNames := make(map[string]bool)
	declarationsByFile := make(map[string][]Declaration)
	for _, declaration := range metadata.Build.Declarations {
		for _, name := range declaration.Names {
			declaredNames[name] = true
		}
		if declaration.Kind == "func" && len(declaration.Names) == 1 && declaration.Names[0] == "init" {
			// A package init function is not part of Eval scope and must not be
			// executed a second time by a generated package.
			continue
		}
		declarationsByFile[declaration.File] = append(declarationsByFile[declaration.File], declaration)
	}
	files := make([]SourceFile, 0, len(declarationsByFile))
	importMap := make(map[string]string)
	for _, sourceFile := range metadata.Build.Files {
		declarations := declarationsByFile[sourceFile.Name]
		if len(declarations) == 0 {
			continue
		}
		selectors := make(map[string]bool)
		identifiers := make(map[string]bool)
		fset := token.NewFileSet()
		for i, declaration := range declarations {
			parsed, err := parser.ParseFile(fset, fmt.Sprintf("jit-scope-%d.go", i), "package jit_scope\n"+declaration.Source, parser.AllErrors)
			if err != nil {
				return nil, nil, fmt.Errorf("jit: parse saved package declaration in %q: %w", sourceFile.Name, err)
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok {
					identifiers[ident.Name] = true
				}
				if selector, ok := node.(*ast.SelectorExpr); ok {
					if ident, ok := selector.X.(*ast.Ident); ok {
						selectors[ident.Name] = true
					}
				}
				return true
			})
		}

		var selected []Import
		for _, imp := range sourceFile.Imports {
			if imp.Alias == "_" {
				continue
			}
			alias := imp.Alias
			if alias == "" {
				alias = importsByPath[imp.Path]
				if alias == "" {
					alias = path.Base(imp.Path)
				}
			}
			used := false
			if alias == "." {
				for name := range linkedNames[imp.Path] {
					if identifiers[name] && !declaredNames[name] {
						used = true
						break
					}
				}
			} else {
				used = selectors[alias]
			}
			if !used {
				continue
			}
			selected = append(selected, imp)
			importMap[imp.SourcePath] = imp.Path
		}

		var source strings.Builder
		fmt.Fprintf(&source, "package %s\n", metadata.Build.PackageName)
		if len(selected) != 0 {
			source.WriteString("import (\n")
			for _, imp := range selected {
				if imp.Alias != "" {
					source.WriteString(imp.Alias)
					source.WriteByte(' ')
				}
				source.WriteString(strconv.Quote(imp.SourcePath))
				source.WriteByte('\n')
			}
			source.WriteString(")\n")
		}
		for _, declaration := range declarations {
			source.WriteString(declaration.Source)
			source.WriteByte('\n')
		}
		name := "jit_scope_" + path.Base(sourceFile.Name)
		files = append(files, SourceFile{Name: name, Data: source.String()})
	}
	for filename := range declarationsByFile {
		found := false
		for _, sourceFile := range metadata.Build.Files {
			if sourceFile.Name == filename {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("jit: saved declarations refer to unknown source file %q", filename)
		}
	}
	return files, importMap, nil
}

func nextSnippetFunctionName(m *Metadata) (string, error) {
	for {
		id, ok := allocateSnippetFunctionID()
		if !ok {
			return "", fmt.Errorf("jit: exhausted unique snippet function IDs")
		}
		name := fmt.Sprintf("__jit_eval_%d", id)
		declared := false
		if m != nil {
			for _, decl := range m.Build.Declarations {
				for _, candidate := range decl.Names {
					if candidate == name {
						declared = true
						break
					}
				}
				if declared {
					break
				}
			}
		}
		if !declared {
			return name, nil
		}
	}
}

func allocateSnippetFunctionID() (uint64, bool) {
	for {
		current := nextSnippetFunctionID.Load()
		if current == ^uint64(0) {
			return 0, false
		}
		next := current + 1
		if nextSnippetFunctionID.CompareAndSwap(current, next) {
			return next, true
		}
	}
}

func snippetImports(source string, ctx PackageContext, packages []PackageExport) (map[string]bool, error) {
	fset := token.NewFileSet()
	wrapped := "package jit_snippet\nfunc _jit_snippet() {\n" + source + "\n}\n"
	file, err := parser.ParseFile(fset, "jit_snippet.go", wrapped, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("jit: parse snippet for import selection: %w", err)
	}
	packageNames := make(map[string]string, len(packages))
	for _, pkg := range packages {
		packageNames[pkg.Path] = pkg.Name
	}
	aliasPaths := make(map[string]string, len(ctx.Imports))
	used := make(map[string]bool)
	for _, imp := range ctx.Imports {
		if imp.Alias == "_" {
			continue
		}
		if imp.Alias == "." {
			used[imp.Path] = true
			continue
		}
		alias := imp.Alias
		if alias == "" {
			alias = packageNames[imp.Path]
		}
		if alias != "" {
			aliasPaths[alias] = imp.Path
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if ok && ident.Obj == nil {
			if path := aliasPaths[ident.Name]; path != "" {
				used[path] = true
			}
		}
		return true
	})
	return used, nil
}
