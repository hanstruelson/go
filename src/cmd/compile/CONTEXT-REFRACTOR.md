# Compiler context and reusable compilation audit

## Current checkout

The working branch `work` contains `c41f35caca0`, which adds the `go build
-JIT` option, toolchain/package bundling in `cmd/go`, and linker support in
`cmd/link`. The compiler-context checkpoint `ae057685c22` has been pushed to
`origin/master`. The JIT commit does not change `cmd/compile`; bundling source
and tools does not add a reusable compiler request API or a code loader for
arbitrary source strings.

## Where one compilation starts and ends

`cmd/compile/main.go` chooses a target architecture and calls
`gc.Main(archInit)`. `gc.Main` performs both command-line setup and the whole
package compilation: it creates `obj.Link`, parses process-global flags,
initializes target and universe state, asks `noder.LoadPackage` to read source
filenames, runs frontend and optimization passes, generates object code, and
writes output files. `base.Exit` eventually terminates the command. There is no
callable request/result boundary today.

## Mutable state that belongs to a compilation

- `base`: `Ctxt`, `Flag`, `Debug`, source position, diagnostics, and output/exit
  behavior. Most compiler packages read these directly. The timing collector is
  now created in `gc.Main` and passed into the frontend; the former `base.Timer`
  singleton is removed.
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
  that compilation's writers. Type-kind and operator classification tables
  are initialized once and shared as immutable data.
- `noder`: package readers, object/body lookup maps, pending body/dictionary
  work, wrapper-generation caches, and generated-name counters. These survive
  `LoadPackage` today.
- `ssagen` and backend passes: selected architecture, SSA configuration and
  caches, environment-derived debug settings, and output hooks. SSA consistency
  checking, its randomization seed, intrinsic debug/disable options, and
  function dump selectors are copied into each target and read from that
  context by backend workers. The active SSA pass slice and its mutable maps
  are cloned per target; command-line phase/pass option parsing remains
  process-wide.
- command and output plumbing: `flag.CommandLine`, process environment,
  `os.Stdout`/`os.Stderr`, logging, telemetry, profiles, and process exit.
- `cmd/internal/obj`: an `obj.Link` is already constructed per `gc.Main`, but
  shared compiler helpers reach it through `base.Ctxt`.

A source scan found at least 158 non-test, top-level single-name `var`
declarations across the main state packages (`base`, `types`, `typecheck`,
`ssagen`, `ir`, `noder`, and `gc`); grouped declarations are not included. Many
are immutable tables, so this is a classification task, not a count of fields
to move. A scan of the selected global selectors found 1,712 matching
non-test source lines across `cmd/compile/internal`. A blind textual rename
would miss unqualified package-local globals, function-value callbacks, and
state access hidden behind helpers.

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
`inline.Context`. This is a real frontend migration slice, but it is not yet a
request/result API: it does not return compiled object bytes, and `base`,
`types`, architecture setup, SSA state, and output/exit behavior still rely on
globals.

The context migration now puts the compiler's pseudo-runtime declarations,
runtime type descriptor symbols and ABI layouts, generic shape symbols,
anonymous receiver symbols, the blank identifier node, generated runtime
`LSym` fields, pointer and register sizes, stack allocation limits, and
intrinsic dispatch on each target package. Runtime lookup, reflection, static initializer analysis, SSA,
stack-map generation, and architecture generators use the target context.
The escape-analysis formatter used by IR dumps is target-owned too.
Default literal selection, constant indexes, closure layouts, and ABI pointer
bitmap generation also take target state explicitly. The synthetic
`internal/runtime/math.MulUintptr` symbol is interned in the owning target's
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
global shape package have been removed. The full compiler tree builds with:

```sh
GOROOT=/workspace/go GOCACHE=/tmp/go-build-cache /workspace/go/bin/go build cmd/compile/...
```

No tests were run.

The baseline search found 1,712 lines that read selected singleton state.
The latest broad selector scan reports 41 non-test lines matching the
selected `base` and `types` names, including comments, compatibility
fallbacks, and target setup. Type sizing, deferred layout, basic-type
universes, and package registries are now target-owned. Remaining blockers
include diagnostics and error counts, command-line parsing, environment and
profile configuration, output files, and process exit. The in-memory API
remains a frontend loading boundary; it does not return a complete object
file from arbitrary source.

## Recommended migration

1. Define a request/result API around a package compilation. A request needs
   named source files (Go packages can require multiple files), package path,
   target, language/build options, import/export data, and diagnostics/output
   sinks. Return object bytes and structured diagnostics rather than exiting
   the process.
2. Keep `main` as a thin adapter that translates flags and files into that
   request. Move initialization and phase orchestration into a `gc.Compiler`
   or equivalent per-request context.
3. Migrate state by subsystem, starting with remaining `base` diagnostics and
   command setup, then remaining `ir` hooks, `ssagen` and optimization passes,
   and finally object writers.
   Pass context explicitly through phase APIs and callbacks.
   Keep immutable opcode/type tables and safe shared caches at package scope.
4. Make diagnostics, timers, profiles, and object output request-owned. Remove
   direct flag parsing, environment mutation, and process exit from the
   reusable path.
5. Add repeated-compilation and concurrent-compilation coverage after each
   subsystem migration. For a true JIT, separately decide how to link and
   load generated code, resolve relocations, and register GC/unwind metadata;
   `cmd/compile` currently emits object files and does not provide that loader.

## Shortcut assessment

Find-and-replace is suitable only after a context is threaded into a bounded
package: it can rewrite mechanical selectors such as `base.Ctxt` to a context
field. It cannot correctly add context parameters through the compiler's call
graph, handle callbacks, or determine whether a package global is immutable.
The efficient safe shortcut is an AST/type-aware codemod that propagates a
context parameter through direct calls, followed by manual handling for
callbacks and cross-package APIs. The current refactor slice is formatted and
`go build cmd/compile/internal/gc` succeeds. No tests have been run.
