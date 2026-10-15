# Compiler context and reusable compilation audit

## Current checkout

The working branch is at `1ef0648d3f9`, including the rename from `internal`
to `publicinternal` and the reusable compiler request API. This change removes
the archive-based `go build -JIT` experiment from `cmd/go` and its symbol
retention and package-loading hooks in `cmd/link`. The compiler still emits an
ordinary object archive for the standard linker; the removed feature was the
extra GOROOT/toolchain bundle appended to executables.

Go source parsing is available through `go/parser` and `go/format`. The
compiler's lower-level parser and printer are also importable as
`cmd/compile/publicinternal/syntax` and expose `syntax.Parse` and
`syntax.Fprint`. The parser and printer keep per-call state; the parser smoke
test calls the standard parser, checker, and formatter and the compiler parser
and printer through their direct imports.

The context migration is not complete. `gc.Compile` still serializes requests
and saves/restores `base` package globals, while regular `gc.Main` sets and
reads `base.Ctxt`, `base.Flag`, and `base.Debug`. The frontend, optimization
passes, and backend now report through target-owned diagnostics when called
with a target. Contextless compatibility helpers remain in `ir`, `typecheck`,
and `types`; those retain global fallbacks for legacy callers. The `types`
package also retains a default context and package-level compatibility state.
Type-check trace indentation and the size-tracing switch now live on each
`types.Context`; the type checker no longer shares a trace-indent slice across
requests. A normal static build succeeds, but it still enters through the
global command-line path and does not establish global-free compilation.

## Where one compilation starts and ends

`cmd/compile/main.go` chooses a target architecture and calls
`gc.Main(archInit)`. `gc.Main` performs both command-line setup and the whole
package compilation: it creates `obj.Link`, parses process-global flags,
initializes target and universe state, asks `noder.LoadPackage` to read source
filenames, runs frontend and optimization passes, generates object code, and
writes output files. `base.Exit` eventually terminates the command.
`gc.Compile(Request)` now provides a request/result boundary for named
in-memory Go source files. It captures diagnostics and returns the object
archive without terminating the caller on compiler errors.

## Mutable state that belongs to a compilation

- `base`: `Ctxt`, `Flag`, `Debug`, command output/exit behavior, and legacy
  diagnostics remain process-wide adapters. The `noder` parser/type2-checker,
  IR typechecker, optimization passes, and code generators use the target's
  `base.Diagnostics` for positions, errors, warnings, and exits. They no longer
  read or write `base.Pos` or the package diagnostic queue on the target-owned
  path. A few contextless compatibility helpers still fall back to package
  diagnostics. The timing collector is created in `gc.Main` and passed into
  the frontend; the former `base.Timer` singleton is removed.
- `types`: target layout and the canonical basic-type universe now live in a
  `types.Context` owned by each `ir.Package`. Each `types.Type` and package
  retains its context, so sizing, deferred layout, and basic-type identity
  follow the owning target. Linker-symbol lookup and fake interface receiver
  types use the same context. Package-level layout variables and legacy
  constructors remain for standalone utilities and tests; the command
  compiler no longer configures those globals. Package registries now live on
  `ir.Package`; the old process-wide `pkgMap` and its `NewPkg` and `PkgMap`
  APIs are removed. The interned-string table is separately synchronized.
- `typecheck` and `ir`: the global `typecheck.Target` and mutable stack-size
  limits are removed; phase APIs carry `*ir.Package`, and each `ir.Func`
  records its owning target. Imported type symbol indexes, package-scope
  closure numbering, temporary-name caches, outlined map-init symbol tracking,
  stack-size limits, and escape-dump formatting now live on that package.
  Selector path and embedded
  method scratch buffers are local to each operation. Text and HTML dump
  registries are scoped to the target/frontend context, and cleanup closes only
  that compilation's writers. Inliner and PGO source-position formatting now
  receives the owning target explicitly; `ir.WithPos` and compiler-phase
  `ir.HasUniquePosFor` calls use the owning target rather than reading or
  modifying `base.Pos`/`base.Flag`. IR constant formatting and default type
  selection follow the type's owning `types.Context`. Type-kind and operator
  classification tables are initialized once and shared as immutable data.
- `noder`: package readers, object/body lookup maps, pending body/dictionary
  work, wrapper-generation caches, and generated-name counters now belong to
  the per-load `noder.Context` rather than surviving in package globals.
- optimizer diagnostics: format selection, destination, and buffered records
  now live on each target's `logopt.Context`. Frontend, optimization, SSA, and
  architecture generators log through the owning function or package.
- `ssagen` and backend passes: selected architecture, SSA configuration and
  caches, environment-derived debug settings, and output hooks. SSA consistency
  checking, its randomization seed, intrinsic debug/disable options, function
  dump selectors, and phase/pass settings now live in a per-compilation
  `ssacompile.Options` and are copied into each target before backend workers
  start. The legacy `ssacompile.PhaseOption` function remains an adapter for
  callers that still need process-level defaults.
- command and output plumbing: `flag.CommandLine`, process environment,
  `os.Stdout`/`os.Stderr`, logging, telemetry, profiles, and process exit.
- `cmd/publicinternal/obj`: an `obj.Link` is already constructed per `gc.Main`, but
  shared compiler helpers reach it through `base.Ctxt`.

Package-scope declarations still include immutable tables, compatibility
defaults, and a smaller number of mutable debug or command adapters. They need
to be classified by call path rather than treated as a blanket move list.

## Existing boundaries worth preserving

The syntax parser already accepts an `io.Reader`, and `types2` already has
per-checker configuration and a `Context` for shared type instances. Those are
useful building blocks, but they do not make the backend reentrant. The current
implementation adds `noder.LoadPackageFromSourceFiles` and an explicit
`noder.Context`. Package reader maps, deferred body and dictionary work,
wrapper caches, init-name counters, the target package, timer, and inliner
context now belong to that compilation context. PGO graph construction,
reflection type writing, and per-package compiler helpers also take the target
explicitly. The inliner's PGO hot-call maps and budgets are owned by
`inline.Context`. This provides a request/result API for sequential
compilation. The adapter still serializes calls and scopes legacy process
globals; it is not a concurrent, fully context-threaded compiler.

The context migration now puts the compiler's pseudo-runtime declarations,
runtime type descriptor symbols and ABI layouts, generic shape symbols,
anonymous receiver symbols, the blank identifier node, generated runtime
`LSym` fields, pointer and register sizes, stack allocation limits, and
intrinsic dispatch on each target package. Runtime lookup, reflection, static initializer analysis, SSA,
stack-map generation, and architecture generators use the target context.
The escape-analysis formatter used by IR dumps is target-owned too.
Default literal selection, constant indexes, closure layouts, and ABI pointer
bitmap generation also take target state explicitly. The synthetic
`publicinternal/runtime/math.MulUintptr` symbol is interned in the owning target's
package registry. `gc.Main` still adapts
command-line linker and flag state into a target. The `types` package retains
legacy default-context globals for callers that have not migrated. PGO lookup finds the current local package through
its target, and the midway recheck resets only this target's direct-import
flags.

Pointer and slice type caches now publish canonical composite types with
atomic operations, so backend workers no longer depend on a process-wide
pointer-cache toggle.
Target-independent type-kind and operator tables are initialized once and
then shared as immutable data instead of being rewritten by every universe
initialization. The target-dependent `SimType` table was replaced by a pure
layout mapping and per-type conversion checks. Debug dump file tracking and
writer registries also live on the target/frontend context, so one run's
cleanup does not close another run's dump writers.

`ir.Syms`, `ir.Pkgs`, `ir.BlankNode`, `types.TypeSym`'s global table, and the
global shape package, and SSA's process-wide dump-option variables have been
removed. Type sizing, deferred layout, basic-type universes, and package
registries are target-owned. Remaining blockers include command-line parsing,
output and process state in `gc.Compile`, plus compatibility globals in
contextless IR, typecheck, and type APIs. The request API serializes access to
its command adapter, restores that state, and converts compiler exits to
returned errors. It skips process-wide runtime profiling during a request.

## Reusable source compilation API

`gc.Compile` accepts an architecture initializer, package path, named source
strings, and compiler options in `-name=value` form. It creates fresh target,
frontend, IR, type-universe, and linker contexts for each request. Its result
contains the Go object archive, captured diagnostic text, and error counts.
Output and profiling flags, `-p`, `-o`, and environment mutation are managed
or rejected by the API. Imported packages still require normal compiler import
configuration through options such as `-importcfg`.

The entry point serializes calls because the command compiler still adapts
package globals such as `base.Flag`, `base.Ctxt`, and Go's flag and standard
stream globals. It saves and restores base diagnostics, debug hashes, cleanup
hooks, command arguments, standard streams, and flag state. DWARF logging and
FIPS debug hashing are attached to each `obj.Link`; SSA pass settings are
request-owned.
Type-check trace
indentation is owned by each target's `types.Context` and no longer requires
save/restore here.
Runtime profiling is skipped in a request. This permits sequential reuse in
one process; full context propagation is still required for concurrent
compilation without the serialization lock.

`gc` now lives at `cmd/compile/publicinternal/gc`, so toolchain packages can
import it directly without a facade. The `cmd/compile` path is toolchain
source, not a stable public library contract for third-party applications.

## Recommended migration

1. Extend the request/result API with caller-owned import/export configuration,
   diagnostics sinks, and other required build inputs. The current API returns
   object bytes and diagnostic text, and requires an architecture initializer.
2. Keep `main` as a thin adapter that translates flags and files into the same
   request path. Move initialization and phase orchestration into a
   `gc.Compiler` or equivalent per-request context.
3. Migrate state by subsystem, starting with remaining `base` diagnostics and
   command setup, then remaining `ir` hooks, `ssagen` and optimization passes,
   and finally object writers.
   Pass context explicitly through phase APIs and callbacks.
   Keep immutable opcode/type tables and safe shared caches at package scope.
4. Make diagnostics, timers, profiles, and object output request-owned. Remove
   direct flag parsing, environment mutation, and process exit from the
   reusable path.
5. Add repeated-compilation and concurrent-compilation coverage after each
   subsystem migration. Runtime execution of generated machine code requires
   a separate linker/loader path that resolves relocations and registers GC
   and unwind metadata; `cmd/compile` does not provide that loader.

## Shortcut assessment

Find-and-replace is suitable only after a context is threaded into a bounded
package: it can rewrite mechanical selectors such as `base.Ctxt` to a context
field. It cannot correctly add context parameters through the compiler's call
graph, handle callbacks, or determine whether a package global is immutable.
The efficient safe shortcut is an AST/type-aware codemod that propagates a
context parameter through direct calls, followed by manual handling for
callbacks and cross-package APIs.

## Verification in this workspace

`GOROOT_BOOTSTRAP=/workspace/.tools/go1.26.0 GOCACHE=/tmp/gocache-context-refactor TMPDIR=/tmp ./make.bash`
completed and installed the toolchain. A standalone program also built with
`CGO_ENABLED=0` and ran
successfully, printing `context compiler smoke build ok`. The direct compiler
syntax parser/printer import test passed. `git diff --check` is clean. The
local `HEAD` and cached `origin/master` both point at `1ef0648d3f9`; the context
work in this checkout is uncommitted. Broader package tests were not rerun in
this continuation.
