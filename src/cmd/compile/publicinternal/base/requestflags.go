// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"cmd/publicinternal/cov/covcmd"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"cmd/publicinternal/sys"
	"cmd/publicinternal/telemetry/counter"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"publicinternal/buildcfg"
	"publicinternal/platform"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

// CompileConfig contains parsed compiler settings owned by one compilation.
// Unlike ParseFlagsWithDebugSSA, ParseCompileConfig does not read or write the
// package-level Flag, Debug, Ctxt, hash-debug settings, os.Args, or the default
// flag set.
type CompileConfig struct {
	Flags CmdFlags
	Debug DebugFlags
	// Environment contains this compilation's inherited environment and -env
	// overrides. It is never applied to the process environment.
	Environment map[string]string

	GossaHash        *HashDebug
	ConvertHash      *HashDebug
	FmaHash          *HashDebug
	LoopVarHash      *HashDebug
	PGOHash          *HashDebug
	LiteralAllocHash *HashDebug
	MergeLocalsHash  *HashDebug
	VariableMakeHash *HashDebug

	err error
}

// ParseCompileConfig parses compiler options into state associated with ctxt.
// args must contain compiler flags only; source files and output paths are
// supplied separately by the request API. out receives opt-in hash-debug logs.
func ParseCompileConfig(goos string, ctxt *obj.Link, args []string, debugSSA func(phase, flag string, val int, valString string) string, out io.Writer) (*CompileConfig, error) {
	cfg, _, err := parseCompileConfig(goos, ctxt, args, debugSSA, out, "", false)
	return cfg, err
}

// ParseCommandLineConfig parses the compiler command line into target-owned
// configuration. It returns the source file names left after flag parsing.
func ParseCommandLineConfig(goos string, ctxt *obj.Link, args []string, debugSSA func(phase, flag string, val int, valString string) string, out io.Writer, toolName string) (*CompileConfig, []string, error) {
	return parseCompileConfig(goos, ctxt, args, debugSSA, out, toolName, true)
}

func parseCompileConfig(goos string, ctxt *obj.Link, args []string, debugSSA func(phase, flag string, val int, valString string) string, out io.Writer, toolName string, commandLine bool) (*CompileConfig, []string, error) {
	if ctxt == nil {
		return nil, nil, fmt.Errorf("nil compiler link context")
	}
	if out == nil {
		out = io.Discard
	}

	cfg := new(CompileConfig)
	flags, debug := &cfg.Flags, &cfg.Debug
	flags.I = func(dir string) {
		if dir != "" {
			flags.Cfg.ImportDirs = append(flags.Cfg.ImportDirs, dir)
		}
	}
	flags.LowerC = runtime.GOMAXPROCS(0)
	flags.LowerD = objabi.NewDebugFlag(debug, debugSSA)
	flags.LowerP = &ctxt.Pkgpath
	flags.LowerV = &ctxt.Debugvlog
	flags.Dwarf = ctxt.Arch.Name != "wasm"
	flags.DwarfBASEntries = &ctxt.UseBASEntries
	flags.DwarfLocationLists = &ctxt.Flag_locationlists
	*flags.DwarfLocationLists = true
	flags.Dynlink = &ctxt.Flag_dynlink
	flags.EmbedCfg = func(file string) {
		if err := readEmbedCfgFor(flags, file); err != nil {
			cfg.err = err
		}
	}
	cfg.Environment = make(map[string]string)
	if commandLine {
		for _, entry := range os.Environ() {
			if key, value, ok := strings.Cut(entry, "="); ok {
				cfg.Environment[key] = value
			}
		}
	}
	flags.Env = func(s string) {
		key, value, ok := strings.Cut(s, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			cfg.err = fmt.Errorf("-env argument must be of the form key=value")
			return
		}
		cfg.Environment[key] = value
	}
	flags.GenDwarfInl = 2
	flags.ImportCfg = func(file string) {
		if err := readImportCfgFor(flags, file); err != nil {
			cfg.err = err
		}
	}
	flags.CoverageCfg = func(file string) {
		if err := readCoverageCfgFor(flags, file); err != nil {
			cfg.err = err
		}
	}
	flags.LinkShared = &ctxt.Flag_linkshared
	flags.Shared = &ctxt.Flag_shared
	flags.WB = true
	flags.Cfg.ImportMap = make(map[string]string)

	debug.ConcurrentOk = true
	debug.CompressInstructions = 1
	debug.MaxShapeLen = 500
	debug.AlignHot = 1
	debug.InlFuncsWithClosures = 1
	debug.InlStaticInit = 1
	debug.FreeAppend = 1
	debug.PGOInline = 1
	debug.PGODevirtualize = 2
	debug.SyncFrames = -1
	debug.VariableMakeThreshold = 32
	debug.ZeroCopy = 1
	debug.RangeFuncCheck = 1
	debug.MergeLocals = 1
	debug.RewriteResults = 1
	debug.Checkptr = -1

	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if commandLine {
		fs.SetOutput(out)
		fs.Usage = func() {
			fmt.Fprintln(out, "usage: compile [options] file.go...")
			fs.PrintDefaults()
		}
		objabi.AddVersionFlagTo(fs, toolName, os.Exit)
	}
	if err := registerFlagsFor(fs, flags); err != nil {
		return nil, nil, err
	}
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if cfg.err != nil {
		return nil, nil, cfg.err
	}
	if !commandLine && fs.NArg() != 0 {
		return nil, nil, fmt.Errorf("compiler request options must not include source filenames")
	}
	if gcd := cfg.Environment["GOCOMPILEDEBUG"]; gcd != "" {
		if err := flags.LowerD.Set(gcd); err != nil {
			return nil, nil, err
		}
	}
	if commandLine {
		counter.CountFlags("compile/flag:", *fs)
	}
	if err := finalizeCompileConfig(goos, cfg, ctxt, out); err != nil {
		return nil, nil, err
	}
	return cfg, fs.Args(), nil
}

type requestCountFlag struct{ value *CountFlag }

func (c requestCountFlag) String() string { return strconv.Itoa(int(*c.value)) }
func (c requestCountFlag) Set(s string) error {
	switch s {
	case "true":
		*c.value++
	case "false":
		*c.value = 0
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("invalid count %q", s)
		}
		*c.value = CountFlag(n)
	}
	return nil
}
func (c requestCountFlag) IsBoolFlag() bool  { return true }
func (c requestCountFlag) IsCountFlag() bool { return true }

type requestFuncFlag func(string)

func (f requestFuncFlag) String() string { return "" }
func (f requestFuncFlag) Set(s string) error {
	f(s)
	return nil
}

func registerFlagsFor(fs *flag.FlagSet, flags *CmdFlags) error {
	boolType := reflect.TypeFor[bool]()
	intType := reflect.TypeFor[int]()
	stringType := reflect.TypeFor[string]()
	ptrBoolType := reflect.TypeFor[*bool]()
	ptrIntType := reflect.TypeFor[*int]()
	ptrStringType := reflect.TypeFor[*string]()
	countType := reflect.TypeFor[CountFlag]()
	funcType := reflect.TypeFor[func(string)]()

	v := reflect.ValueOf(flags).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Name == "Cfg" {
			continue
		}

		name := flagName(f)
		help := f.Tag.Get("help")
		if help == "" {
			return fmt.Errorf("base.CmdFlags.%s is missing help text", f.Name)
		}
		field := v.Field(i)
		if (f.Type.Kind() == reflect.Ptr || f.Type.Kind() == reflect.Func) && field.IsNil() {
			return fmt.Errorf("base.CmdFlags.%s is uninitialized %v", f.Name, f.Type)
		}

		switch f.Type {
		case boolType:
			p := field.Addr().Interface().(*bool)
			fs.BoolVar(p, name, *p, help)
		case intType:
			p := field.Addr().Interface().(*int)
			fs.IntVar(p, name, *p, help)
		case stringType:
			p := field.Addr().Interface().(*string)
			fs.StringVar(p, name, *p, help)
		case ptrBoolType:
			p := field.Interface().(*bool)
			fs.BoolVar(p, name, *p, help)
		case ptrIntType:
			p := field.Interface().(*int)
			fs.IntVar(p, name, *p, help)
		case ptrStringType:
			p := field.Interface().(*string)
			fs.StringVar(p, name, *p, help)
		case countType:
			fs.Var(requestCountFlag{field.Addr().Interface().(*CountFlag)}, name, help)
		case funcType:
			fs.Var(requestFuncFlag(field.Interface().(func(string))), name, help)
		default:
			if value, ok := field.Interface().(flag.Value); ok {
				fs.Var(value, name, help)
			} else {
				return fmt.Errorf("base.CmdFlags.%s has unexpected type %s", f.Name, f.Type)
			}
		}
	}
	return nil
}

func flagName(f reflect.StructField) string {
	if tag := f.Tag.Get("flag"); tag != "" {
		return tag
	}
	if len(f.Name) == 1 {
		return f.Name
	}
	if len(f.Name) == 6 && f.Name[:5] == "Lower" && 'A' <= f.Name[5] && f.Name[5] <= 'Z' {
		return string(rune(f.Name[5] + 'a' - 'A'))
	}
	return strings.ToLower(f.Name)
}

func finalizeCompileConfig(goos string, cfg *CompileConfig, ctxt *obj.Link, out io.Writer) error {
	flags, debug := &cfg.Flags, &cfg.Debug
	if err := ctxt.SetFIPSDebugHash(debug.FIPSHash); err != nil {
		return err
	}
	newHashDebug := func(ev, selector string) (*HashDebug, error) {
		return NewHashDebugWithError(ev, selector, out)
	}
	var err error
	if cfg.GossaHash, err = newHashDebug("gossahash", debug.Gossahash); err != nil {
		return err
	}
	if flags.Std && objabi.LookupPkgSpecial(ctxt.Pkgpath).Runtime {
		flags.CompilingRuntime = true
	}
	ctxt.Std = flags.Std

	if debug.LoopVarHash != "" {
		mostInlineOnly := true
		if strings.HasPrefix(debug.LoopVarHash, "IL") {
			debug.LoopVarHash = debug.LoopVarHash[2:]
			mostInlineOnly = false
		}
		if cfg.LoopVarHash, err = newHashDebug("loopvarhash", debug.LoopVarHash); err != nil {
			return err
		}
		if debug.LoopVar < 11 {
			debug.LoopVar = 1
		}
		cfg.LoopVarHash.SetInlineSuffixOnly(mostInlineOnly)
	} else if buildcfg.Experiment.LoopVar && debug.LoopVar == 0 {
		debug.LoopVar = 1
	}

	if debug.Converthash != "" {
		if cfg.ConvertHash, err = newHashDebug("converthash", debug.Converthash); err != nil {
			return err
		}
	} else {
		if cfg.ConvertHash, err = newHashDebug("converthash", "qn"); err != nil {
			return err
		}
	}
	if debug.Fmahash != "" {
		if cfg.FmaHash, err = newHashDebug("fmahash", debug.Fmahash); err != nil {
			return err
		}
	}
	if debug.PGOHash != "" {
		if cfg.PGOHash, err = newHashDebug("pgohash", debug.PGOHash); err != nil {
			return err
		}
	}
	if debug.LiteralAllocHash != "" {
		if cfg.LiteralAllocHash, err = newHashDebug("literalalloc", debug.LiteralAllocHash); err != nil {
			return err
		}
	}
	if debug.MergeLocalsHash != "" {
		if cfg.MergeLocalsHash, err = newHashDebug("mergelocals", debug.MergeLocalsHash); err != nil {
			return err
		}
	}
	if debug.VariableMakeHash != "" {
		if cfg.VariableMakeHash, err = newHashDebug("variablemake", debug.VariableMakeHash); err != nil {
			return err
		}
	}

	if flags.MSan && !platform.MSanSupported(goos, ctxt.Arch.Name) {
		return fmt.Errorf("%s/%s does not support -msan", goos, ctxt.Arch.Name)
	}
	if flags.ASan && !platform.ASanSupported(goos, ctxt.Arch.Name) {
		return fmt.Errorf("%s/%s does not support -asan", goos, ctxt.Arch.Name)
	}
	if flags.Race && !platform.RaceDetectorSupported(goos, ctxt.Arch.Name) {
		return fmt.Errorf("%s/%s does not support -race", goos, ctxt.Arch.Name)
	}
	if (*flags.Shared || *flags.Dynlink || *flags.LinkShared) && !ctxt.Arch.InFamily(sys.AMD64, sys.ARM, sys.ARM64, sys.I386, sys.Loong64, sys.MIPS64, sys.PPC64, sys.RISCV64, sys.S390X) {
		return fmt.Errorf("%s/%s does not support -shared", goos, ctxt.Arch.Name)
	}
	if err := parseSpectreFor(flags, ctxt); err != nil {
		return err
	}

	ctxt.CompressInstructions = debug.CompressInstructions != 0
	ctxt.Flag_shared = ctxt.Flag_dynlink || ctxt.Flag_shared
	ctxt.Flag_optimize = flags.N == 0
	ctxt.Debugasm = int(flags.S)
	ctxt.Flag_maymorestack = debug.MayMoreStack
	ctxt.Flag_noRefName = debug.NoRefName != 0

	if *flags.LowerP == "" {
		*flags.LowerP = obj.UnlinkablePkg
	}
	if flags.GoVersion != "" && flags.GoVersion != runtime.Version() {
		return fmt.Errorf("compile: version %q does not match go tool version %q", runtime.Version(), flags.GoVersion)
	}
	if flags.Race && flags.MSan {
		return fmt.Errorf("cannot use both -race and -msan")
	}
	if flags.Race && flags.ASan {
		return fmt.Errorf("cannot use both -race and -asan")
	}
	if flags.MSan && flags.ASan {
		return fmt.Errorf("cannot use both -msan and -asan")
	}
	if flags.Race || flags.MSan || flags.ASan {
		if debug.Checkptr == -1 {
			debug.Checkptr = 1
		}
	}
	if flags.LowerC < 1 {
		return fmt.Errorf("-c must be at least 1, got %d", flags.LowerC)
	}
	if len(flags.Cfg.ImportDirs) > 0 && flags.Cfg.PackageFile != nil {
		return fmt.Errorf("cannot use both -I and -importcfg")
	}
	if debug.Checkptr == -1 {
		debug.Checkptr = 0
	}
	if ctxt.Headtype == objabi.Hplan9 && ctxt.Arch.Name == "386" {
		debug.AlignHot = 0
	}
	if !concurrentBackendAllowedFor(flags, debug, ctxt) {
		flags.LowerC = 1
	}
	if flags.CompilingRuntime {
		flags.N = 0
		ctxt.Flag_optimize = true
		debug.Checkptr = 0
		debug.Libfuzzer = 0
	}
	ctxt.Debugpcln = debug.PCTab
	return nil
}

func concurrentBackendAllowedFor(flags *CmdFlags, debug *DebugFlags, ctxt *obj.Link) bool {
	if flags.Percent != 0 || flags.E != 0 || flags.K != 0 || flags.L != 0 || flags.LowerJ != 0 || flags.LowerM != 0 || flags.LowerR != 0 {
		return false
	}
	if ctxt.Debugvlog || !debug.ConcurrentOk || flags.Live > 0 || buildcfg.Experiment.FieldTrack || ctxt.Flag_dynlink || flags.Race {
		return false
	}
	return true
}

func readImportCfgFor(flags *CmdFlags, file string) error {
	if flags.Cfg.ImportMap == nil {
		flags.Cfg.ImportMap = make(map[string]string)
	}
	flags.Cfg.PackageFile = make(map[string]string)
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("-importcfg: %w", err)
	}
	for lineNum, line := range strings.Split(string(data), "\n") {
		lineNum++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verb, args, found := strings.Cut(line, " ")
		if found {
			args = strings.TrimSpace(args)
		}
		before, after, hasEq := strings.Cut(args, "=")
		switch verb {
		case "importmap":
			if !hasEq || before == "" || after == "" {
				return fmt.Errorf("%s:%d: invalid importmap: syntax is \"importmap old=new\"", file, lineNum)
			}
			flags.Cfg.ImportMap[before] = after
		case "packagefile":
			if !hasEq || before == "" || after == "" {
				return fmt.Errorf("%s:%d: invalid packagefile: syntax is \"packagefile path=filename\"", file, lineNum)
			}
			flags.Cfg.PackageFile[before] = after
		default:
			return fmt.Errorf("%s:%d: unknown directive %q", file, lineNum, verb)
		}
	}
	return nil
}

func readCoverageCfgFor(flags *CmdFlags, file string) error {
	var cfg covcmd.CoverFixupConfig
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("-coveragecfg: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("error reading -coveragecfg file %q: %w", file, err)
	}
	flags.Cfg.CoverageInfo = &cfg
	return nil
}

func readEmbedCfgFor(flags *CmdFlags, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("-embedcfg: %w", err)
	}
	if err := json.Unmarshal(data, &flags.Cfg.Embed); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if flags.Cfg.Embed.Patterns == nil || flags.Cfg.Embed.Files == nil {
		return fmt.Errorf("%s: invalid embedcfg: missing Patterns or Files", file)
	}
	return nil
}

func parseSpectreFor(flags *CmdFlags, ctxt *obj.Link) error {
	for f := range strings.SplitSeq(flags.Spectre, ",") {
		f = strings.TrimSpace(f)
		switch f {
		case "":
		case "all":
			flags.Cfg.SpectreIndex = true
			ctxt.Retpoline = true
		case "index":
			flags.Cfg.SpectreIndex = true
		case "ret":
			ctxt.Retpoline = true
		default:
			return fmt.Errorf("unknown setting -spectre=%s", f)
		}
	}
	if flags.Cfg.SpectreIndex && ctxt.Arch.Name != "amd64" {
		return fmt.Errorf("GOARCH=%s does not support -spectre=index", ctxt.Arch.Name)
	}
	return nil
}
