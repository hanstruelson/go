// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"bufio"
	"bytes"
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/bloop"
	"cmd/compile/publicinternal/coverage"
	"cmd/compile/publicinternal/deadlocals"
	"cmd/compile/publicinternal/dwarfgen"
	"cmd/compile/publicinternal/escape"
	"cmd/compile/publicinternal/inline/interleaved"
	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/logopt"
	"cmd/compile/publicinternal/loopvar"
	"cmd/compile/publicinternal/noder"
	"cmd/compile/publicinternal/pgoir"
	"cmd/compile/publicinternal/pkginit"
	"cmd/compile/publicinternal/reflectdata"
	"cmd/compile/publicinternal/rewriteresults"
	"cmd/compile/publicinternal/rttype"
	"cmd/compile/publicinternal/slice"
	"cmd/compile/publicinternal/ssacompile"
	"cmd/compile/publicinternal/ssagen"
	"cmd/compile/publicinternal/staticinit"
	"cmd/compile/publicinternal/stats"
	"cmd/compile/publicinternal/typecheck"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/dwarf"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"cmd/publicinternal/src"
	"cmd/publicinternal/telemetry/counter"
	"flag"
	"fmt"
	"io"
	"os"
	"publicinternal/buildcfg"
	"runtime"
	"strconv"
	"strings"
)

// handlePanic ensures that we print out an "internal compiler error" for any panic
// or runtime exception during front-end compiler processing (unless there have
// already been some compiler errors). It may also be invoked from the explicit panic in
// hcrash(), in which case, we pass the panic on through.
func handlePanic(target *ir.Package, reusable bool) {
	ir.CloseHTMLWriters(target)
	noder.CloseHTMLWriters(target)
	if err := recover(); err != nil {
		if _, ok := err.(base.ExitError); ok {
			panic(err)
		}
		if err == "-h" {
			// Force real panic now with -h option (hcrash) - the error
			// information will have already been printed.
			panic(err)
		}
		if target != nil && target.Diagnostics != nil {
			target.Diagnostics.Fatalf("panic: %v", err)
		}
		if reusable {
			panic(err)
		}
		fmt.Fprintf(os.Stderr, "compile: panic before compiler context initialization: %v\n", err)
		os.Exit(2)
	}
}

// Main parses flags and Go source files specified in the command-line
// arguments, type-checks the parsed Go package, compiles functions to machine
// code, and finally writes the compiled package definition to disk.
// Main runs the command-line compiler using arguments copied by the process
// entry point. Parsing and compilation state is owned by this invocation.
func Main(archInit func(*ssagen.ArchInfo), toolName string, args []string) {
	args, err := objabi.ExpandArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile: %v\n", err)
		os.Exit(2)
	}
	mainWithConfig(archInit, nil, false, nil, "", os.Stdout, nil, &commandLine{toolName: toolName, args: args})
}

func targetArchFeatures(goarch string, useProcessConfig bool) ir.ArchFeatures {
	if useProcessConfig {
		return ir.ArchFeatures{
			GO386: buildcfg.GO386, GOAMD64: buildcfg.GOAMD64,
			GOARMVersion: buildcfg.GOARM.Version, GOARMSoftFloat: buildcfg.GOARM.SoftFloat,
			GOARM64Version: buildcfg.GOARM64.Version, GOARM64LSE: buildcfg.GOARM64.LSE, GOARM64Crypto: buildcfg.GOARM64.Crypto,
			GOMIPS: buildcfg.GOMIPS, GOMIPS64: buildcfg.GOMIPS64,
			GOPPC64: buildcfg.GOPPC64, GORISCV64: buildcfg.GORISCV64,
			GOWASM: buildcfg.GOWASM.String(),
		}
	}

	var features ir.ArchFeatures
	switch goarch {
	case "386":
		features.GO386 = buildcfg.DefaultGO386
	case "amd64":
		features.GOAMD64, _ = strconv.Atoi(strings.TrimPrefix(buildcfg.DefaultGOAMD64, "v"))
	case "arm":
		arm := buildcfg.DefaultGOARM
		features.GOARMVersion, _ = strconv.Atoi(arm[:1])
		features.GOARMSoftFloat = features.GOARMVersion == 5 || strings.Contains(arm, "softfloat")
	case "arm64":
		arm64, _ := buildcfg.ParseGoarm64(buildcfg.DefaultGOARM64)
		features.GOARM64Version = arm64.Version
		features.GOARM64LSE = arm64.LSE
		features.GOARM64Crypto = arm64.Crypto
	case "mips", "mipsle":
		features.GOMIPS = buildcfg.DefaultGOMIPS
	case "mips64", "mips64le":
		features.GOMIPS64 = buildcfg.DefaultGOMIPS64
	case "ppc64", "ppc64le":
		features.GOPPC64, _ = strconv.Atoi(strings.TrimPrefix(buildcfg.DefaultGOPPC64, "power"))
	case "riscv64":
		value := strings.TrimPrefix(buildcfg.DefaultGORISCV64, "rva")
		value = strings.TrimSuffix(value, "u64")
		features.GORISCV64, _ = strconv.Atoi(value)
	}
	return features
}

type commandLine struct {
	toolName string
	args     []string
}

func mainWithConfig(archInit func(*ssagen.ArchInfo), sourceFiles []noder.SourceFile, reusable bool, request *Request, outputPath string, output io.Writer, onTarget func(*ir.Package), command *commandLine) {
	var compilequeue []*ir.Func
	var target *ir.Package
	var commandFiles []string
	timer := &base.Timings{}
	backend := ssagen.NewContext()
	if output == nil {
		output = os.Stdout
	}
	timer.Start("fe", "init")
	if !reusable {
		counter.Open()
		counter.Inc("compile/invocations")
	}

	defer func() { handlePanic(target, reusable) }()

	arch := new(ssagen.ArchInfo)
	if request != nil {
		arch.TargetArch = request.GOARCH
	} else {
		arch.TargetArch = buildcfg.GOARCH
	}
	arch.TargetFeatures = targetArchFeatures(arch.TargetArch, request == nil || arch.TargetArch == buildcfg.GOARCH)
	if request != nil && request.archFeatures != nil {
		arch.TargetFeatures = *request.archFeatures
	}
	archInit(arch)
	if arch.InitError != nil {
		if reusable {
			fmt.Fprintln(output, arch.InitError)
			panic(base.ExitError(1))
		}
		fmt.Fprintln(os.Stderr, arch.InitError)
		os.Exit(1)
	}
	if arch.LinkArch == nil || arch.LinkArch.Arch == nil {
		fmt.Fprintln(output, "architecture initializer did not select a target architecture")
		panic(base.ExitError(1))
	}
	selectedArch := arch.LinkArch.Arch.Name
	if request != nil && request.GOARCH != "" && request.GOARCH != selectedArch {
		fmt.Fprintf(output, "requested GOARCH %q, but architecture initializer selected %q\n", request.GOARCH, selectedArch)
		panic(base.ExitError(1))
	}
	arch.TargetArch = selectedArch

	target = new(ir.Package)
	if request == nil {
		target.GOOS = buildcfg.GOOS
	} else {
		target.GOOS = runtime.GOOS
	}
	target.GOARCH = selectedArch
	target.ArchFeatures = arch.TargetFeatures
	target.LogOpt = logopt.NewContext(target.GOOS, target.GOARCH)
	target.Diagnostics = base.NewDiagnostics(output)
	target.Diagnostics.PanicOnExit = reusable
	target.ArchInfo = arch
	target.Ctxt = obj.Linknew(arch.LinkArch)
	target.Ctxt.DisableProgReuse = reusable
	target.TypeContext = types.NewContext(arch.LinkArch.PtrSize, arch.LinkArch.RegSize, arch.MAXWIDTH)
	target.TypeContext.Link = target.Ctxt
	target.TypeContext.Diagnostics = target.Diagnostics
	if request != nil {
		target.Ctxt.Pkgpath = request.PackagePath
	}
	target.Ctxt.DiagFunc = target.Diagnostics.Diagf
	target.Ctxt.DiagFlush = target.Diagnostics.FlushErrors
	target.Ctxt.Bso = bufio.NewWriter(output)
	if onTarget != nil {
		onTarget(target)
	}

	// UseBASEntries is preferred because it shaves about 2% off build time, but LLDB, dsymutil, and dwarfdump
	// on Darwin don't support it properly, especially since macOS 10.14 (Mojave).  This is exposed as a flag
	// to allow testing with LLVM tools on Linux, and to help with reporting this bug to the LLVM project.
	// See bugs 31188 and 21945 (CLs 170638, 98075, 72371).
	target.Ctxt.UseBASEntries = target.Ctxt.Headtype != objabi.Hdarwin

	ssaOptions := ssacompile.NewOptions()
	if command != nil {
		options, files, err := base.ParseCommandLineConfig(target.GOOS, target.Ctxt, command.args, ssaOptions.PhaseOption, os.Stderr, command.toolName)
		if err != nil {
			if err == flag.ErrHelp {
				return
			}
			fmt.Fprintf(os.Stderr, "compile: %v\n", err)
			os.Exit(2)
		}
		commandFiles = files
		target.Flags = options.Flags
		target.Debug = options.Debug
		target.Environment = options.Environment
		target.ClobberDeadHash = target.Getenv("GOCLOBBERDEADHASH")
		target.SSADumpDir = target.Getenv("GOSSADIR")
		target.GossaHash = options.GossaHash
		target.PGOHash = options.PGOHash
		target.LoopVarHash = options.LoopVarHash
		target.ConvertHash = options.ConvertHash
		target.FmaHash = options.FmaHash
		target.LiteralAllocHash = options.LiteralAllocHash
		target.VariableMakeHash = options.VariableMakeHash
		target.MergeLocalsHash = options.MergeLocalsHash
	} else if request != nil {
		options, err := base.ParseCompileConfig(target.GOOS, target.Ctxt, request.Options, ssaOptions.PhaseOption, output)
		if err != nil {
			target.Diagnostics.Fatalf("invalid compiler options: %v", err)
		}
		target.Flags = options.Flags
		target.Flags.LowerO = outputPath
		target.Flags.Cfg.PackageExports = request.PackageExports
		target.Flags.Cfg.JITCallSiteFile = request.JITCallSiteFile
		if request.ImportMap != nil {
			target.Flags.Cfg.ImportMap = request.ImportMap
		}
		target.Debug = options.Debug
		target.Environment = options.Environment
		target.GossaHash = options.GossaHash
		target.PGOHash = options.PGOHash
		target.LoopVarHash = options.LoopVarHash
		target.ConvertHash = options.ConvertHash
		target.FmaHash = options.FmaHash
		target.LiteralAllocHash = options.LiteralAllocHash
		target.VariableMakeHash = options.VariableMakeHash
		target.MergeLocalsHash = options.MergeLocalsHash
	}
	target.Diagnostics.Configure(target.Ctxt, target.Flags, target.Debug, output)
	ssaCompiler := ssacompile.ConfigureTarget(target, ssaOptions)

	if !reusable {
		if flagGCStart := target.Debug.GCStart; flagGCStart > 0 || // explicit flags overrides environment variable disable of GC boost
			target.Getenv("GOGC") == "" && target.Getenv("GOMEMLIMIT") == "" && target.Flags.LowerC != 1 { // explicit GC knobs or no concurrency implies default heap
			startHeapMB := int64(128)
			if flagGCStart > 0 {
				startHeapMB = int64(flagGCStart)
			}
			base.AdjustStartingHeap(uint64(startHeapMB)<<20, 0, 0, 0, target.Debug.GCAdjust == 1, target.AddCleanup)
		}
	}

	target.LocalPkg = target.Pkg(target.Ctxt.Pkgpath, "")
	target.LocalPkg.Local = true
	target.TypeSymPkg = target.Pkg("type", "type")

	// pseudo-package, for scoping
	target.BuiltinPkg = target.Pkg("go.builtin", "") // TODO(gri) name this package go.builtin?
	target.BuiltinPkg.Prefix = "go:builtin"

	// pseudo-package, accessed by import "unsafe"
	target.UnsafePkg = target.Pkg("unsafe", "unsafe")

	// Pseudo-package that contains the compiler's builtin
	// declarations for maps.
	target.InternalMapsPkg = target.Pkg("go.publicinternal/runtime/maps", "publicinternal/runtime/maps")
	target.InternalMapsPkg.Prefix = "publicinternal/runtime/maps"

	// pseudo-packages used in symbol tables
	target.ItabPkg = target.Pkg("go.itab", "go.itab")
	target.ItabPkg.Prefix = "go:itab"

	// pseudo-package for use with code coverage instrumentation.
	target.CoveragePkg = target.Pkg("go.coverage", "runtime/coverage")
	target.CoveragePkg.Prefix = "runtime/coverage"

	// Record flags that affect the build result. (And don't
	// record flags that don't, since that would cause spurious
	// changes in the binary.)
	dwarfgen.RecordFlags(target, "B", "N", "l", "msan", "race", "asan", "shared", "dynlink", "dwarf", "dwarflocationlists", "dwarfbasentries", "smallframes", "spectre")

	if !base.EnableTrace && target.Flags.LowerT {
		target.Diagnostics.Fatalf("compiler not built with support for -t")
	}

	// Enable inlining (after RecordFlags, to avoid recording the rewritten -l).  For now:
	//	default: inlining on.  (Flag.LowerL == 1)
	//	-l: inlining off  (Flag.LowerL == 0)
	//	-l=2, -l=3: inlining on again, with extra debugging (Flag.LowerL > 1)
	if target.Flags.LowerL <= 1 {
		target.Flags.LowerL = 1 - target.Flags.LowerL
	}

	if target.Flags.SmallFrames {
		target.MaxStackVarSize = 64 * 1024
		target.MaxImplicitStackVarSize = 16 * 1024
	}

	if target.Flags.Dwarf {
		target.Ctxt.DebugInfo = func(ctxt *obj.Link, fnsym *obj.LSym, infosym *obj.LSym, curfn obj.Func) ([]dwarf.Scope, dwarf.InlCalls) {
			return dwarfgen.Info(target, ctxt, fnsym, infosym, curfn)
		}
		target.Ctxt.GenAbstractFunc = func(fn *obj.LSym) { dwarfgen.AbstractFunc(target.Ctxt, fn) }
		target.Ctxt.DwFixups = obj.NewDwarfFixupTable(target.Ctxt)
	} else {
		// turn off inline generation if no dwarf at all
		target.Flags.GenDwarfInl = 0
		target.Ctxt.Flag_locationlists = false
	}
	if target.Ctxt.Flag_locationlists && len(target.Ctxt.Arch.DWARFRegisters) == 0 {
		target.Diagnostics.Fatalf("location lists requested but register mapping not available on %v", target.Ctxt.Arch.Name)
	}

	langVersion, err := types.ParseLangFlag(target.Flags.Lang)
	if err != nil {
		target.Diagnostics.Fatalf("%v", err)
	}
	target.LangVersion = langVersion

	symABIs := ssagen.NewSymABIs()
	if target.Flags.SymABIs != "" {
		symABIs.ReadSymABIs(target, target.Flags.SymABIs)
	}

	if objabi.LookupPkgSpecial(target.Ctxt.Pkgpath).NoInstrument {
		target.Flags.Race = false
		target.Flags.MSan = false
		target.Flags.ASan = false
	}

	arch.LinkArch.Init(target.Ctxt)
	if !reusable {
		startProfile(target)
	}
	if target.Flags.Race || target.Flags.MSan || target.Flags.ASan {
		target.Flags.Cfg.Instrumenting = true
	}
	if target.Flags.Dwarf {
		target.Ctxt.LogDwarf = target.Debug.DwarfInl != 0
	}
	if target.Debug.SoftFloat != 0 {
		arch.SoftFloat = true
	}

	if target.Flags.JSON != "" { // parse version,destination from json logging optimization.
		if err := target.LogOpt.LogJsonOption(target.Flags.JSON); err != nil {
			target.Diagnostics.Fatalf("invalid -json option: %v", err)
		}
	}

	target.EscFmt = escape.Fmt
	target.IntrinsicCall = func(call *ir.CallExpr) bool { return ssagen.IsIntrinsicCall(target, call) }
	target.IntrinsicSym = func(sym *types.Sym) bool { return ssagen.IsIntrinsicSym(target, sym) }
	ssagen.InitEnv(backend, target)
	target.SoftFloat = arch.SoftFloat

	target.PtrSize = arch.LinkArch.PtrSize
	target.RegSize = arch.LinkArch.RegSize
	target.MaxWidth = arch.MAXWIDTH

	target.AutoPos = makePos(target.Ctxt, src.NewFileBase("<autogenerated>", "<autogenerated>"), 1, 0)
	target.Pos = target.AutoPos

	typecheck.InitUniverse(target)
	typecheck.InitRuntime(target, timer)
	rttype.Init(target)

	// Some intrinsics (notably, the simd intrinsics) mention
	// types "eagerly", thus ssagen must be initialized AFTER
	// the type system is ready.
	ssagen.InitTables(target)

	// Parse and typecheck input.
	var frontend *noder.Context
	if command != nil {
		jitFiles, hasJIT, err := commandJITSourceFiles(target, commandFiles)
		if err != nil {
			target.Diagnostics.ErrorfAt(target.AutoPos, 0, "jit metadata: %v", err)
			target.Diagnostics.ErrorExit()
		}
		if hasJIT {
			frontend = noder.LoadPackageFromSourceFiles(jitFiles, target, timer)
		} else {
			frontend = noder.LoadPackage(commandFiles, target, timer)
		}
	} else {
		frontend = noder.LoadPackageFromSourceFiles(sourceFiles, target, timer)
	}
	if err := finalizeJITScope(target); err != nil {
		target.Diagnostics.ErrorfAt(target.AutoPos, 0, "jit linker roots: %v", err)
		target.Diagnostics.ErrorExit()
	}
	if request != nil {
		frontend.ObjectOutput = request.objectOutput
	}

	// As a convenience to users (toolchain maintainers, in particular),
	// when compiling a package named "main", we default the package
	// path to "main" if the -p flag was not specified.
	if target.Ctxt.Pkgpath == obj.UnlinkablePkg && target.LocalPkg.Name == "main" {
		target.Ctxt.Pkgpath = "main"
		target.LocalPkg.Path = "main"
		target.LocalPkg.Prefix = "main"
	}

	dwarfgen.RecordPackageName(target)

	// Prepare for backend processing.
	ssagen.InitConfig(backend, ssacompile.NewConfig(target, arch.SoftFloat), target)

	// Apply coverage fixups, if applicable.
	coverage.Fixup(target)

	// Read profile file and build profile-graph and weighted-call-graph.
	timer.Start("fe", "pgo-load-profile")
	var profile *pgoir.Profile
	if target.Flags.PgoProfile != "" {
		var err error
		profile, err = pgoir.New(target.Flags.PgoProfile, target, frontend.LookupFunc, frontend.PostLookupCleanup)
		if err != nil {
			target.Diagnostics.Fatalf("%s: PGO error: %v", target.Flags.PgoProfile, err)
		}
	}

	for _, fn := range target.Funcs {
		if ir.MatchAstDump(fn, "start") {
			ir.AstDump(fn, "start, "+ir.FuncName(fn))
		}
	}

	// Apply bloop markings.
	bloop.Walk(target)

	// Interleaved devirtualization and inlining.
	timer.Start("fe", "devirtualize-and-inline")
	interleaved.DevirtualizeAndInlinePackage(target, profile, frontend.Inliner, frontend.InlineCall)

	for _, fn := range target.Funcs {
		if ir.MatchAstDump(fn, "devirtualize-and-inline") {
			ir.AstDump(fn, "devirtualize-and-inline, "+ir.FuncName(fn))
		}
	}

	frontend.MakeWrappers(target) // must happen after inlining

	// Get variable capture right in for loops.
	var transformed []loopvar.VarAndLoop
	for _, fn := range target.Funcs {
		transformed = append(transformed, loopvar.ForCapture(fn)...)
	}

	// Build init task, if needed.
	pkginit.MakeTask(frontend)

	// Generate ABI wrappers. Must happen before escape analysis
	// and doesn't benefit from dead-coding or inlining.
	symABIs.GenABIWrappers(target)

	// Preassign symbol indexes so that they're set when we emit export data,
	// if it's emitted early. This depends on the results of GenABIWrappers.
	preassignSymIdxs(target, symABIs)

	deadlocals.Funcs(target.Funcs)

	// Escape analysis.
	// Required for moving heap allocations onto stack,
	// which in turn is required by the closure implementation,
	// which stores the addresses of stack variables into the closure.
	// If the closure does not escape, it needs to be on the stack
	// or else the stack copier will not update it.
	// Large values are also moved off stack in escape analysis;
	// because large values may contain pointers, it must happen early.
	timer.Start("fe", "escapes")
	escape.Funcs(target.Funcs)

	rewriteresults.Funcs(target.Funcs)

	slice.Funcs(target.Funcs)

	loopvar.LogTransformations(transformed)

	// Collect information for go:nowritebarrierrec
	// checking. This must happen before transforming closures during Walk
	// We'll do the final check after write barriers are
	// inserted.
	if target.Flags.CompilingRuntime {
		backend.EnableNoWriteBarrierRecCheck(target)
	}

	timer.Start("fe", "dumpexport")
	dumpexport(frontend)

	reflectdata.WriteBasicTypes(target)

	// Compile top-level declarations.
	//
	// There are cyclic dependencies between all of these phases, so we
	// need to iterate all of them until we reach a fixed point.
	timer.Start("be", "compilefuncs")
	for nextFunc, nextExtern := 0, 0; ; {
		reflectdata.WriteRuntimeTypes(target)

		if nextExtern < len(target.Externs) {
			switch n := target.Externs[nextExtern]; n.Op() {
			case ir.ONAME:
				dumpGlobal(target, n)
			case ir.OLITERAL:
				dumpGlobalConst(target, n)
			case ir.OTYPE:
				reflectdata.NeedRuntimeType(target, n.Type())
			}
			nextExtern++
			continue
		}

		if nextFunc < len(target.Funcs) {
			enqueueFunc(target.Funcs[nextFunc], symABIs, &compilequeue, backend)
			nextFunc++
			continue
		}

		// The SSA backend supports using multiple goroutines, so keep it
		// as late as possible to maximize how much work we can batch and
		// process concurrently.
		if len(compilequeue) != 0 {
			compileFunctions(target, &compilequeue, backend, frontend.Inliner, profile, ssaCompiler)
			continue
		}

		// Finalize DWARF inline routine DIEs, then explicitly turn off
		// further DWARF inlining generation to avoid problems with
		// generated method wrappers.
		//
		// Note: The DWARF fixup code for inlined calls currently doesn't
		// allow multiple invocations, so we intentionally run it just
		// once after everything else. Worst case, some generated
		// functions have slightly larger DWARF DIEs.
		if target.Ctxt.DwFixups != nil {
			target.Ctxt.DwFixups.Finalize(target.Ctxt.Pkgpath, target.Debug.DwarfInl != 0)
			target.Ctxt.DwFixups = nil
			target.Flags.GenDwarfInl = 0
			continue // may have called reflectdata.TypeLinksym (#62156)
		}

		break
	}

	if target.Flags.Stats {
		s := new(stats.Stats)
		for _, fn := range target.Funcs {
			s.Merge(fn.Stats)
		}
		s.Print()
	}

	timer.AddEvent(int64(len(target.Funcs)), "funcs")

	if target.Flags.CompilingRuntime {
		// Write barriers are now known. Check the call graph.
		backend.NoWriteBarrierRecCheck()
	}

	// Add keep relocations for global maps.
	if target.Debug.WrapGlobalMapCtl != 1 {
		staticinit.AddKeepRelocations(target)
	}

	// Write object data to disk.
	timer.Start("be", "dumpobj")
	dumpdata(target)
	target.Ctxt.NumberSyms()
	dumpobj(frontend)
	if target.Flags.AsmHdr != "" {
		dumpasmhdr(target)
	}

	backend.CheckLargeStacks()

	if len(compilequeue) != 0 {
		target.Diagnostics.Fatalf("%d uncompiled functions", len(compilequeue))
	}

	if err := target.LogOpt.FlushLoggedOpts(target.Ctxt, target.Ctxt.Pkgpath); err != nil {
		target.Diagnostics.Fatalf("cannot write optimizer log: %v", err)
	}
	target.Diagnostics.ExitIfErrors()

	target.Diagnostics.FlushErrors()
	timer.Stop()

	if target.Flags.Bench != "" {
		if err := writebench(timer, target.Ctxt.Pkgpath, target.Flags.Bench); err != nil {
			target.Diagnostics.Fatalf("cannot write benchmark data: %v", err)
		}
	}
	target.RunCleanup()
}

func writebench(timer *base.Timings, pkgpath, filename string) error {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	fmt.Fprintln(&buf, "commit:", buildcfg.Version)
	fmt.Fprintln(&buf, "goos:", runtime.GOOS)
	fmt.Fprintln(&buf, "goarch:", runtime.GOARCH)
	timer.Write(&buf, "BenchmarkCompile:"+pkgpath+":")

	n, err := f.Write(buf.Bytes())
	if err != nil {
		return err
	}
	if n != buf.Len() {
		panic("bad writer")
	}

	return f.Close()
}

func makePos(ctxt *obj.Link, b *src.PosBase, line, col uint) src.XPos {
	return ctxt.PosTable.XPos(src.MakePos(b, line, col))
}
