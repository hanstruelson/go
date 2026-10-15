// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

import (
	"cmd/compile/publicinternal/base"
	"cmd/compile/publicinternal/logopt"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/src"
	"sync"
)

// A Package holds information about the package being compiled.
type Package struct {
	// Flags and Debug are the parsed options for this compilation.
	Flags       base.CmdFlags
	Debug       base.DebugFlags
	LangVersion types.LangVersion

	// Pos and AutoPos are the current and generated source positions for this
	// compilation. Pos is independent of the legacy base.Pos compatibility path.
	Pos     src.XPos
	AutoPos src.XPos

	// Debug hash selectors for this compilation.
	PGOHash          *base.HashDebug
	LoopVarHash      *base.HashDebug
	GossaHash        *base.HashDebug
	ConvertHash      *base.HashDebug
	FmaHash          *base.HashDebug
	LiteralAllocHash *base.HashDebug
	VariableMakeHash *base.HashDebug
	MergeLocalsHash  *base.HashDebug

	// Ctxt is the linker context for this compilation.
	Ctxt *obj.Link
	// LogOpt owns optimizer diagnostics for this compilation.
	LogOpt *logopt.Context
	// Diagnostics owns parse, type-check, and backend messages for this compilation.
	Diagnostics *base.Diagnostics
	// ArchInfo is the backend's architecture-specific code generation context.
	// It is stored opaquely here to avoid an ir <-> ssagen import cycle.
	ArchInfo any
	// FrontendContext is the active noder context, stored opaquely to avoid an
	// ir <-> noder import cycle. It is used to close per-compilation debug output.
	FrontendContext any
	// EscFmt adds escape-analysis annotations to IR dumps for this target.
	EscFmt func(Node) string
	// PtrSize, RegSize, and MaxWidth describe this target's type layout limits.
	PtrSize  int
	RegSize  int
	MaxWidth int64
	// Stack allocation limits are set by compiler options such as -smallframes.
	MaxStackVarSize         int64
	MaxImplicitStackVarSize int64

	// LocalPkg is the types.Pkg associated with this compilation unit.
	LocalPkg *types.Pkg
	// BuiltinPkg and UnsafePkg are this compilation's pseudo-packages.
	BuiltinPkg *types.Pkg
	UnsafePkg  *types.Pkg
	BlankSym   *types.Sym

	// Pkgs is the per-compilation package registry used by import readers.
	PkgsMu sync.Mutex
	Pkgs   map[string]*types.Pkg
	// BlankNode is the package's IR node for the blank identifier.
	BlankNode *Name
	// ShapePkg and GoPkg hold generated symbols used by this compilation.
	ShapePkg *types.Pkg
	GoPkg    *types.Pkg
	// RuntimePkg holds the compiler's builtin declarations for runtime.
	RuntimePkg *types.Pkg
	// Symbols holds linker symbols for runtime helpers on this target.
	Symbols symsStruct

	// IntrinsicCall and IntrinsicSym dispatch backend intrinsic queries for this target.
	IntrinsicCall    func(*CallExpr) bool
	IntrinsicSym     func(*types.Sym) bool
	IntrinsicBuilder func(*types.Sym) any
	SoftFloat        bool
	SSACheck         bool // run SSA consistency checks for this target
	SSACheckSeed     int  // randomization seed used by SSA consistency checks
	// SSA debug options read by compiler workers.
	SSAIntrinsicsDebug   int
	SSAIntrinsicsDisable bool
	SSABuildDump         map[string]bool
	SSAGenssaDump        map[string]bool

	// TypeSymPkg holds runtime type descriptor symbols for this compilation.
	TypeSymPkg *types.Pkg
	TypeSymMu  sync.Mutex

	// Basic and predeclared types for this compilation. These point at the
	// universe initialized for the target and keep frontend/backend lookups
	// explicit at call sites.
	TypeContext    *types.Context
	BasicTypes     [types.NTYPE]*types.Type
	AnyType        *types.Type
	ByteType       *types.Type
	RuneType       *types.Type
	ErrorType      *types.Type
	ComparableType *types.Type
	ABITypesMu     sync.Mutex
	ABISynthSlice  *types.Type
	ABISynthString *types.Type
	ABISynthIface  *types.Type

	// InternalMapsPkg contains compiler-generated map runtime types for this
	// compilation.
	InternalMapsPkg *types.Pkg
	ItabPkg         *types.Pkg
	CoveragePkg     *types.Pkg
	MapTableType    *types.Type
	MapType         *types.Type
	MapIterType     *types.Type
	ScaseType       *types.Type
	ItabTypeField   *types.Field
	RTypeHashField  *types.Field
	ItabHashField   *types.Field

	// Closgen is the next generated closure suffix for package scope.
	Closgen int32
	// AutoTmpNames interns generated temporary names for this compilation.
	AutoTmpNames []string
	// FuncSymsMu and FuncSyms track generated function-value symbols.
	FuncSymsMu sync.Mutex
	FuncSyms   []*Name

	// DnameGen is the next generated reflect name-data suffix.
	DnameGen int

	// TypeAssertGen and InterfaceSwitchGen are generated descriptor suffixes.
	TypeAssertGen      int
	InterfaceSwitchGen int

	// InstrumentedGlobals tracks globals selected for ASan registration.
	InstrumentedGlobalsMap   map[string]Node
	InstrumentedGlobalsSlice []Node

	// Static initialization state for this compilation unit.
	StaticUniqueGen  int
	MapInitGen       int
	VarInitGen       int
	VarToMapInit     map[*Name]*Func
	MapInitToVar     map[*Func]*Name
	ZeroSize         int64
	MemhashVarlen    *obj.LSym
	MemequalVarlen   *obj.LSym
	RuntimeTypeMu    sync.Mutex
	RuntimeTypes     map[*types.Type]struct{}
	RuntimeTypeQueue []*types.Type
	GCSymTypes       map[*types.Type]struct{}
	// RuntimeTypeInfo contains the runtime ABI type layouts for this target.
	RuntimeTypeInfo any

	// TypeSymIdx maps imported types to their type descriptor symbol indexes
	// for this compilation unit.
	TypeSymIdx map[*types.Type][2]int64

	// MapInitLsyms contains outlined map initializer symbols for this package.
	MapInitLsyms map[*obj.LSym]struct{}

	// Imports, listed in source order.
	// See golang.org/issue/31636.
	Imports []*types.Pkg

	// Init functions, listed in source order.
	Inits []*Func

	// Funcs contains all (instantiated) functions, methods, and
	// function literals to be compiled.
	Funcs []*Func

	// Externs holds constants, (non-generic) types, and variables
	// declared at package scope.
	Externs []*Name

	// AsmHdrDecls holds declared constants and struct types that should
	// be included in -asmhdr output. It's only populated when -asmhdr
	// is set.
	AsmHdrDecls []*Name

	// Cgo directives.
	CgoPragmas [][]string

	// Variables with //go:embed lines.
	Embeds []*Name

	// PluginExports holds exported functions and variables that are
	// accessible through the package plugin API. It's only populated
	// for -buildmode=plugin (i.e., compiling package main and -dynlink
	// is set).
	PluginExports []*Name

	// Debug dump state belongs to this compilation. dumpMu serializes writes
	// because separate target contexts may still choose the same output path.
	dumpMu          sync.Mutex
	astDumpFiles    map[string]bool
	htmlDumpWriters map[*Func]*HTMLWriter
	htmlDumpFuncs   []*Func
}

// Pkg returns the package instance for path in this compilation context.
func (target *Package) Pkg(path, name string) *types.Pkg {
	target.PkgsMu.Lock()
	defer target.PkgsMu.Unlock()
	if target.Pkgs == nil {
		target.Pkgs = make(map[string]*types.Pkg)
	}
	if pkg := target.Pkgs[path]; pkg != nil {
		if name != "" {
			if pkg.Name == "" {
				pkg.Name = name
			} else if pkg.Name != name {
				panic("conflicting package names " + pkg.Name + " and " + name + " for path " + path)
			}
		}
		return pkg
	}
	var pkg *types.Pkg
	if target.TypeContext != nil {
		pkg = types.NewPkgInstanceFor(target.TypeContext, path, name)
	} else {
		pkg = types.NewPkgInstance(path, name)
	}
	target.Pkgs[path] = pkg
	return pkg
}

// LookupPkg reports the package instance for path in this compilation context.
func (target *Package) LookupPkg(path string) *types.Pkg {
	target.PkgsMu.Lock()
	defer target.PkgsMu.Unlock()
	return target.Pkgs[path]
}

// Type returns the target's canonical type for kind.
func (target *Package) Type(kind types.Kind) *types.Type {
	return target.BasicTypes[kind]
}
