// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"cmd/publicinternal/cov/covcmd"
	"flag"
)

// PackageExportData is the compiler-facing, code-free import record used by
// in-process compilation. ObjectHeader contains the target object header and
// any package export headers; Data is the unified Go export payload only.
// Package archives and object code are deliberately not part of this type.
type PackageExportData struct {
	PackagePath       string
	PackageName       string
	ObjectHeader      string
	ExportDataVersion uint32
	Fingerprint       [8]byte
	Data              []byte
}

// A CountFlag is a counting integer flag.
// It accepts -name=value to set the value directly,
// but it also accepts -name with no =value to increment the count.
type CountFlag int

// CmdFlags defines the command-line flags (see var Flag).
// Each struct field is a different flag, by default named for the lower-case of the field name.
// If the flag name is a single letter, the default flag name is left upper-case.
// If the flag name is "Lower" followed by a single letter, the default flag name is the lower-case of the last letter.
//
// If this default flag name can't be made right, the `flag` struct tag can be used to replace it,
// but this should be done only in exceptional circumstances: it helps everyone if the flag name
// is obvious from the field name when the flag is used elsewhere in the compiler sources.
// The `flag:"-"` struct tag makes a field invisible to the flag logic and should also be used sparingly.
//
// Each field must have a `help` struct tag giving the flag help message.
//
// The allowed field types are bool, int, string, pointers to those (for values stored elsewhere),
// CountFlag (for a counting flag), and func(string) (for a flag that uses special code for parsing).
type CmdFlags struct {
	// Single letters
	B CountFlag    "help:\"disable bounds checking\""
	C CountFlag    "help:\"disable printing of columns in error messages\""
	D string       "help:\"set relative `path` for local imports\""
	E CountFlag    "help:\"debug symbol export\""
	I func(string) "help:\"add `directory` to import search path\""
	K CountFlag    "help:\"debug missing line numbers\""
	L CountFlag    "help:\"also show actual source file names in error messages for positions affected by //line directives\""
	N CountFlag    "help:\"disable optimizations\""
	S CountFlag    "help:\"print assembly listing\""
	// V is added by objabi.AddVersionFlag
	W CountFlag "help:\"debug parse tree after type checking\""

	LowerC int        "help:\"concurrency during compilation (1 means no concurrency)\""
	LowerD flag.Value "help:\"enable debugging settings; try -d help\""
	LowerE CountFlag  "help:\"no limit on number of errors reported\""
	LowerH CountFlag  "help:\"halt on error\""
	LowerJ CountFlag  "help:\"debug runtime-initialized variables\""
	LowerL CountFlag  "help:\"disable inlining\""
	LowerM CountFlag  "help:\"print optimization decisions\""
	LowerO string     "help:\"write output to `file`\""
	LowerP *string    "help:\"set expected package import `path`\"" // &Ctxt.Pkgpath, set below
	LowerR CountFlag  "help:\"debug generated wrappers\""
	LowerT bool       "help:\"enable tracing for debugging the compiler\""
	LowerW CountFlag  "help:\"debug type checking\""
	LowerU CountFlag  "help:\"emit unsorted warnings/errors\""
	LowerV *bool      "help:\"increase debug verbosity\""

	// Special characters
	Percent          CountFlag "flag:\"%\" help:\"debug non-static initializers\""
	CompilingRuntime bool      "flag:\"+\" help:\"compiling runtime\""

	// Longer names
	AsmHdr             string       "help:\"write assembly header to `file`\""
	ASan               bool         "help:\"build code compatible with C/C++ address sanitizer\""
	Bench              string       "help:\"append benchmark times to `file`\""
	BlockProfile       string       "help:\"write block profile to `file`\""
	BuildID            string       "help:\"record `id` as the build id in the export metadata\""
	CPUProfile         string       "help:\"write cpu profile to `file`\""
	Complete           bool         "help:\"compiling complete package (no C or assembly)\""
	ClobberDead        bool         "help:\"clobber dead stack slots (for debugging)\""
	ClobberDeadReg     bool         "help:\"clobber dead registers (for debugging)\""
	Dwarf              bool         "help:\"generate DWARF symbols\""
	DwarfBASEntries    *bool        "help:\"use base address selection entries in DWARF\""                        // &Ctxt.UseBASEntries, set below
	DwarfLocationLists *bool        "help:\"add location lists to DWARF in optimized mode\""                      // &Ctxt.Flag_locationlists, set below
	Dynlink            *bool        "help:\"support references to Go symbols defined in other shared libraries\"" // &Ctxt.Flag_dynlink, set below
	EmbedCfg           func(string) "help:\"read go:embed configuration from `file`\""
	Env                func(string) "help:\"add `definition` of the form key=value to environment\""
	ExportFD           int          "help:\"write a byte to file descriptor `fd` once the export data has been written\""
	GenDwarfInl        int          "help:\"generate DWARF inline info records\"" // 0=disabled, 1=funcs, 2=funcs+formals/locals
	GoVersion          string       "help:\"required version of the runtime\""
	ImportCfg          func(string) "help:\"read import configuration from `file`\""
	InstallSuffix      string       "help:\"set pkg directory `suffix`\""
	JSON               string       "help:\"version,file for JSON compiler/optimizer detail output\""
	Lang               string       "help:\"Go language version source code expects\""
	LinkObj            string       "help:\"write linker-specific object to `file`\""
	LinkShared         *bool        "help:\"generate code that will be linked against Go shared libraries\"" // &Ctxt.Flag_linkshared, set below
	Live               CountFlag    "help:\"debug liveness analysis\""
	MSan               bool         "help:\"build code compatible with C/C++ memory sanitizer\""
	MemProfile         string       "help:\"write memory profile to `file`\""
	MemProfileRate     int          "help:\"set runtime.MemProfileRate to `rate`\""
	MutexProfile       string       "help:\"write mutex profile to `file`\""
	NoLocalImports     bool         "help:\"reject local (relative) imports\""
	CoverageCfg        func(string) "help:\"read coverage configuration from `file`\""
	Pack               bool         "help:\"write to file.a instead of file.o\""
	Race               bool         "help:\"enable race detector\""
	Shared             *bool        "help:\"generate code that can be linked into a shared library\"" // &Ctxt.Flag_shared, set below
	SmallFrames        bool         "help:\"reduce the size limit for stack allocated objects\""      // small stacks, to diagnose GC latency; see golang.org/issue/27732
	Spectre            string       "help:\"enable spectre mitigations in `list` (all, index, ret)\""
	Stats              bool         "help:\"print compiler statistics\""
	Std                bool         "help:\"compiling standard library\""
	SymABIs            string       "help:\"read symbol ABIs from `file`\""
	TraceProfile       string       "help:\"write an execution trace to `file`\""
	TrimPath           string       "help:\"remove `prefix` from recorded source file paths\""
	WB                 bool         "help:\"enable write barrier\"" // TODO: remove
	PgoProfile         string       "help:\"read profile or pre-process profile from `file`\""
	ErrorURL           bool         "help:\"print explanatory URL with error message if applicable\""

	// Configuration derived from flags; not a flag itself.
	Cfg struct {
		Embed struct { // set by -embedcfg
			Patterns map[string][]string
			Files    map[string]string
		}
		ImportDirs      []string                     // appended to by -I
		ImportMap       map[string]string            // set by -importcfg
		PackageFile     map[string]string            // set by -importcfg; nil means not in use
		PackageExports  map[string]PackageExportData // code-free package exports supplied by an in-process compile request
		JITImportFiles  map[string]bool              // source files whose imports are visible to runtime Eval
		JITCallSiteFile string                       // host source file inherited by runtime-compiled Eval calls
		CoverageInfo    *covcmd.CoverFixupConfig     // set by -coveragecfg
		SpectreIndex    bool                         // set by -spectre=index or -spectre=all
		// Whether we are adding any sort of code instrumentation, such as
		// when the race detector is enabled.
		Instrumenting bool
	}
}
