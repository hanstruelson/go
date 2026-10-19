# In-memory JIT linker

## Current source path

The source tree contains an implementation attempt for the general path below.
This summary is based on code inspection, not proof that arbitrary snippets,
imports, captures, or unloading work. A separate TypeScript Go-target client
exercises a narrow Linux/amd64 dynamic-RegExp path through jit.Eval; its scope
is summarized below.

The current jit.Eval identifies the importing source file, parses statements
at runtime, compiles them in-process, links compiler output in memory, maps the
result, registers runtime function metadata, and executes through an amd64
trampoline. The snippet has no package clause or imports. Its generated
function uses the caller package identity, and references to imported symbols
are intended to bind to the host executable.

The linker metadata prototype contains selected references to actual linked
package symbols. A JIT call to a host package function and access to a host
global are intended to use the same code and storage as static code. The
prototype does not yet have a complete linker-generated root set or host
symbol manifest, and its current metadata does not prove arbitrary import
coverage.

The current runtime returns a manual Code handle. Code.Close unmaps after
calls made through that handle finish, but it cannot discover callbacks,
function values, goroutines, dynamic types, profiles, or foreign references
created by evaluated code. It is not safe automatic unloading for general Eval.
The proposed production lifetime contract is below.

### Narrow dynamic-RegExp evidence

The TypeScript client parses a runtime pattern, emits a Go matcher expression,
and calls `jit.Eval` with a bridge to a host-package function. Its four-file
stress fixture was transpiled and its generated executable was run three times
on Linux/amd64. Each run checked 32 imported-factory patterns, 32 direct
constructor patterns, 16 calls to `RegExp.compile`, global `lastIndex` behavior,
capture extraction, and state shared by imported TypeScript modules. All
reported results matched expectations. The fixture's call graph requests 82
runtime compilations per process (including the initial pattern before the 16
recompiles); that number is derived from the fixture, not a JIT counter.

This is evidence that a restricted function-expression snippet can be parsed,
compiled, linked to one host bridge symbol, mapped, and called repeatedly in
one process. The imported TypeScript state is not a test of shared Go package
globals. The run does not establish general imported-package linking, lexical
capture, broad ECMAScript RegExp compatibility, automatic unloading, or the
GC owner protocol. The temporary RegExp path retains each `*jit.Code` mapping
for process lifetime.

## Target architecture

The first complete target is the toolchain and executable running this JIT:
Linux/amd64. ARM64 is a separate backend after Linux/amd64 contracts and
fixtures are stable. Maps, pointers, interfaces, channels, and goroutines use
ordinary compiler object symbols, relocations, type data, and runtime calls;
they do not receive feature-specific linkers.

The intended pipeline has five contracts:

1. **Build snapshot.** The compiler emits a versioned package snapshot with
   each source file's import scope, declarations, and linker-visible package
   symbols with ABI/type hashes. The normal link roots the relevant symbols
   and emits references to the exact selected host definitions.
2. **Eval scope and invocation.** A compiler-generated call-site descriptor
   supplies package/file scope and typed capture slots. Runtime source is
   parsed and type-checked against that scope, then compiled to a fresh
   function with a typed capture frame.
3. **Generic object linking.** The in-memory linker reads only object code
   produced for the snippet, binds undefined symbols through the host
   manifest, applies validated target relocations, and creates runtime
   function/stack/type metadata. It has no feature-specific cases.
4. **Runtime module registration.** The runtime publishes a complete
   generation-tagged code/metadata index atomically. Executable text and
   immutable tables live in an external mapping. Pointerful package state,
   owner cells, dynamic types, and itabs live in typed GC heap objects.
5. **Lifetime.** Every callable, live frame, escaped dynamic value, and
   cross-module reference carries a GC edge to its owner or is explicitly
   pinned. A JIT-specific cleanup record is scheduled when an owner is
   unreachable; a worker unpublishes metadata, drains readers, then unmaps.
   Raw uintptr, unsafe, and foreign escapes are pinned or rejected.

This keeps code bytes out of GC object scanning and avoids changing every Go
heap sweep/reuse path to protect executable permissions. GC reachability still
controls the mapping lifetime through the owner object. It does not make the
mapping bytes themselves a Go heap object.

Same-package declaration scope is the first implemented stage: saved
declarations are supplied to the in-process compiler, while existing package
symbols resolve back to the host binary. The next work is compiler-emitted
lexical capture descriptors, complete host manifests, dynamic type/cache
registration, and general owner propagation. Each stage needs an end-to-end
fixture that checks identity and runtime behavior, not only successful
compilation.

## Cross-architecture production map

The current source tree has compiler backend initialization files for the Go
architectures listed below, but the JIT linker, executable-memory mapper, and
trampoline are implemented only for Linux/amd64. Compiler support is not JIT
support: a target is enabled only when its relocations, ABI, memory mapping,
runtime metadata, and execution path pass the same conformance suite.

Keep four boundaries separate:

1. **Shared compiler and package metadata.** Reuse the in-process compiler,
   Go object format, package snapshots, symbol manifest, and ABI checks. Keep
   metadata target/version tagged; never treat compiler object layouts or
   runtime `moduledata` as a stable cross-version public ABI.
2. **Shared link planning.** Reuse Go's object reader, symbol graph, GC program,
   type, pcln, and module metadata generation wherever those pieces can be
   called independently. The current `cmd/link` high-level linker is an
   executable linker, not an in-memory JIT API. If a required phase is coupled
   to final executable output, extract a narrow internal builder and have both
   the normal linker and JIT call it. Do not copy or maintain a second version
   of Go's type/GC metadata rules.
3. **Architecture backend.** Translate the Go toolchain's relocation kinds
   into target instruction/data patches; define pointer size, endianness,
   alignment, function ABI, branch reach, veneers/GOT rules, trampoline ABI,
   and instruction-cache synchronization. Preserve the source relocation kind
   until this backend validates and lowers it. Unsupported forms must fail
   before a mapping becomes executable.
4. **Operating-system memory backend.** Create and protect external JIT
   mappings, flush instruction caches when required, and enforce the OS's
   executable-memory policy. The JIT runtime owns mapping reservation,
   accounting, retirement, and release. This is a GOOS boundary, not a GOARCH
   relocation rule.

The serialized image should carry a target-neutral module description plus
validated target relocation records, not pretend that machine relocations are
portable. The runtime registration API should accept complete code/metadata
ranges, stack maps, types, itabs, and function tables in one version-checked
descriptor. The first unloadable format has no pointerful module-global root
ranges; those globals are owner-held typed heap state. Registration publishes
a complete module atomically; code must not run before registration succeeds.
Relocation work happens in private RW/NX mappings before publication. The
GC-visible owner controls lifetime; unload still requires reader quiescence and
cache invalidation.

### GOARCH rollout

The table lists architectures exposed by this toolchain's `dist list`. It is a
backend plan, not a claim that all GOOS/GOARCH combinations support JIT.

| GOARCH family | Target-specific work | Rollout |
| --- | --- | --- |
| amd64 | Existing Linux backend: x86-64 relocations, RIP-relative range constraints, GOT slots, Go entry trampoline | Stabilize the complete GC/type/runtime contract first |
| arm64 | AArch64 page-relative relocation pairs, branch range/veneers, ABI and cache synchronization | First architecture backend after amd64 |
| 386, arm | 32-bit pointer layout and ABI; target relocation widths and branch/literal reach; ARM build variants | After arm64, if 32-bit targets remain in product scope |
| loong64, riscv64 | Target relocation pairs, branch encodings/ranges, register ABI, and cache synchronization | Add individually from compiler-emitted relocation fixtures |
| ppc64, ppc64le | Endianness and ABI-specific TOC/addressing rules, relocation range, and trampoline ABI | Add as separate ABI variants, not one assumed backend |
| mips, mipsle, mips64, mips64le | 32/64-bit layouts, endianness, HI/LO and GOT relocation behavior, ABI variants | Add only with native or emulated execution coverage |
| s390x | Big-endian data and PC-relative relocation forms, branch reach, and calling convention | Add from compiler-output fixtures and runtime CI |
| wasm | Function indices and a WebAssembly execution/module model rather than native PCs, `mmap`, and machine trampolines | Separate design; do not force it through the native image mapper |

### GOOS rollout

The first release target should remain Linux/amd64. Add Linux/arm64 second,
then add other GOARCH backends on Linux where executable mappings and runtime
support are available. Once those contracts work, implement OS mapping backends
separately for selected GOOS targets:

| GOOS group | Additional execution constraints |
| --- | --- |
| Linux, Android | W^X and executable-memory policy for runtime-created mappings (the runtime may use `mmap`); per-architecture instruction-cache synchronization |
| Darwin, iOS | Hardened-runtime and `MAP_JIT`/entitlement rules; some applications cannot request JIT memory |
| Windows | `VirtualAlloc`/`VirtualProtect`, W^X, control-flow policy, and unwind/runtime integration |
| FreeBSD, NetBSD, OpenBSD, DragonFly, illumos, Solaris, AIX | Per-OS virtual-memory APIs, executable-page policy, ABI and linker conventions |
| Plan 9, js/wasip1 WebAssembly | Different process or code-execution models; require a separate feasibility decision |

For hardened platforms, prove that the runtime can create and manage
executable mappings under the platform policy. For example, Darwin MAP_JIT
requirements and entitlements may constrain the mapping backend. Account these
bytes in the JIT/runtime memory budget. Do not advertise a target based on
cross-compilation alone. Each enabled
GOOS/GOARCH pair needs native or emulated execution tests for compiler-emitted
relocations, overflow/rejection cases, W^X mapping, trampoline calls, stack
maps, forced GC, type/interface behavior, concurrent registration, and cleanup
quiescence. The CI matrix should report compile-only and execute-tested targets
separately.

### Production decision

The deployment target is one self-contained application executable per
GOOS/GOARCH target. Matching toolchain versions means that the compiler, linker
metadata, and runtime code used to build that executable agree with each other;
it does not mean the executable looks for a Go installation on the user's
computer. `jit.Compile` calls the in-process compiler API, the program's
ordinary Go link includes the reachable JIT, compiler, and runtime package
code, and the custom linker maps generated code from memory. Runtime evaluation
must not start `go`, `compile`, or `link`, and must not read `GOROOT`.

The executable needs a small amount of build-produced state: package/import
scope for runtime type checking, a host-symbol manifest whose references keep
the real statically linked package symbols alive, and toolchain/target identity
checks. That is application link metadata, not a bundled Go distribution. The
compiler's output archive is a transient byte slice passed directly to the
in-memory linker; it is not copied to disk or shipped as a second compiler.

The current worktree has a schema/importer change intended to remove imported
`.a` archives from runtime metadata. Build-time extraction still reads the
exact selected package archive to obtain Go export data; the runtime compiler
is intended to receive only that export payload. This source change has not
been built or exercised here. The linker-root and complete host-symbol
manifest described below are not implemented by the current static list of
generated references.

## Master plan: export-only package snapshots and host linking

### Design decisions

1. **Ship one executable per target.** The ordinary Go link includes the JIT,
   in-process compiler, runtime support, and each statically linked host
   package once. The user needs no Go installation, `GOROOT`, compiler
   subprocess, plugin, or sidecar archive.
2. **Use Go export data for compiler semantics, not package object code.** At
   application build time, read the exact package inputs selected by the
   compiler and extract their package headers and unified export data. Store
   those records in a versioned JIT metadata section. A record includes the
   import path, declared package name, export-data version and fingerprint,
   dependency identities, and the compiler/target identity. Its payload has
   type and generic-instantiation information the compiler needs, but no
   package object code.
3. **Keep Go's export representation; own only the container and import API.**
   Do not invent a second Go type system or copy package implementations into
   a private format. Extend the in-process compiler with a package-import
   provider that reads the embedded export records through the existing Go
   export-data decoder. The provider resolves dependencies from the same
   embedded snapshot. It must not wrap the payload in an `.a` archive as its
   runtime interface.
4. **Keep type information separate from executable symbol binding.** Export
   data answers “what does this name mean?” A distinct host link manifest
   answers “which live symbol implements it?” For every source file that
   imports `jit`, form an Eval-visible package set from that file's named and
   dot imports. An unused named import in that file is still available to
   Eval; a blank import remains initialization-only. The normal linker's
   dead-code pass must root the caller package and all named/dot imported
   packages visible at that Eval site, then root every symbol in their
   linker-resolved library dependency closure. This includes private helpers,
   generic support, type descriptors, method wrappers, and data. This makes arbitrary runtime
   instantiations resolvable without keeping package object files in JIT
   metadata. Do not disable dead-code elimination globally: packages outside
   Eval scopes remain normally prunable. The linker also emits the host symbol
   manifest from the retained symbol set during final link layout, with
   relocations to the actual executable functions, variables, and type data.
   The manifest records package/symbol
   identity, kind, ABI/type identity, and the build-selected package
   fingerprint; it contains process-correct references, not addresses copied
   from another build.
5. **Never load imported package code twice.** The JIT linker consumes only
   object code produced for the current snippet in memory. Its undefined
   references bind to the host manifest. Package initialization runs once in
   the ordinary executable, and JIT reads and writes use the same functions
   and global storage as statically compiled code.
6. **Treat toolchain and target identity as a checked contract.** The export
   decoder, compiler object reader, symbol ABI, and runtime module format are
   internal Go contracts. The metadata records their version and target; the
   runtime rejects mismatches. This is co-versioning within the executable,
   not a dependency on a Go installation at deployment time.

### Eval-scoped linker roots

The retention decision belongs in the ordinary executable link, before its
dead-code pass. It is not a request to make every Go build greedy. The compiler
records the import scope of each Eval-enabled source file in a linker-readable
root record. The linker matches each resolved import path and package
fingerprint to the exact package library loaded for that build, then roots its
symbols and the package closure required by its export data. This includes
unexported implementation helpers needed by generic bodies the JIT may
instantiate. The root record is consumed by the linker's reachability pass;
the `.a` input is still used only as a build-time input and is never copied
into the executable as metadata.

The linker emits the runtime host-symbol manifest from the same resolved
symbol graph. Each entry carries a stable package/symbol identity and ABI or
type identity plus a linker relocation to the final executable symbol. Data
entries point at the existing mutable global storage. Function entries name
the Go ABI the JIT object expects. Type and method support entries identify
the host type descriptors and wrappers. The JIT linker resolves against this
manifest and rejects missing or mismatched identities; it never loads a
package implementation or runs its initializer a second time.

This is intentionally wider than keeping only references observed in static
source. Eval source is a runtime string, so static call analysis cannot know
which exported API will be used. The root set is therefore all symbols in the
package closure visible from Eval-enabled files, while unrelated packages and
unrelated build configurations remain prunable. A blank import exposes no
package names to the snippet, so it retains ordinary initialization behavior
without rooting that package's callable API.

### Build and runtime pipeline

1. **Capture the authoritative package graph.** During the normal build, use
   the compiler action's resolved package-file/import configuration for direct
   and transitive imports reachable from Eval-enabled files. Do not rediscover
   packages from a later `GOROOT`, module cache, or source tree. Extract export
   records from those exact inputs, deduplicate identical payloads by
   fingerprint, record all dependency identities needed by the decoder, and
   hand the Eval-visible package set to the normal linker's root pass. The
   linker must compare like-for-like identities; an export-data fingerprint
   cannot be compared to an unrelated build-action or object fingerprint.
2. **Serialize a compact snapshot.** Store package import records, source-file
   import scopes and aliases, same-package declarations needed for Eval,
   package-root identities, and build/target fingerprints. The normal linker
   creates the final host-symbol manifest from the retained object symbols;
   source-generated references alone cannot enumerate private generic-body
   helpers. Do not serialize imported `.a` archives, object files, or raw
   process addresses. Keep compiler output produced by an Eval transient in
   memory as a separate object: that is generated code, not embedded package
   metadata.
3. **Parse and type-check at Eval time.** Parse the statement text at runtime
   in the caller's package and file import scope. Resolve imported identifiers
   from the embedded export provider, including transitive exports and generic
   bodies. Resolve same-package declarations from the caller snapshot. No
   package clause or import declarations are required in the snippet.
4. **Compile and link in memory.** Run the embedded compiler API and pass its
   resulting object data directly to the custom linker. Resolve each imported
   function, variable, type descriptor, method wrapper, or runtime symbol by
   package/symbol identity and ABI checks. Bind references through the
   linker's manifest to the host executable's retained symbols; never extract
   object code from package export records or initialize another copy.
5. **Register a complete runtime module before execution.** Publish generated
   text together with relocations, stack maps, function tables, type
   descriptors, itabs, and any required runtime tables. The first unloadable
   format has no pointerful static module roots; pointerful package state is
   owner-held typed heap data.
   Registration either succeeds atomically or an unpublished external
   mapping is released before its owner is dropped. This is necessary
   for calls, stack growth, panic unwinding, reflection, interface behavior,
   and forced garbage collection.
6. **Add lexical capture as a typed ABI.** Compiler-generated call-site
   descriptors enumerate all live values in scope, including receiver,
   parameters, results, locals, loop/switch/select variables, and closure
   captures. A typed capture frame preserves addresses, aliasing, shadowing,
   and GC pointer maps; runtime stack inspection is not used to guess at
   optimized values.
7. **Make the GC own automatic lifetime.** The owner graph keeps code live
   through callbacks, goroutines, dynamic types, and interfaces. A JIT-specific
   cleanup record is queued when sweep finds the owner unreachable; a reaper
   establishes reader quiescence before unmapping. General Eval does not offer
   synchronous close.
8. **Add architecture backends behind shared contracts.** Keep export import,
   symbol identity, reachability, and runtime metadata shared. Target backends
   own relocation lowering, ABI, executable-mapping policy, cache flush, and
   trampoline details. Linux/amd64 is the first complete target; Linux/
   arm64 follows only after the same conformance gates pass.

### Relocation and object compatibility contract

The production linker does not maintain a fixture-derived subset of Go
relocations. The pinned compiler/object version and the matching `cmd/link`
implementation are authoritative. Extract a reusable in-memory relocation
phase from `cmd/link` that consumes the same Go object relocation records and
symbol identities, but writes into a private `ModuleImage` instead of an
executable output file. The Linux/amd64 backend supplies the final section
addresses and host-manifest bindings to that shared phase. This preserves
Go's handling of relocation variants, addends, symbol ABI selection, weak
references, method offsets, GOT slots, branch reach, and overflow checks.

The source-to-implementation record must account for every relocation kind
that the pinned `cmd/compile` can emit for an Eval function and its compiler
generated helpers. Reachability-only markers affect symbol selection but do
not patch bytes. Any linker-only, external-object, or dynamic-loader form that
cannot be emitted by the Go compiler object input is outside the JIT object
format. A compiler-emitted form is never silently ignored: until the shared
phase handles it with the same semantics as normal linking, compilation fails
before mapping publication and that source feature is outside the supported
profile. The runtime format fingerprints the compiler/object schema and
relocation semantics, so a changed toolchain cannot reinterpret an old image.

The current `imageRelocationFor` switch and hand-applied amd64 relocations are
prototype code, not the production compatibility contract. Preserve each
source relocation kind through object parsing, validate source/target section
bounds and integer overflow, apply relocations only in a private RW/NX
reservation, and check every target-specific displacement before sealing
text RX. Generated coverage should include the full relocation set exercised
by functions, closures, interfaces, generic dictionaries, types, methods,
runtime calls, and host variables; the source audit, not the fixture count,
defines completeness.

### Completion gates

- The serialized metadata contains no imported package `.a` archives or
  imported object code. An inspection test verifies this, and executable size
  is compared against the current archive-bearing build.
- An import used only by an Eval-enabled file is accepted by the normal
  compiler and its package closure is retained in the executable, including
  private helpers needed by generic instantiations. Eval can call an exported
  symbol that has no static source call; packages outside Eval-enabled import
  scopes remain subject to ordinary dead-code elimination.
- A runtime import provider type-checks direct, transitive, aliased, dot, and
  generic imports directly from export records, with no synthetic archive
  envelope. Corrupt, missing, or mismatched package fingerprints fail before
  code is mapped.
- An integration program calls an imported package from Eval and proves
  shared identity: a package global incremented before Eval is incremented by
  the snippet, and both sides observe the same storage and address. Package
  initialization is observed exactly once.
- The process runs successfully with no usable Go tools, `GOROOT`, or package
  files at runtime. The compiler object produced by Eval remains in memory and
  is passed directly to the custom linker.
- Forced-GC, stack-growth, panic/traceback, type/interface, malformed-input,
  repeated-Eval, concurrent-call, and explicit-close tests pass before a target
  is declared supported.

A standalone JIT package cannot safely promise compatibility with arbitrary
stock Go releases because object formats, `moduledata`, stack maps, and runtime
type caches are internal contracts. Upstreaming stable hooks would reduce
toolchain maintenance; until then, the application binary carries matching
compiler/runtime code and rejects mismatched embedded metadata. This minimizes
the fork: reuse normal compiler and linker metadata algorithms, extract shared
internal builders only where the executable linker has no callable phase, and
keep architecture and OS differences in small, explicit backends. Complete
the shared module and GC contracts before pursuing every target.

### Implementation order and confidence boundary

This is the implementation sequence I would commit to. Each gate is a
dependency of the next one; skipping a gate would make later failures hard to
distinguish from link or runtime bugs.

1. **Close the compiler/link context audit.** The reusable compiler API in
   this tree already constructs fresh target, IR, frontend, type-universe,
   and linker contexts, and its source has concurrent-request coverage. Run
   that coverage and audit every mutable value reachable from the request
   path. The invariant is no cross-request mutable compiler state; immutable
   tables and explicitly synchronized caches do not need to be eliminated.
   The command-line adapter's process state is a separate question from the
   reusable API. Give the in-memory linker the same per-request isolation.
2. **Finish the build snapshot and source scope contract.** For each source
   file that imports `jit`, record that file's named and dot imports, aliases,
   package identity, declaration/type scope, and generic bodies that may be
   instantiated. A blank import only requests normal package initialization.
   A non-blank import in a JIT-enabled file is accepted even when the static
   source never names it; the unused-import rule is relaxed only for that
   file. Other files' imports are not in scope because Go imports are
   file-local.
3. **Add linker-readable import roots to the normal Go link.** The compiler
   writes a versioned root record containing the exact Eval-visible package
   paths and package identities, including the package containing the Eval
   call. `cmd/link` consumes it after loading the libraries and before
   dead-code elimination. Its loader already associates
   symbols with package paths and its dead-code pass already has an explicit
   root phase. Root every symbol in the caller package and every named/dot
   imported package visible at that Eval site, then root every symbol in the
   linker's already-resolved library dependency closure so late generic
   instantiations can reference private support code. Keep packages outside
   that closure under ordinary dead-code elimination. Generated references alone cannot name all private
   symbols and data, so they are not the root mechanism.
4. **Generate the host-symbol table from the retained link graph.** After the
   root pass establishes the retained set, the linker emits records keyed by
   full object-symbol identity (package, linker name, ABI/version, symbol
   kind, and type identity where available). It emits ordinary relocations
   for function, variable, type, and required runtime metadata references, so
   the executable's relocation process supplies ASLR-correct addresses. The
   runtime table contains no addresses copied from another process and no
   imported package object code. JIT-generated undefined symbols resolve
   against this table, including private support symbols reached by generic
   instantiation. Missing, ambiguous, or ABI-incompatible references fail
   before executable memory is published.
5. **Use one runtime compiler namespace and an explicit parser entrypoint.**
   `jit.Eval` remains the user API. The Go compiler rewrites each call to
   attach a call-site/file-scope descriptor; it must not guess the file from
   runtime paths or expose package/import boilerplate to the caller. The
   source string is parsed on the Eval call. Add a parser entrypoint for a
   statement list, or an equivalent parser-owned wrapper that preserves
   source positions. The checker receives the caller package, current file's
   imports, current package declarations, and embedded Go export records.
   Same-package declarations that denote existing functions or variables
   must lower to external symbol references; a synthetic `var` definition
   must never allocate a second copy of a host global. Package-local generic
   bodies needed for new instantiations must be available as compiler
   metadata, not silently assumed to be in the host executable.
6. **Define the capture ABI before claiming lexical Eval.** The compiler
   rewrites the call site to pass a typed frame for values in lexical scope.
   Entries need name and shadowing identity, type identity, address/value
   semantics, and GC pointer layout. Address-taken variables must remain the
   same storage so assignments are visible to the caller. Values that can be
   retained by a returned callback need heap lifetime; a pointer to a caller
   stack frame cannot outlive that frame. Specify supported contexts
   (ordinary functions, loops, closures, generic functions) and reject
   unsupported contexts explicitly rather than pretending stack inspection
   can recover optimized locals.
7. **Make the JIT object linker a Go module linker, not an archive loader.**
   Reuse Go's object reader, symbol/relocation model, ABI data, GC program,
   type metadata, pclntab generation, and target relocation rules. Refactor
   internal final-link builders where useful. The JIT linker consumes only
   the new object bytes, resolves every host relocation through the manifest,
   lays out code/rodata and pointer-free module sections, applies relocations,
   builds runtime tables, and allocates pointerful package state as typed,
   owner-held heap objects before sealing executable pages. The first
   unloadable format has no pointerful static module roots. Unknown relocation
   or symbol kinds fail before registration; there is no fallback subprocess, plugin,
   temporary source/object file, or package archive.
8. **Register complete runtime metadata atomically.** The runtime descriptor
   covers text and pointer-free metadata ranges, function/stack maps, types,
   typelinks, itabs, and tables used by stack walkers,
   traceback, panic, interfaces, reflection, and the collector. Registration
   follows the runtime synchronization protocol (including stop-the-world
   when required) and publishes only after validation. Pointer-bearing JIT
   globals live in owner-held typed heap state, not module root ranges. Reuse
   runtime module validation and type/itab initialization where it applies,
   but do not copy the plugin file loader or assume plugin registration is
   itself a JIT API.
9. **Keep unload conservative.** The current Code.Close cannot discover
   callbacks, goroutines, function values, or type/cache references. General
   Eval exposes Release, which drops its handle edge; GC cleanup retires the
   module after all owner-carrying values disappear. A synchronous Close is
   limited to a compiler-certified scoped invocation. Raw-PC and foreign
   escapes pin until explicitly deregistered. Never unmap code that a Go value,
   active frame, runtime cache, or external reference can still name.
10. **Bring up targets one at a time.** Complete and test the whole contract
    on Linux/amd64 before Linux/arm64. Shared code owns metadata and link
    planning; each target backend owns relocation encoding, ABI/trampoline,
    executable memory, W^X, and instruction-cache maintenance. Cross-building
    is not target support.

The linker insertion point is concrete: `cmd/link` loads libraries, computes
init tasks, then runs dead-code elimination before layout. The root-record
consumer belongs at that root phase. Its package/symbol map also supplies the
retained set for a relocation-backed manifest. This is a bounded extension to
the existing link pipeline, not a second archive-discovery mechanism.

I am confident this is the right architecture for a self-contained
executable on a fixed Go toolchain and target. I am not confident that the
current source implements it yet: package-wide roots and manifest generation,
complete runtime type/GC registration, lexical capture, and safe cache-aware
unloading remain substantive work. A production claim requires the completion
gates above to pass on a clean build; source inspection alone cannot establish
that.

### Research findings from this checkout

These findings are from tracing the source at the current repository revision;
they describe concrete seams and missing contracts. They are not build/test
results.

| Area | Source evidence | Consequence for the design |
| --- | --- | --- |
| Reusable compiler | `gc.Compile(Request)` creates request-local target/frontend/type/linker objects. `gc/api_test.go` contains concurrent cross-target requests and checks legacy process state is unchanged. | The compiler context migration is a foundation already present in source. Verify the test and audit mutable caches; do not spend time removing immutable globals. |
| Source visibility | Go import scope is per file. Current metadata records imports per source file and permits unused imports only for files importing `jit`. | The compiler call site must identify its exact source file; imports from a different file in the package are not visible to that Eval. |
| Call-site identity | Current `Eval` uses `runtime.Caller(1)` and filename matching. | Replace path inference with a compiler-emitted scope/call-site descriptor. Runtime paths can be transformed by `-trimpath` and do not carry lexical-variable/type-parameter scope. |
| Export import | The in-process request accepts package export records and `noder` validates target header, export version, and pkgbits fingerprint before using the existing decoder. | This is the correct code-free package type-checking path. Build-time extraction may read the chosen archive; no archive belongs in the runtime snapshot. |
| Link root point | `cmd/link` runs `loadlib`, `inittasks`, then `deadcode`; the dead-code pass has an explicit initial-root phase. `sym.Library` retains `Imports`, and loader symbols carry package identity. | A compiler-produced root record can be consumed before dead-code elimination. The linker can traverse its already-resolved library graph and root symbols for only Eval-visible packages plus their implementation dependencies. |
| Host binding | `gc/jitmeta.go` synthesizes ordinary source references for local declarations and selected exported imported functions, variables, methods, and types; `jit/metadata.go` resolves function values to PCs with reflection. `cmd/link` has no JIT root or manifest integration in this worktree. | This keeps selected symbols alive for the prototype, but it is not a complete linker manifest: it omits private helpers and does not carry the exact linker-selected ABI symbol identity needed for arbitrary object relocations. |
| Runtime module publication | `runtime.moduledata` carries text, function/PC tables, data/BSS ranges and GC masks, types, itabs, and module links. `modulesinit` builds GC masks and adds global-root scan work. The current JIT registration fills function/PC tables and publishes the module list, but not the full data/type contract. | Registration must be extended or factored through shared runtime code before arbitrary snippets with dynamic types or itabs can be supported. The first unloadable format routes pointerful globals to owner-held heap state instead of module root scanning. |
| Runtime type caches | `typelinksinit` records module typelinks and keeps typemaps in `pinnedTypemaps`; `moduleToTypelinks` caches decoded links; itabs are inserted into a process-wide table. These paths have no general module-removal API. | Stop-the-world alone cannot make unloading safe. New-type/itab modules must stay pinned until every cache and escaped Go value can be invalidated or tracked. |
| Plugin comparison | The inspected `runtime.plugin_lastmoduleinit` validates the loaded module, updates module/type state, and adds itabs under the itab lock; it does not itself call `stopTheWorld` and the plugin API has no unload operation. | Plugin code is a source of data-layout and validation patterns, not proof of a JIT publication or reclamation protocol. The JIT runtime contract needs its own audit. |
| Target linker | The current custom object linker is a hand-maintained Linux/amd64 subset with explicit rejection paths. Normal `cmd/link` owns mature object loading, ABI, relocation, type, GC, pcln, and moduledata algorithms but has no incremental in-memory module API. | The long-lived design should factor reusable linker/runtime builders where practical and keep one explicit target backend. It must not claim universal relocation support from a selected fixture matrix. |
| Trust boundary | `Eval` executes generated Go code in the host process with its imports, filesystem/network permissions, and runtime access. Go has no safe in-process mechanism to terminate arbitrary runaway code. | Document Eval as trusted code. Adversarial code needs an OS/process sandbox; compile limits alone do not sandbox execution. |

The loader-root closure should be derived from `cmd/link`'s resolved
`Library.Imports` graph, not reconstructed from a second package resolver. The
root record names only packages visible to Eval at a call site. The linker
uses the already-selected library objects and their graph for implementation
dependencies, then emits a versioned manifest for the retained symbol set.
If fingerprints are carried across this boundary, use one canonical
linker-visible identity or validate both representations explicitly; never
assume the pkgbits hash and a linker/archive build fingerprint are
interchangeable.

The remaining uncertainty is now concentrated in implementation proof
obligations, not in an alternate architecture: demonstrate that package roots
retain every symbol class generated by imported generic bodies; demonstrate a
manifest can represent and relocate every allowed host symbol/ABI; and audit
runtime registration, concurrent GC, and every persistent type/function cache
before reclaiming any mapping. These require narrow source experiments and
then tests on Linux/amd64. A written plan cannot settle them by assertion.

### Research update: owner reachability and external mapping cleanup

This is a source review of the current checkout, not a claim that the
prototype safely unloads general Go code. It revises the earlier proposal to
put executable pages inside the Go heap.

#### Architecture decision

Use ordinary GC-visible Go objects for ownership and dynamic values. Keep
machine code, immutable link tables, and runtime PC metadata in a separate
runtime-created mapping. The owner holds the mapping handle as a non-pointer
resource record. Every Go reference that needs the code or metadata reaches
the owner through a typed field, a JIT allocation descriptor scanned as an
extra GC edge, or an explicit pin.

This avoids making Go heap spans executable and avoids changing every sweep,
zeroing, clobber, scavenging, and reuse path. The mapping itself is not scanned
as a Go object. Its liveness is represented by the owner graph, and the runtime
cleanup record keeps the mapping allocated until its registry entries and
readers are gone.

The source supports this direction. `runtime.AddCleanup` demonstrates the
external-resource pattern: callback/argument roots are separate from the
target, and sweep can discover a dead target before asynchronous teardown.
The JIT does not use `AddCleanup` or its shared worker for executable
resources. It adds a dedicated sweep special that queues a control record in
the unit's RW/NX page. The mapping remains mapped until the reaper
finishes. This is unsafe for executable pages inside the target's Go heap
object, because sweep could reuse those pages before asynchronous teardown.

The public cleanup contract allows arbitrary delay and does not promise
execution before process exit. The JIT therefore needs its own sweep hook,
non-blocking queue insertion, dedicated reaper, and generation-scoped
wait/drain operation for explicit memory pressure. A runtime mode without
that hook must fail closed or pin the module; it cannot claim automatic
unloading.

#### Owner and runtime roots

A jitOwner is a non-zero, pointerful, non-tiny Go heap object. It contains the
evaluation generation, owner-held typed package state, dynamic metadata
cells, dependency owners, counters, and a handle for the external mapping. It
does not contain executable bytes. No process-global scanned list may retain
jitOwner.

A control record is reserved in the unit's private RW/NX page before
publication. It stores the generation, mapping address/length, runtime index
token, and teardown state. It contains no Go pointer to jitOwner or any object
that points back to jitOwner. The runtime indexes contain control records and
ranges only. Thus runtime code can continue to read mapped PC metadata while
retirement is pending without keeping the owner alive.

`jitOwner` is an ordinary pointerful, non-tiny GC object. Attach one
JIT-specific special to its base only after construction succeeds and before
publication. The special stores only the control-page record address; neither
it nor the queued record points back to the owner or its children. A failed
pre-publication load removes the private special and releases the mapping
synchronously.

The relevant source is `runtime/mcleanup.go`, `runtime/mheap.go`,
`runtime/mgcsweep.go`, and `runtime/mgcmark.go`. It shows that the stock
cleanup queue is shared, its target is not resurrected, and cleanup may run
late; these facts motivate a dedicated special and external control record.
The custom special still needs its own source-level proof that enqueue is
allocation-free, exactly once, and never dereferences the swept owner.

#### Serialized and runtime metadata inventory

| Information | Producer and location | Runtime representation and lifetime |
| --- | --- | --- |
| Package/file scope and export data | Compiler build metadata from exact build-selected inputs; package roots carried in a versioned Go object record | One executable contains code-free export records, import aliases, package identity, and target/toolchain fingerprints. Runtime parsing/type checking reads only this snapshot. |
| Eval package roots | Compiler call-site/file-scope record; consumed by cmd/link before dead-code elimination | The ordinary linker roots only packages visible to Eval and their resolved dependency closure. Blank imports request initialization only. |
| Host symbol manifest | cmd/link after archive resolution, ABI wrapper selection, and dead-code roots | Final link relocations point to the exact host functions, variables, types, methods, dictionaries, and required private helpers. Runtime resolves by full symbol identity and ABI/type hash; no process address is copied from a different build. |
| Runtime compiler output | In-process compiler object bytes and validated ModuleImage | Transient memory only. It contains new snippet code and relocation records, never imported package object archives. |
| JIT text and immutable runtime tables | In-memory linker and pcln/stack-map builder | One accounted external reservation with target, toolchain, module generation, section ranges, function/PC tables, stack maps, unwind data, source names, relocations, and synthetic scan descriptors. It stays mapped through full sweep completion and reader grace periods. |
| Mutable JIT package state | Compiler-generated typed state layout | Ordinary typed Go heap objects reached from jitOwner. No pointerful module data/BSS is scanned through a global module list. |
| Dynamic types and itabs | Compiler/runtime type builder | GC heap ABI cells with precise maps and owner/dependency fields. Name, text, and GC program payloads can live in the external mapping while the cell's owner is live. |
| Cleanup/control record | Runtime JIT loader before publication | A JIT-owned RW/NX control page: generation, mapping address/size, index token, embedded `specialJIT`, and queue state. It contains no pointer to jitOwner or its Go children; reader counts live in the global epoch protocol. Runtime queue links are separate from the control record. The whole reservation is unmapped last. |
| Type/reflection caches | Runtime and reflect dynamic-type paths | Owner-local construction/layout caches plus the weak-value canonicalizers specified below. Static host types retain existing cache paths. |
| Long-lived profile/trace locations | Runtime profiler, traceback, and trace producers | Detached CodeLoc (module ID, generation, text offset) and copied symbol/file/line data. Handles promising live code carry an owner; legacy raw-PC APIs pin the generation until process exit. |

Object-local goobj SymRef values and cmd/link LoaderSym IDs are transient
identities. They must never be serialized as runtime symbol IDs. The durable
manifest uses package fingerprint, object symbol name, ABI/version, kind,
type identity, and final-link relocations.

#### Owner references and escape cases

| Reference that can outlive Eval | Required GC-visible edge or conservative rule |
| --- | --- |
| JIT function value or callback | Every JIT funcval uses a typed closure environment containing jitOwner, even with no source captures. A top-level function used as a value is lowered to an owner-bearing wrapper. |
| Active JIT frame | The hidden owner argument is copied to a stack slot live at every safe point and represented in stack maps. Code.Run counters alone do not cover callbacks or spawned goroutines. |
| Captured caller local | The normal compiler marks every lexical local/parameter at an Eval call site as capturable and promotes it through typed heap storage before passing it. The frame records names/shadow identities, types, and addresses, preserving aliasing and assignment. |
| JIT-created local captured by an escaping closure | Escape analysis promotes it to a typed heap object allocated with an owner-associated scan descriptor. The GC shades the owner whenever it scans a live object of that allocation class. |
| JIT-defined type or reflect.Type | The interface type pointer points to a GC heap ABI type cell with an owner edge. Named Eval type identity is `(callSiteID, evalGeneration, declarationOrdinal)`; unnamed types use the canonical identity protocol below. |
| Interface and itab | A JIT itab is a GC heap cell with scanned owner/type edges; its method words are raw PCs. It is cached by owner, not in a process-global strong table. |
| Pointer to a JIT-created object or backing store | Allocate it through the owner-aware allocator. A runtime scan descriptor associates the exact user layout with the owner; the GC treats that association as an extra edge without changing the user's object bytes, size, or address. |
| String or slice backed by image constants | Copy into owner-aware heap backing storage before returning. Never return a data pointer into the external mapping without an owner edge. |
| Map or channel with JIT-defined types | Add an owner edge to the runtime map/channel object and ensure buckets/buffers use precise GC layouts. The container keeps its dynamic type metadata alive. |
| Host package function/global | Resolve through the linker-emitted manifest to the exact executable symbol or storage address. |
| JIT-to-JIT relocation | The source owner strongly retains the target owner. Relocated PCs alone are not GC dependencies. |
| Profile, trace, or panic symbol record | Store generation-relative CodeLoc and copied symbols/lines. Live CodePC/Func handles carry an owner. Legacy raw uintptr results pin the generation until process exit. |
| unsafe, uintptr, or foreign C reference | Reject from unloadable mode or pin until an explicit owner-aware deregistration proves the external reference is gone. |

Owner-local cycles are fine: a module owner may own type cells and state, and
those cells may point back to the owner. They become collectible when no
application root reaches the cycle. A process-global cache entry that points
strongly to any member is a real root and prevents cleanup.

#### Runtime type and cache design

Do not add unloadable JIT modules to ordinary activeModules root scanning.
JIT code has no pointerful module-global data/BSS roots. New package state
lives in owner-held typed heap objects. Host globals continue to resolve to
their original linked addresses.

JIT ABI type and itab records need GC heap cells whose ABI-visible record
begins at the address expected by the runtime and whose allocation scan map
also marks explicit owner/dependency fields. Confirm that heap interior-pointer
lookup marks the containing cell. Keep the ABI record layout, method offsets,
name offsets, equality function, and GC bitmap exact. A runtime equality
function that points into JIT text is itself an owner-bearing function value
or owner-relative function reference.

Use `(callSiteID, evalGeneration, declarationOrdinal)` for named Eval types.
Structurally identical unnamed types share Go identity through the weak-value
canonical type table specified below. Reuse the exact host descriptor when a
dynamic type is identical to a static host type.

#### Canonical Go type identity protocol

Type identity is a separate linker/runtime contract; `Type.Hash`, `Type.String`,
and a package/name pair are lookup hints, never proof of identity. The normal
link emits a static type catalog for every type descriptor reachable from the
Eval-visible package closure, package declarations, import signatures, and
capture types. The catalog is made after dead-code decisions and points to the
exact descriptors selected for the executable. Packages eligible for Eval
must retain their complete type descriptor closure, including types that only
flow through `any` or reflection. A missing catalog entry is a load/type-check
error; the JIT must not manufacture a duplicate descriptor for a host type.

The object linker resolves compiler-emitted references to these static type
identities to the catalog's final descriptor address, just as it resolves a
function or variable reference to the exact linked definition. This preserves
ordinary Go descriptor pointer identity for type assertions, interface
conversion, map keys, reflection equality, and method lookup. For dynamic
types, use one runtime canonicalizer shared by compiled JIT type records and
the fork's reflection constructors; extract and reuse the toolchain's full
type-equivalence rules rather than comparing printed names or hashes alone.

The canonical key is a versioned, collision-safe encoding of the Go identity
tree. It includes kind; array length; channel direction; function parameters,
results, and variadic status; map key and element; pointer/slice element; and
struct field order, field names, unexported-field package paths, tags,
embedded status, and field type identities. Interface method sets include
method names, unexported-method package paths, and signatures. Named types are
opaque identity atoms: static named types use the final linker type identity;
Eval declarations use `(callSiteID, evalGeneration, declarationOrdinal)`;
generic instances also include their canonical type-argument identities.
Unnamed components recurse through this representation. `Hash` selects a
bucket, followed by full structural equality; hash collisions cannot merge
types. The key includes a schema/toolchain fingerprint and target ABI so a
descriptor from another runtime layout is rejected.

The canonicalizer first resolves an equal static identity from the static
catalog. Otherwise it interns a GC-heap ABI type cell in a weak-value table
whose copied key contains no descriptor, name, or mapping pointer. The cell
has a traced owner/dependency edge and exact ABI layout/GC map. While the cell
is live, all equivalent unnamed types resolve to that cell; when no value,
code unit, or reflected Type owns it, the weak entry may disappear and a later
construction may create a new cell. This is sound because no live Go value
can observe the old identity after its last owning reference is gone. A
dynamic named type never aliases another Eval generation. A type that embeds
or refers to dynamic named types retains every contributing owner.

All reflection constructors must use the same decision path. Constructors
whose inputs are static descriptors may return/use static descriptors through
the existing process-lifetime caches. If any input descriptor is dynamic,
they use the owner-aware canonicalizer and owner-local layout/cache cells; no
dynamic result enters `ptrMap`, `lookupCache`, `funcLookupCache`,
`structLookupCache`, `layoutCache`, `reflectOffs`, or another append-only
global cache. Compiler-generated type-assertion and interface-switch caches
for JIT code are owner-local and typed. Host runtime paths detect dynamic
descriptors and route them to equivalent owner-aware caches rather than
placing their addresses in global uintptr caches. Static descriptors and
their current fast caches keep existing behavior.

This catalog intentionally includes type metadata, not duplicate package
object code. It is a prerequisite for general package interoperability, not
optional reflection enrichment. It also settles the anonymous-type edge
case: if a type can arrive from a host package through an interface, the
normal link must retain its descriptor even if source-level export data does
not name it; otherwise the build cannot claim that this type is compatible
with JIT-created unnamed types.

Route dynamic type operations away from process-global strong caches:
- keep itab and reflection construction caches on the owning module;
- make cross-module inline caches store typed references, so a cached target
  owner is a real dependency, or use generation IDs and resolve under a lease;
- keep dynamic type canonicalization in weak GC handles keyed by structural
  identity, while named Eval types use generation plus declaration ordinal;
- do not add dynamic descriptors to pinnedTypemaps, moduleToTypelinks,
  reflectOffs, or append-only reflection caches without a removable or weak
  path;
- preserve the existing fast global caches for static host types.

Audit runtime/iface.go, runtime/type.go, runtime/runtime1.go,
publicinternal/abi/switch.go, and reflect type/layout caches. Each path must
identify dynamic descriptors from their GC cell/owner, avoid making the owner
a global root, and reject stale generations. Stop-the-world alone does not
remove an escaped interface or reflect.Type value.

#### PC metadata, readers, and raw addresses

A JIT PC is needed for stack maps, traceback, panic/defer unwinding, async
preemption, profiling, and symbolization. The runtime needs a separate
generation-tagged JIT text index in addition to the static module chain.
Entries point only to external control records and mapped tables. They do not
point strongly to jitOwner.

Every lookup uses the two-epoch protocol in “Source-grounded design decisions.” The
immutable index snapshot is protected until a reader transfers its result to
an owner-bearing `funcInfo`; that strong owner edge protects later metadata
reads. Retirement publishes a snapshot without the range, flips epoch, waits
for the old reader count, and only then frees the snapshot/control record.
New lookups on the new snapshot continue to see other active JIT units.

While JIT code runs, its hidden owner stack slot keeps the owner live. A saved
function value keeps it through its closure environment. A goroutine waiting to
invoke a callback keeps the callback environment. Prove these edges for
synchronous/asynchronous preemption, stack growth, callbacks into host code,
nested JIT calls, panic, defer, and recover.

Add owner-bearing `CodePC`/`FuncHandle` and `Callers` variants for reclaimable
references. Legacy raw integer APIs (`Callers`, `Frame.PC`/`Entry`,
`Func.Entry`, `reflect.Value.Pointer`) permanently pin every JIT generation
they expose. Runtime profiling and traces use the owner-bearing path, then
convert to generation-relative `CodeLoc` plus copied symbols before long-term
storage. Panic values and traceback records follow the same rule. Known legacy
raw-PC APIs install their documented permanent pin automatically. An unknown
raw-address escape is rejected by Auto; foreign retention uses a rooted
`runtime/cgo.Handle` or an explicit pin released only after deregistration and
quiescence.

#### GC cleanup and external mapping lifecycle

The runtime API keeps Go's existing tri-color marking algorithm and adds a
JIT allocation edge source plus a narrow cleanup hook; it does not turn code
pages into heap objects:

1. A fully built module gets a private external mapping in RW/NX state. The
   linker validates symbols and relocations, writes sections, synchronizes
   instruction caches, changes text to RX and immutable data to R/NX, and
   keeps mutable pointerful state in typed Go heap objects.
2. The runtime registers complete PC/stack-map metadata and a control record
   atomically before exposing an entry point. The control record is independent
   of the owner allocation. Publication uses normal Go write barriers for owner
   references; the runtime index contains only non-GC control records.
3. A GC-visible jitOwner reaches every escaped value or dependency that needs
   code. Attach a JIT-specific sweep special to that owner, linked to a
   retirement record in the unit's RW/NX control page. The record has no edge
   back to the owner.
   Ordinary root tracing and cycle collection decide when cleanup is eligible.
4. When sweep finds the owner dead, it enqueues the retirement record without
   unmapping or running arbitrary teardown in sweep. The mapping remains
   readable/executable during queue delay. A worker publishes a new index
   snapshot without this generation, waits for old leases, unregisters runtime
   metadata and owner-scoped caches, then unmaps the control page with the unit.
   Other active JIT generations remain available. Cleanup order is arbitrary;
   a live source module's dependency edge keeps a target owner live.
5. A generation-scoped drain waits for cleanup discovery from a completed GC
   and for reaper work to finish. Account the full external reservation in
   runtime memory statistics and the memory-limit controller; the JIT budget
   also reserves bytes before mapping. Under pressure, collect and drain before
   mapping more; if all remaining owners are live or pinned, return a
   deterministic limit error.

Cleanup may be delayed and is not guaranteed to run before process exit. The
OS reclaims the process mapping at exit. A long-running process must have a
pressure path that forces collection and waits for JIT retirement. If a
runtime/platform configuration cannot provide cleanup and drain guarantees,
it cannot advertise automatic reclamation.

Code.Release drops the caller's owner edge and requests normal GC retirement.
General Eval must not promise synchronous Close: escaped closures, types,
callbacks, goroutines, foreign pointers, and raw PCs are not enumerable from a
Code handle. A verified scoped API can unmap synchronously because it returns
no owner-carrying value and proves no escape.

### Implementation research and evidence gates

The selected lifetime architecture uses an ordinary GC-visible owner and an
external executable mapping. Existing cleanup semantics discover dead owners
during sweep; a JIT-specific worker safely removes the mapping. Do not place
executable pages in Go heap spans and do not add a mark-termination
graph-tracing round.

The source review changed the earlier candidate because:
- Go heap spans participate in allocator zeroing, sweep, clobber, scavenging,
  and reuse. Executable heap pages would require changes to every such path.
- `runtime.AddCleanup` is eventual and uses a shared worker; it is not the
  executable-mapping reaper. The fork adds a dedicated sweep special and a
  bounded, allocation-free enqueue under a short runtime lock for a control
  record in the unit's RW/NX page.
- For an external mapping, dead-owner sweep can enqueue the record while the
  mapping remains valid until the dedicated reaper waits for the whole sweep
  generation, removes indexes, drains readers, and releases allocator metadata.
  The JIT needs that narrow runtime hook and explicit runtime memory accounting,
  not a custom mark-work algorithm.

Implementation gates:

1. **Owner values and lifetime.** Make the owner non-tiny and pointerful.
   Prove that closures, active frames, dynamic types/itabs, dynamic objects,
   containers, and cross-module links carry its GC edge. Unsafe, raw-PC, and
   foreign references pin or fail closed.
2. **Cleanup record separation.** Use the independent RW/NX control-page
   record with no Go pointer back to the owner. Audit the JIT special layout,
   sweep hook, enqueue ordering, and reaper inputs. Verify target sweep can
   finish before the reaper without any consumer dereferencing the target.
3. **Runtime index.** Store only generations, mapped ranges, and external
   control-record addresses in the JIT PC index. Never make the index a
   scanned root. Implement the selected two-epoch signal-safe reader and
   owner-bearing metadata handoff.
4. **Type/cache partition.** Trace every static module-chain consumer, itab
   insertion, typelink/reflection cache, and inline type cache. Dynamic entries
   must be owner-local, weak, or generation-aware and removable.
5. **Linker manifest.** Root every Eval-visible package before dead-code
   elimination and emit final selected symbol relocations. Cover functions,
   data, wrappers, dictionaries, types, and private helper dependencies.
6. **JIT resource pressure.** Count external code and metadata bytes. Add a
   runtime/JIT operation that forces GC, waits for cleanup discovery and
   reaper completion, and returns only after eligible mappings are reclaimed.
   If owners remain reachable or pinned, honor the configured limit with an
   error.
7. **Evidence.** Execute the verification gates in the revised design chapter
   on Linux/amd64 before declaring support; preserve the regex fixture as a
   constrained client, not proof of general Eval safety.

The prototype currently registers runtime module metadata and manually unmaps
through Code.Close. It is not safe for arbitrary escaping values. Its current
Linux/amd64 mmap is a useful backend seam, not evidence of owner propagation,
type/cache handling, or GC-driven retirement.

## Current limits

The implementation described above is a prototype. In this worktree the
export-only importer and metadata changes are drafts; they have not been
verified in this turn. The runtime registration path currently carries
function/PC/stack metadata, while complete type, itab, static-data, and
runtime-cache integration remains unfinished. The current generated host
references keep selected symbols alive but do not yet provide the package-wide
linker roots or complete symbol manifest required for arbitrary imported
generic bodies. `CompileSnippet` currently passes only `-l=1`; it does not
apply the recorded language version or architecture features, and has no
explicit propagation of race/MSan/ASan/coverage settings.

- The custom object linker and trampoline currently target Linux/amd64.
- The Linux/amd64 linker supports absolute 64-bit addresses, PC-relative
  calls/data, 32-bit section offsets and method offsets, GOTPCREL references
  backed by read-only image slots, weak address/addroff references, zero-width
  retention markers, and indirect-call markers. Unsupported relocation types,
  widths, symbol kinds, and malformed object metadata fail with diagnostics.
- Runtime imports currently resolve functions through runtime function tables,
  builtin and reflect-constructible composite type descriptors, and the
  specific runtime data symbols `staticuint64s` and `writeBarrier`. Other
  runtime data references fail explicitly until the runtime link manifest is
  broadened.
- A snippet uses the caller's package identity and file import scope and sees
  saved package declarations. Existing package variables, functions, types,
  and methods are resolved to the host process's symbols; generic package
  function bodies are supplied to the compiler for instantiation. New package
  declarations and package initialization are not supported. Lexical locals
  are not yet in scope; local-variable capture is not implemented.
- The build metadata records package declarations, imports, and linker-visible
  package symbols. It does not describe live variables at an `Eval` call site.
  A complete capture ABI must include package variables and all lexically
  visible runtime variables: receivers, parameters, named results, locals,
  short-declared variables, loop/range variables, type-switch and select
  variables, and variables captured by enclosing closures. Constants and
  labels are not runtime storage; imports and types are name/type scope data.
  Capturing these values requires compiler-generated call-site metadata and
  stable typed storage, including GC-visible pointer maps and Go's aliasing
  and shadowing rules. Runtime stack inspection cannot recover optimized-away
  or register-only locals.
- The runtime receives each generated function's PC-to-file and PC-to-line
  tables and the corresponding compilation-unit file table. JIT-created type
  descriptors and itabs are not yet registered with the runtime, so reflection
  and some interface conversions involving new types remain unsupported.
- Runtime type and itab caches can retain pointers into a loaded module. Modules
  containing local type or itab symbols are currently pinned: `Code.Close`
  returns an error and leaves them mapped until a runtime cache invalidation
  protocol exists.
- No GC-owned `moduleOwner` or implicit closure capture is implemented. The
  current `Code.Close` directly unregisters the module and unmaps it after its
  own synchronous `Run` calls finish. Safe use still depends on the caller
  proving that no callback, goroutine, runtime metadata handle, or pointer can
  reach the mapping.
- This is not yet a general Go module loader. New package-level declarations,
  package initialization, arbitrary type descriptors, and every possible
  compiler relocation are not supported.
- Unloading is explicit. `Code.Close` tracks synchronous calls through that
  handle; it cannot prove that evaluated source did not start a goroutine or
  retain a callback. Keep the handle until such work has stopped.

## Verification

From the Go source tree, run:

```sh
GOROOT="$PWD" GOPATH="$PWD" GO111MODULE=off go test ./src/jit
GOROOT="$PWD" GOPATH="$PWD" GO111MODULE=off go test ./misc/jit-compile
GOROOT="$PWD" GOPATH="$PWD" GO111MODULE=off GOGC=1 go run ./misc/jit-compile
```

The test checks that a generated function appears in `runtime.FuncForPC`
while loaded and disappears after `Code.Close`. The example exercises imported
function calls, shared package storage, forced GC and stack growth while a JIT
frame is live, and explicit unload.

Source tests for compiler-generated linker coverage are present in
`src/jit/linker_coverage_test.go`. I did not run them in this planning pass,
so the table describes intended cases in the source, not a result verified
for the current worktree:

| Area | Cases exercised | Coverage |
| --- | --- | --- |
| Local code and data | Direct helper calls, package globals, strings, slices, closures, generics | Compiler relocations and local symbol closure |
| Runtime operations | Map lookup, mutation, delete, clear, iteration and pointer writes; interface assertions, type switches, method dispatch and method values; channels, select, goroutines, defer, panic, string concatenation and byte conversion | Runtime imports, function metadata, stack/GC maps, closure code, and write barriers |
| Local code and closures | Capturing and recursive closures, function values, method values, generic instantiation, named results and deferred calls | Reachable local code and closure environment references |
| Relocation handling | Absolute addresses, PC-relative data, direct calls, addroffsets, method offsets, weak references, indirect-call markers and GOT slots | Compiler object to image relocation mapping; an explicit supported/rejected relocation table guards the Linux/amd64 contract |
| GOT | Compiler object relocation fixture plus mapped GOT pointer slot | GOTPCREL displacement and pointer initialization |
| Host package reuse | `fmt` and shared `jitcounter` package state | `misc/jit-compile` integration test |
| Runtime type identity | Builtin/composite runtime types and local interface types | Runtime descriptor lookup; modules with local types are pinned |
| Close lifecycle | Active-run guard, rejection of runs after close, idempotent close | Prevents unmapping during a tracked call; escaped goroutines and callbacks remain the caller's responsibility |

This matrix is not a claim that every relocation emitted by every Go
architecture, build mode, or assembly source is supported. The linker rejects
forms outside its declared Linux/amd64 Go-object subset.

## Next implementation gates

1. Audit and finish invocation-local compiler/link state; document what state
   remains shared and why it is immutable or synchronized.
2. Complete the code-free export snapshot and in-memory importer using the
   exact build-selected package exports. Runtime must not open archives or
   consult Go tools, `GOROOT`, or package files.
3. Implement the normal linker's Eval-scoped root record and relocation-backed
   host-symbol manifest. Verify package identity, private helper retention,
   direct imported functions/variables/types, and exact shared global storage.
4. Make Eval's caller package/import scope explicit at the compiler call site;
   parse and compile the source string at runtime. Lower same-package existing
   functions and variables as references to host symbols rather than duplicate
   definitions. Support package-scope behavior before adding lexical captures.
5. Complete the module linker and runtime registration contract for relocations,
   stack/heap-object maps, stack growth, panic/traceback, types, itabs, and
   interfaces. Keep pointerful globals in owner-held heap state. Registration
   must be atomic and survive forced GC during JIT calls; the cleanup/reaper
   protocol must drain readers before unmapping.
6. Add the typed capture ABI and GC-visible ownership edges. `Release` drops a
   root; it does not synchronously free code. Define explicit close behavior
   separately, and keep unsupported escapes or cache-retained types pinned
   rather than unloading them optimistically.
7. Run the full Linux/amd64 conformance suite, including rejection cases and
   concurrent requests. Add Linux/arm64 only after amd64 passes; add each OS
   and architecture backend only with its own execution evidence.

## Revised design: GC-owned lifetimes for external JIT mappings

### Decision and safety boundary

A Go GC object owns the right to use a JIT module. The executable mapping is
a separate OS resource. This keeps executable pages out of the Go heap while
letting ordinary Go reachability decide when their code is no longer needed.

Every supported Go reference that can enter JIT text, use its type metadata,
or access its data carries a strong edge to the module owner. The runtime also
tracks active PCs and readers. When the owner becomes unreachable, sweep
schedules a JIT cleanup record. The mapping stays mapped until the reaper
removes the JIT index and drains every reader.

The cleanup mechanism is for an external resource. It must never protect code
bytes that are themselves Go heap objects. Its argument is independent of the
target and contains no reference back to it. The cleanup path only queues the
resource record; a dedicated worker handles synchronization and unmapping.

No Go runtime mechanism can discover an integer PC or pointer copied into
arbitrary unsafe memory or foreign C memory. Such references must use an
owner-aware handle, pin the module until explicitly deregistered, or be
rejected by the unloadable profile. Production behavior must state this
boundary. Automatic unloading cannot be promised for an untracked uintptr.

### Owner graph

Each successful module has a non-zero, pointerful Go heap owner. It is not
stored as a strong reference in a process-global module index. Strong edges
come from:

- the public Code handle while the application retains it;
- every generated closure/function value, including method values and
  top-level functions converted to values;
- a hidden owner slot in every active JIT stack frame;
- JIT-created type and itab cells, and values whose layout or method code
  depends on a JIT type;
- JIT-created heap objects and slice/string backing arrays whose allocation
  descriptors carry the GC-traced owner edge, plus maps and channels whose
  runtime objects carry an explicit scanned owner field;
- owner-scoped package state and closures that capture that state;
- owner-bearing CodePC/FuncHandle values that promise live code;
- source-to-target dependency edges for direct cross-module relocations.

Host package functions and globals already have process lifetime. The linker
manifest binds them to their exact selected addresses. A JIT reference to a
host global neither copies its value nor creates a second package instance.

The owner may own GC heap state and type cells that point back to it. Those
cycles are intentional and collectible when no ordinary root reaches them.
A global cache entry that points strongly to any member is an ordinary root
and prevents cleanup; cache partitioning is part of lifetime correctness.

The reclamation unit is an independently linked and registered `LinkUnit`.
For the first supported general-Eval contract, one Eval image is one unit:
the linker removes unreachable functions before publication, but once any
function, type, or data value from that image escapes, the complete unit stays
alive. This is deliberate coarse retention, not an untracked leak: it has a
single owner, a single accounting charge, and a documented lifetime. Do not
try to unmap individual functions from a published image.

Keep owner IDs, relocations, PC-index entries, type cells, and cleanup records
unit-scoped from the start. That permits a later finer-grained linker to emit
multiple units per Eval, with separate page-aligned mappings and runtime
metadata. Such splitting must model direct-call dependencies, function values,
methods/itabs, type methods and equality/hash code, dictionaries, constants,
and all PC readers. A live unit strongly retains each unit it calls or whose
metadata it uses. A dynamic type surviving independently retains its type
metadata and any code units its methods require. Fine-grained units reduce
retained bytes; they are not required for correctness if module-level budgets
and deterministic limit errors are enforced. Do not describe whole-image
retention as per-function unloading.

### Function values, frames, callbacks, and captures

runtime.funcval stores a code address in its first word, which is not a GC
edge. The compiler must produce an owner-bearing typed closure environment
for every function value from JIT code, even if the source function has no
captures. A top-level function value and a method expression need a wrapper
environment; a method value must retain receiver semantics and owner.
Interface dispatch entries are raw PCs, so the containing itab cell also
carries the owner edge.

The internal JIT call ABI passes an owner to every JIT entry. The prologue
stores it in a stack slot that remains live at every safe point. A host-call
trampoline supplies it from the closure environment or Code handle. Direct
same-module calls propagate it; cross-module calls retain a dependency owner.
Stack maps must prove the slot remains visible during nested host calls,
stack growth, asynchronous preemption, panic unwinding, defers, recovery, and
callbacks.

Runtime source strings are unknown when the host package is built. At each
Eval call site, the normal compiler therefore marks every lexically visible
local and parameter as capturable and promotes its storage through typed heap
cells before passing it. The call-site descriptor supplies names, shadowing
identity, type identity, and addresses. This preserves reads, writes,
aliasing, loop iteration semantics, closure captures, receivers, and
parameters even after the caller frame returns. Package globals resolve
through the host manifest. Dynamic code cannot retain a pointer into a caller
stack frame.

JIT code may store a callback in a host global, pass it to an imported
package, start a goroutine, send it through a channel, or retain it in a
closure. Every such function value uses an owner-bearing environment, and an
active invocation has a frame owner. If a callback or goroutine can retain a
raw entry address rather than a Go function value, it must use the explicit
pin/foreign-registration path.

### Dynamic types, objects, containers, and data

Go interfaces point at runtime type or itab records. A JIT-defined descriptor
cannot be only an address in an untracked mapping if an interface may outlive
Eval. Create a GC heap cell whose ABI-visible record begins at the address
expected by the runtime and whose precise allocation map traces an explicit
module owner. Confirm that interior-pointer object lookup marks the containing
cell. Keep ABI record layout, method offsets, name offsets, equality
functions, and GC bitmaps exact. A runtime equality function that points into
JIT text is itself an owner-bearing function value or owner-relative
reference.

Use `(callSiteID, evalGeneration, declarationOrdinal)` for named Eval types.
Structurally identical unnamed types share Go identity through the weak-value
canonical type table specified below. Reuse the exact host descriptor when a
dynamic type is identical to a static host type.

All JIT-associated heap allocations use the allocation-side owner edge and
JIT scan descriptor described in “Source-grounded design decisions.” The
user allocation has its exact language-visible size and layout. Slice/string
backing storage, maps, channels, dynamic descriptors, finalizers, and
zero-sized escaping values follow those concrete rules. Every allocation
path must use the owner-aware allocator or fail closed.

Mutable package state created by Eval is an ordinary typed owner-held object.
The runtime does not scan a global JIT data/BSS range, which would keep an
owner reachable from its own registry. Existing host globals remain direct
manifest references. Pointer-free code, constant tables, names, file/line
records, and stack maps may live in the external mapping as long as every Go
string, function value, and type handle handed to user code is owner-bearing
or copied.

### Runtime caches and reflection

Ordinary runtime type and itab caches assume process-lifetime modules. Keep
static host behavior unchanged and apply the exact disposition table in
“Source-grounded design decisions” to every dynamic type or itab.

- Dynamic itabs use typed cells and weak canonicalization; never put a strong
  JIT itab in the process-wide `itabTable`.
- Dynamic inputs bypass the ABI's unscanned uintptr assertion/switch caches;
  JIT sites use owner-held typed caches.
- Reflection constructor/layout caches use owner-held typed entries for
  dynamic types. Static types remain on the existing process-lifetime paths.
- resolveNameOff, resolveTypeOff, textOff, and reflect offset handling derive
  the owner from the dynamic type cell and validate generation/range.
- reflect.Type, reflect.Value, method values, and runtime.Func values that
  escape must retain their dynamic type or detached symbol data.

Audit runtime/iface.go, runtime/type.go, runtime/runtime1.go,
publicinternal/abi/switch.go, and reflect type/layout caches against the exact
table. Stop-the-world does not remove an escaped interface or reflect.Type.

### PCs, profiling, and foreign references

JIT PC metadata is needed for stack maps, traceback, panic/defer unwinding,
asynchronous preemption, profiling, and symbolization. The runtime needs a
separate generation-tagged JIT text index in addition to the static module
chain. Entries point only to external control records and mapped tables; they
never point strongly to jitOwner.

Every lookup uses the selected two-epoch protocol: readers announce before
loading an immutable snapshot and transfer the result to an owner-bearing
`funcInfo`. The owner protects later table reads; retirement removes the range
from the next snapshot and waits for old readers before freeing control data.
Other live JIT modules remain visible during that grace period.

While JIT code runs, its hidden owner stack slot keeps the owner live. A saved
function value keeps it through the closure environment. A goroutine waiting
to invoke a callback keeps the callback environment. Prove these edges for
synchronous/asynchronous preemption, stack growth, callbacks into host code,
nested JIT calls, panic, defer, and recover.

Add owner-bearing CodePC/FuncHandle values for reclaimable references.
Legacy APIs returning raw uintptrs pin the exposed generation until process
exit. Internal profiling, trace, panic, and stack records use detached
module-ID/generation/text-offset values plus copied symbol/file/line data.
Known raw-PC APIs install their defined permanent pin automatically; Auto
rejects unknown integer/address escapes. Foreign retention requires a rooted
handle or a Manual pin and deregistration before release.

### Manual pinning, raw PCs, and unsafe references

A raw PC includes a code address returned as `uintptr` or copied to an integer
such as `int64`. Integers are not GC pointers. Converting such an address to
`unsafe.Pointer` does not make it a pointer to a Go heap owner: the executable
mapping is outside the Go heap, so that pointer cannot keep its JIT owner
alive. A Go function value is different only if the compiler constructs its
closure environment with an owner edge; the function value's code word alone
is not that edge. A pointer to an ordinary Go heap object that itself carries
the owner is GC-visible, but does not make arbitrary copied PCs safe.

The public contract has three lifetime policies:

- `Auto` is the default. Ordinary owner-bearing Go values keep code, types,
  and data alive; when their last owner edge disappears, the runtime cleanup
  path eventually retires the unit.
- `Manual` returns a `Pin` for code whose address or foreign registration is
  retained outside owner-aware Go values. An active pin suppresses automatic
  retirement until explicit `Release`. A private Go root table maps pin IDs
  to owners; losing the `Pin` value without calling `Release` intentionally
  leaves that root in place and leaks the pin rather than risking use-after-
  unmap. Expose active-pin counts and diagnostic IDs so this failure is
  visible. `Release` removes that pin root only after the caller has stopped
  all foreign execution and removed every copied raw address. Other ordinary
  Go owner edges can still keep the unit alive afterward.
- `Scoped` is the verified synchronous no-escape policy described above. It
  does not return a `Pin` or any owner-bearing value and may retire the unit
  after the call only when the effect certificate proves that safe.

The API should make the unsafe action explicit, for example `Code.Pin()`
returning an opaque `*Pin`, `Pin.PC(fn)` returning a documented raw address,
and `Pin.Release()` being idempotent. Prefer `Pin.Func`/`CodePC` for an
owner-bearing callable handle. `Pin.PC` is valid only while that pin remains
active. A foreign registration helper should pair registration with a pin and
require deregistration before release. Releasing a pin does not claim that
arbitrary C code, an integer, or an unsafe memory location has stopped using
the address; that is the caller's explicit precondition. If the caller cannot
prove quiescence, it must keep the pin. Release drops the manual root; it does
not promise immediate unmapping if ordinary owner-bearing Go values still
exist or collection/reaping has not completed. The retirement hook is
installed before publication; if that setup fails, compilation fails before
any address is exposed.

“Manual” means no GC-driven code retirement while the pin is active; it does
not mean that general Go code may skip GC participation. Every published unit
gets its sweep special before publication; a pin root keeps it from retiring
while foreign code may still use it. JIT frames need correct stack maps, and
Go pointers, captures, dynamic types, and owner-bearing values still need
ordinary GC edges. The owner-root table is a strong GC root by design. General
Eval cannot omit owner tracking: if it returns a closure, type, object, or
callback, those values must keep their code and metadata valid. A mode that
truly avoids Go GC metadata is only valid for a separate leaf-code ABI that
cannot call Go, allocate, retain pointers, expose types, or participate in Go
stack walking; it is not Go `Eval`.

### Cleanup guarantees and the runtime hook

The checked-in `runtime.AddCleanup` contract is intentionally eventual and
does not provide the JIT queue/drain guarantee. The design therefore does not
use its shared cleanup worker for executable mappings. A dedicated JIT sweep
special is attached to the pointerful GC owner and points to an independent
record in the unit's RW/NX control page. Arenas and runtime modes without this
special fail closed for Auto.

Source inspection of `runtime/mheap.go` and `runtime/mgcsweep.go` confirms
that `freeSpecial` enqueues a cleanup when sweep discovers the dead target;
it does not resurrect the target. `runtime.GC` in `runtime/mgc.go` waits for a
full cycle and helps finish sweeping before returning. The existing cleanup
worker is a shared queue, however, and its drain helper is internal/test
support, not a per-JIT production API. Waiting for that shared queue would
couple JIT memory pressure to unrelated cleanup callbacks, including callbacks
that can block indefinitely.

The selected lifecycle is in “Source-grounded design decisions”: sweep performs
one bounded, allocation-free enqueue to a dedicated queue; a separate reaper
waits for full sweep completion, removes the index entry, waits for the epoch
grace period, unregisters metadata, and unmaps. `Reclaim` waits only for JIT
retirement. The OS reclaims mappings left at process exit; the runtime does not
promise cleanup before exit.

This is a justified custom runtime hook, but not a custom collector marking
algorithm. The GC still decides liveness using ordinary owner edges. The new
special only hands an already-dead owner's external resource record to a
separate reaper. Preserve the independent-record rule so target sweep and
resource teardown can race safely.

### External mapping and cleanup state machine

| State | Visibility | Mapping | Owner/resource rule |
| --- | --- | --- | --- |
| building | Private | RW/NX | Owner is on the compiler/runtime stack; no public entry exists |
| active | JIT range index published | Text RX, immutable data R/NX | Go values hold owner edges; runtime record holds external control data |
| manual-pinned | JIT range index published | Text RX, immutable data R/NX | A typed pin-root entry retains the owner until explicit release |
| cleanup-queued | Still mapped for existing leases | Protected | GC found the owner unreachable; the control-page record is queued |
| sweep-drained | Still mapped | Protected | The full sweep generation is complete; scan descriptors are no longer in use by GC |
| retiring | Retiring range omitted from the next index snapshot; other ranges remain available | Still mapped until old readers drain | Worker removes indexes and owner-scoped cache entries |
| unmapped | Absent from all indexes | Released | Worker unmaps code, metadata, and control page; then decrements mapped-byte counters |

Build and relocation happen in a private RW/NX mapping. Validate symbols,
relocations, ranges, type identities, and metadata before publication. Publish
runtime metadata and code atomically; no entry is callable until registration
succeeds. A failed build rolls back synchronously.

The owner cleanup target is a normal non-tiny pointerful heap object. Attach a
JIT-specific sweep special whose payload is a record in the unit's separate
RW/NX control page. It contains no pointer to the owner, owner children, or Go
heap type cells. The sweep path only queues the record. A dedicated reaper
publishes an index snapshot without this generation, waits for old readers,
removes owner-scoped cache entries, unregisters runtime metadata, then unmaps.
Other JIT generations remain available throughout. The owner may already have
been reclaimed; no retirement code may dereference it.

Use the dedicated sweep special, not the shared AddCleanup queue, for the
external resource. Track mapped bytes through successful unmap. Cleanup is
collection-driven and eventual, not an immediate destructor; `Reclaim` is the
explicit barrier for eligible units.

Code.Release drops the caller's owner edge and requests normal GC retirement.
General Eval cannot promise synchronous Close because escaped closures,
types, callbacks, goroutines, foreign pointers, and raw PCs are not
enumerable from a Code handle. A verified scoped API can unmap synchronously
because it returns no owner-carrying value and proves no escape.

### Proven no-escape fast path

Static escape/effect analysis may select a scoped path only when it proves a
strict certificate from the runtime-compiled object and complete call graph.
This is an optimization; unknown effects always select general Eval.

The certificate requires:
- no generated function, callback, dynamic type, pointer, string, slice, map,
  channel, or PC escapes the synchronous call;
- every imported call has a trusted summary proving it does not retain passed
  pointers/callbacks or return aliases to JIT storage;
- no goroutine start, C/foreign call, unsafe operation, reflection of code
  addresses, raw-PC API, or unknown indirect/interface call;
- defer runs before return; if a panic crosses the scoped boundary, the
  wrapper conservatively pins the owner before re-panicking, because panic
  values or saved traceback state may retain JIT data;
- image-backed Go data is copied or owner-backed, and profile/trace paths use
  detached CodeLoc records;
- object verification confirms there are no unlisted relocations or dynamic
  type/itab references.

The scoped wrapper roots the owner on its stack, registers stack maps and PC
metadata, and calls the entry. On normal return it closes lookup, drains
readers, unregisters, and unmaps. A blocking call keeps the mapping live until
it returns. If a panic crosses the boundary, the wrapper pins the owner before
re-panicking unless a stronger proof shows neither the panic value nor saved
traceback state refers to JIT data. If any certificate check fails, use the
general owner path.

This path cannot skip stack metadata while code executes. The collector must
scan JIT frames, and profiling/unwind readers still need a valid mapping. A
persistent regex object that owns one matcher is not scoped: the regex object
keeps the regular owner alive until its final reference is collected. The
dynamic-RegExp stress run exercises parsing/linking only; it does not prove
either lifetime protocol.

### API and semantic contract

1. Eval source is a statement list parsed at runtime as a function body in the
   caller's package and exact file import scope. It has no package clause or
   import declaration. The call-site descriptor supplies package declarations,
   import aliases/dot imports, type parameters, and typed capture slots.
2. Every lexical variable visible at that call site is capturable because
   source is unknown during the ordinary build. The caller compiler promotes
   those storage locations through heap cells before exposing the frame.
   Captures preserve identity, writes, shadowing, loop iteration semantics,
   receiver/parameter values, and closure aliasing.
3. Package globals and imported functions/variables resolve to the existing
   linker-selected symbols and exact addresses. No duplicate package code or
   copied global state is created. Each Eval image gets a 128-bit process
   nonce plus a checked monotonic sequence; declaration identities append a
   per-image ordinal. Symbol names include that identity, and sequence
   exhaustion fails instead of reusing an ID. Timestamps are not identities.
4. Every result reaching ordinary Go code is owner-carrying, copied into
   host-owned data, explicitly pinned, or rejected before publication. This
   applies through interfaces, containers, reflection, callbacks, goroutines,
   and method values.
5. Eval runs trusted code with host-process privileges. Go code can block
   forever, call exit, mutate globals, or use unsafe/C. Compile limits do not
   sandbox execution. Untrusted evaluation requires an OS/process sandbox and
   is outside this in-process lifetime design.

### Historical design-gap checklist (resolved by the decisions below)

The checklist below recorded questions that were open when this section was
written. The selected policies in “Source-grounded design decisions” supersede those
questions. Implementation still has to satisfy the source-level invariants;
the checklist is retained as traceability for the work that was closed.

1. **Choose and specify the PC-reader lifetime protocol.** The current text
   says “immutable index snapshot plus an epoch or signal-safe counter” but
   does not choose one, define its memory ordering, or say exactly where a
   lease begins and ends. This matters in this checkout: `findmoduledatap` and
   `findfunc` traverse/read module metadata without a lock
   (`runtime/symtab.go`), and `sigprof` is explicitly allowed to run during
   stop-the-world and calls `findfunc` (`runtime/proc.go`). A stop-the-world
   update by itself is therefore not a sufficient lifetime argument.

   Close this by choosing one concrete signal-safe protocol, specifying its
   admission/retirement linearization points, nested-reader behavior, memory
   ordering, counter overflow behavior, and what a reader does while a module
   is retiring. Then make a source inventory of every consumer of `funcInfo`,
   `_func`, `moduledata`, PC tables, type offsets, and JIT text. The lease must
   cover the last dereference, not merely the initial `findfunc` call. Include
   signal-time profiling, async preemption, traceback, GC stack scans,
   `runtime.FuncForPC`, reflection, panic/defer, and saved profile/trace data.
   Long-lived records must become detached `CodeLoc` values while protected;
   they cannot retain bare PCs after the lease ends.

2. **Set one lifetime rule for every raw-PC API.** The plan says legacy raw
   uintptr results “pin” but does not state for how long, and an integer copy
   has no observable last use. The production contract must choose explicitly:
   owner-bearing `CodePC` for unloadable references, explicit `Pin` for raw
   addresses, and either a permanent conservative pin or a hard-fork API
   change for every legacy API that exposes a JIT PC. There is no safe
   automatic “unpin when the last integer is gone” mechanism. Inventory
   `runtime.Callers`, `runtime.FuncForPC`/`Func.Entry`, `reflect.Value.Pointer`,
   traceback/profile APIs, and linknamed PC helpers, and make each outcome
   explicit. The manual pin's final-release precondition and behavior on
   failed release must be uniform.

3. **Fix the GC-visible representation of each escaping allocation.** The
   current design allows a hidden wrapper/header “before or after” the user
   value, but does not choose one representation or show it works for every
   allocation path. Specify the GC object shape and pointer map for ordinary
   `new(T)`, closure environments, slice/string backing storage, map and
   channel objects, dynamic type/itab cells, and zero-sized values. Preserve
   the user's pointer identity and layout, finalizer behavior, reflection,
   checkptr, and typed write barriers. Include `runtime.SetFinalizer`,
   `runtime.AddCleanup`, `runtime.Pinner`, and arenas: arena storage is not
   traced or cleanup-eligible, so it must have an owner pin through arena
   release or be rejected from Auto mode. For host functions reachable from
   Eval, define transitive effect summaries; an unknown callee that may retain
   a raw address, allocate arena storage, or hand a reference to C must select
   Manual or fail closed.

4. **Replace cache categories with a complete cache disposition table.**
   “Owner-local, weak, or generation-aware” is still a set of alternatives.
   Enumerate every runtime and reflect cache that can retain or dereference a
   JIT type/code address, including `itabTable`, `pinnedTypemaps`,
   `moduleToTypelinks`, `reflectOffs`, method/type assertion caches, and inline
   caches. For each one, choose exactly one policy, give its ownership edge,
   and state how it is removed or invalidated. Define named versus unnamed
   dynamic type identity and `reflect.TypeByName` behavior when no live value
   owns a dynamic type. Global weak canonicalization must not become an owner
   root; owner-local strong tables must die with the owner.

5. **Complete the lifecycle state machine with concurrency and failure rules.**
   Add linearization points for publication, pin acquisition, final pin
   release, owner-death enqueue, range unpublication, metadata unregister,
   and unmap. Specify how multiple simultaneous pins are counted, how release
   races with GC, how duplicate cleanup is rejected, and how a concurrent
   compile interacts with a byte-budget reclaim. The sweep enqueue cannot
   allocate; preallocate its system-memory record and define the queue's
   exactly-once and overflow behavior. Define what happens if runtime
   unregistration or OS unmap fails: retain a tombstone/control record and
   report the failure; never free metadata or reuse the address while a reader
   may still hold it. `Reclaim` also needs reentrancy and timeout semantics; it
   cannot run from a signal handler, GC callback, or its own reaper.

6. **Publish the exact supported semantic/build matrix.** Linux/amd64 is the
   first target, but the plan must say which runtime modes and Go facilities
   are supported, pinned, or rejected: cgo, unsafe, arenas, finalizers,
   profiling, async preemption, race instrumentation, checkptr, coverage,
   plugins, and runtime/linkname APIs. The ordinary binary's metadata schema,
   compiler/runtime ABI fingerprint, and linker-root policy must be versioned
   together. A mode without an owner, PC-reader, or cleanup guarantee must
   fail closed rather than silently weaken unloading.

7. **Make the safety argument traceable without treating tests as proof.**
   Add a matrix mapping every path that can name code/type/data to its owner
   edge or manual pin; every runtime reader to its lease; every cache to its
   disposition; and every resource to its retirement action. State the
   invariants: a live executable reference implies a live owner or pin; no
   published reader can observe freed metadata; only one retirement wins;
   unmap follows unpublication and reader quiescence; and every retained byte
   is charged to a live owner, pin, queue record, or explicit tombstone.
   Source review and invariants form the safety argument. Tests remain useful
   for regression and counterexamples, but passing tests does not establish
   these invariants for unenumerated paths.

### Source-grounded design decisions

This section selects a coherent policy for the first target, Linux/amd64. The
source-audit resolutions below are authoritative where they refine earlier
exploratory text. These are architecture decisions, not evidence that the
implementation is already safe. The source-to-invariant review remains
necessary after each runtime/compiler change; tests can find counterexamples
but cannot prove an unenumerated path safe.

#### 1. PC lookup: immutable snapshots with two-epoch readers

Do not add JIT modules to `firstmoduledata.next`, `lastmoduledatap`, or the
static `activeModules` slice. Keep the static module chain unchanged and add
separate sorted JIT PC and metadata-address indexes. One immutable epoch
snapshot publishes both indexes and points only to non-GC control records.
Every range has a 64-bit generation and cannot be reused until that generation
has retired.

Each published LinkUnit carries a private synthetic `moduledata` view in its
immutable mapping, populated only with the fields consumed by supported
function lookup, traceback, PC-value, stack-map, and unwind paths. The JIT
index points to this view; it is never linked through `moduledata.next`,
`lastmoduledatap`, or `activeModules`. Pointerful globals, init tasks, and
typelinks remain absent. Before publication, the implementation builds a
field-use matrix for every runtime consumer and rejects a unit if any required
field or table is missing. Any consumer that requires global module-chain
membership is changed to use the JIT index or is rejected for Linux/amd64.

Use a two-epoch reader protocol, not a mutex, STW-only update, per-M lock, or
unspecified hazard-pointer scheme. The reader is allocation-free and
`nosplit`, using only lock-free atomics:

1. Read the current epoch with sequentially consistent ordering.
2. Increment that epoch's reader count with a checked CAS. Saturation is a
   runtime fatal error; counters never wrap.
3. Re-read the epoch. If it changed, decrement and retry before loading the
   root. This closes the race where a reader is interrupted between seeing an
   epoch and announcing itself.
4. Load the immutable root, find the PC or metadata range, and copy the owner
   into an owner-bearing `funcInfo`/`NameRef`/`TypeRef` before decrementing the
   reader count. Do not return a control-record or module-metadata pointer
   without the GC-visible owner.
5. A miss returns the ordinary static result or invalid result. A lookup on an
   old snapshot finishes normally; its epoch announcement keeps that snapshot
   and its records alive.

One serialized publisher builds a replacement snapshot, publishes the root,
flips the epoch, and waits for the old epoch count to reach zero before
freeing the old snapshot or any control record reachable only from it. It
cannot flip again until that grace period completes. New readers continue on
the new snapshot while old readers finish. Removing a range precedes freeing
its metadata or executable pages.

Every JIT `funcInfo` carries a strong `*jitOwner`; that edge keeps the mapping
and metadata alive after the short index read. `runtime.FuncForPC` returns a
GC heap wrapper with an owner and metadata handle, never a pointer cast into
the mapping. Runtime callers may not extract `_func*`, `moduledata*`, mapped
table slices, or strings and let them outlive the `funcInfo`; they must copy
data or return an owner-bearing object. Unlisted `linkname` access to
`findfunc` or its result layout is unsupported in this hard fork.

An unscanned index record is not itself a GC root. Therefore each legal JIT-PC
lookup must already have an independent liveness witness: the owner-bearing
`CodePC`/function/type handle being inspected, a hidden owner slot in the
active JIT stack, or a pin installed before a raw PC is exposed. `Callers` and
other legacy raw-PC producers install permanent generation pins before
returning their integers. `FuncForPC` on a JIT address is supported only for
such a previously exposed-and-pinned address; arbitrary integer fabrication
is outside Auto's contract. Copying the owner from the control record is a
handoff under that witness, not an attempt to resurrect an owner that GC has
already found dead. The reader keeps the owner strongly reachable after the
handoff; `KeepAlive` marks the end of that use. Signal-time lookup uses the
active frame's owner slot and returns no Go heap handle.

The signal path allocates nothing and takes no blocking lock. Nested readers
increment independently. `sigprof` can run during STW, so STW is not the
reader grace period. Active JIT frames root their owner through stack maps;
the index epoch protects only lookup records. The existing per-M PC-value
cache bypasses JIT PCs because cached PC-relative values have no generation.

The current Linux/amd64 `findfunc` caller inventory is `runtime/symtab.go`,
`traceback.go`, `tracestack.go`, `proc.go` (`sigprof`), `preempt.go`,
`stack.go`, `panic.go`, `heapdump.go`, `hexdump.go`, `mprof.go`,
`mcheckmark.go`, `race.go`, `debuglog.go`, `debugcall.go`, and `error.go`. Also audit
`findmoduledatap`, `funcInfo.funcInfo`, `FuncForPC`, `resolveNameOff`,
`resolveTypeOff`, `textOff`, `moduleTypelinks`, `reflectOffs`, `pcvalue`, and
saved-PC producers. `plugin.go` and non-Linux/amd64 callers are outside this
target and get separate disposition. Any new caller joins this inventory.
Long-lived profile, trace, and panic records store
`CodeLoc{moduleID, generation, textOffset}` and copied symbol/file/line text;
they do not keep a bare PC or `funcInfo` after the reader/owner lifetime.
Audit `runtime/pprof` as an internal consumer: it must use owner-aware PC
handles and detach samples before storage, never call a legacy raw-PC producer
that would permanently pin every sampled JIT generation.

#### 2. Raw PCs: permanent pin for legacy integers, typed handles otherwise

An integer PC or `unsafe.Pointer` to executable memory has no observable
last-use point. Use these uniform rules:

| Reference | Lifetime rule |
| --- | --- |
| Go function value, closure, `jit.Code`, `CodePC` | Carries a GC-visible owner edge and unloads after all such edges disappear. |
| Explicit `Pin` or foreign registration | Strong runtime root until explicit `Release` after deregistration and quiescence of every external user. |
| Legacy API returning a JIT address (`runtime.Caller`, `runtime.Callers`, `CallersFrames.Frame.PC`, `Func.Entry`, `reflect.Value.Pointer`/`UnsafePointer` for a function, or an equivalent PC/address surface) | Pins that generation until process exit. It cannot promise reclamation. New runtime/library paths use owner-aware APIs. |
| Public profile/stack record exposing PCs (`MemProfileRecord`, `BlockProfileRecord`, `MutexProfileRecord`, `StackRecord`, `GoroutineProfile`) | Pins each exposed JIT generation until process exit; an integer array has no observable last use. Internal `runtime/pprof` storage uses detached `CodeLoc` records instead of these public compatibility surfaces. |
| Runtime-retained trace/profile/panic sample | Detached generation-relative `CodeLoc` and copied symbols; no executable pointer. |
| Untracked unsafe address, unknown raw-PC path, or foreign retention found in Eval or a linked effect summary | `Auto` rejects before publication. Known legacy raw-PC APIs install a process-lifetime pin; explicit foreign use requires `Manual`/`Pin` or a rooted `runtime/cgo.Handle`. |

`Pin` is an opaque handle backed by a runtime root table keyed by monotonic ID
and generation. The table, not the Go token, is authoritative; losing a token
leaves a visible pin rather than risking use-after-unmap. Insert the root
before exposing the address. `Release` is idempotent, keeps a temporary local
owner root across removal, uses the GC write barrier, and does not promise
immediate unmapping. Its precondition is that every foreign registration and
copied raw address has been removed and quiesced. If that cannot be proven,
keep the pin. Active Go frames independently root owners.

The fork changes internal PC APIs to owner-bearing handles. Compatibility-
shaped public APIs that return `uintptr` keep their signatures but permanently
pin any JIT generation they expose. This is the conservative cost of an
untrackable integer API; ordinary Eval, closures, reflection values, and
internal profiling do not take that path.

#### 3. GC representation: allocation-side owner edge, unchanged user layout

Executable memory remains outside the Go heap. `jitOwner` is the GC-visible
lifetime object; the external control record and scan descriptors are runtime
metadata and are not ordinary GC roots. A live owner-associated allocation
must nevertheless keep its owner alive. Carry that association in
runtime-defined allocation metadata, not in bytes before or after the user's
object. Reject hidden tail words: they make the allocator extent larger than
`T`, can be overwritten through unsafe access, complicate slice capacity and
finalizer rules, and are unnecessary once allocation metadata is extended.

Do not register RX pages as Go heap objects. The collector's object lookup,
allocation liveness, and scan maps are built around `mheap` arenas and `mspan`
allocation bits; an `mmap` range does not become a GC object by being reachable
from a Go pointer. Turning executable pages into ordinary heap allocations
would also conflict with the required W^X transition. The owner and explicit
runtime hooks integrate lifetime with GC while code and immutable metadata
remain separately mapped, accounted OS resources.

For each `(owner, allocation type/shape)`, create an immutable JIT scan
descriptor in the LinkUnit's accounted non-GC mapping. It contains a
scanner-compatible view of the ordinary type's exact size, pointer span, and
GC bitmap/program, plus the owner identity and an internal JIT descriptor
tag. It is not a language-visible type identity and is never placed in an
interface or reflection value. The descriptor's owner pointer is a manually
traced GC edge: the collector shades it while scanning a live JIT allocation.
At allocation publication, the runtime also shades the owner if marking is
active, because a new object is allocated black and the descriptor is outside
ordinary write-barrier scanning. The explicit barrier is required; relying on
the owner's temporary presence on the mutator stack is insufficient.

Add JIT small-object span classes after the existing normal scan/noscan
classes. `NumSizeClasses` is 68, including reserved class zero, so there are
67 allocatable small size classes: normal encodings occupy 0..135 and JIT
classes occupy 136..202. This gives 203 central/cache slots and fits
`uint8 spanClass`. Preserve normal encodings (`2*sizeclass + noscanBit`)
exactly. Encode JIT class `136 + (sizeclass - 1)`; decoding returns
`sc - 136 + 1`. JIT classes are **always scan classes**. Their low bit is part
of the size-class number and must never be interpreted as the normal noscan
bit. `spanClass.noscan` returns false for JIT classes, and every size-class
consumer uses the JIT-aware decoder. Large allocations keep span class zero
and are identified by the tagged `largeType` descriptor.

JIT small allocations always use the headered representation, even when the
exact user type is smaller than `MinSizeForMallocHeader` or contains no user
pointers. Choose the physical size class from `userSize + MallocHeaderSize`;
the public pointer addresses the first user byte after the header. The header
names the scan descriptor, while that descriptor reports the exact user size
and user-field bitmap. No user payload is padded and no hidden bytes exist.
For large JIT allocations, `mspan.largeType` names the descriptor.
`typePointersOfUnchecked` must recognize the JIT class/descriptor before
taking the ordinary small-object heap-bitmap path; its iterator carries the
owner edge separately from the user-field bitmap. Since these are scan
classes, scanning also shades the owner when the user bitmap is empty.

The initial JIT allocator does not use GreenTea's inline span-mark scanner.
That scanner derives object addresses from the span base and assumes there is
no per-object header. JIT classes therefore use ordinary per-object mark bits
and `scanObject` under both collector implementations. Make the inline-mark
predicate class-aware and return false for JIT spans; do not reserve GreenTea
inline-mark bytes for them. This keeps GreenTea builds supported without
introducing a second header-aware packed scanner. A later optimization may
add one only with a separate proof for header geometry and owner shading.

Both the normal and GreenTea GC scanners must shade the descriptor owner in
addition to scanning the exact user bitmap. Cover allocation-black barriers,
checkmark, heap verification/dumps, race and memory sanitizers, finalizer
processing, `findObject`/interior pointers, and GC tracing. Scanner metadata
must not be dereferenced for free slots. Once the owner is found dead, its
retirement worker waits for the full sweep generation and the PC reader grace
period before releasing descriptors. Free small-object headers may still
contain stale descriptor addresses; prove that sweep skips free slots and
allocation rewrites the header before publication. Prove the corresponding
`largeType`/dead-span path too. Do not require clearing every dead slot if the
allocator invariants prove stale references unreadable; do not unmap until
that proof holds.

This metadata association also lets runtime helpers safely recover an owner
from a live object pointer: find its allocation base, inspect the JIT
class/header or `largeType`, validate the descriptor generation, and retain
the owner while copying or growing. Apply this to `growslice`, string/slice
conversions, reflection constructors, and every runtime path that allocates
from or copies an owner-associated value. It never reads past slice capacity.
JIT-created strings that can outlive Eval must use copied owner-associated
backing storage; pointer-free bytes copied into detached host storage need no
code owner. A missed allocator or copy path is an unload-safety bug.

| Dynamic value | Required edge and storage |
| --- | --- |
| Function value, including captureless/top-level functions and method values | Compiler-generated `funcval` environment stores owner and any receiver/captures. Never expose a bare JIT entry as a Go function value. |
| Active JIT frame | Hidden owner slot is live in every safe-point and async-preemption stack map. Entry trampolines root it until initialized; direct/cross-unit calls propagate it. |
| `new(T)`, `&T{}`, escaping local, boxed interface payload | `mallocgcWithOwner` uses the JIT allocation class/header and exact type bitmap; the descriptor scan shades owner. A zero-sized value that could escape gets a unique owner cell, never `zerobase`. |
| Slice backing array and string bytes | Owner-associated array descriptor; `growslice` and conversions recover/propagate owner before publishing the new header. Image-backed strings are copied before return. |
| Map | Add a scanned owner field to runtime `maps.Map`; allocate pointer-bearing bucket/key/value storage with owner-associated scan descriptors. Keep compiler-known `used` at offset zero. |
| Channel and goroutine | Add owner to scanned `hchan`; buffers use owner-associated scan descriptors. Goroutine closure and suspended JIT frames retain owner. |
| Dynamic type and itab | GC heap ABI cells begin at ABI-required address, have precise scanned layouts, and carry owner/dependency edges. Variable pointer-bearing components use typed heap sidecars or owner-held typed roots. The fingerprinted ABI adds a fixed `JITOwner` field; static type cells encode nil and dynamic cells point to their owner. |
| JIT mutable package state and pointerful constants | Typed heap cells rooted by `jitOwner`; no pointerful globals in mmap data/BSS. Host globals remain direct relocations to their existing addresses. |
| Capture cell | Caller compiler promotes every visible variable to a typed GC cell before Eval; returned closures retain that same cell and their code owner. |
| Interface, `reflect.Value`, method value, panic/defer payload | Typed references reach an owner-bearing descriptor, allocation, or function environment. Names/file strings read from JIT metadata are copied. |

Every JIT-generated allocation helper receives the current owner explicitly;
it does not infer the owner from a PC or goroutine-global variable. Runtime and
reflection constructors that allocate from a dynamic type (`newobject`,
`reflect.New`, `MakeSlice`, `MakeMap`, `MakeChan`, and growth/copy helpers)
derive it from `abi.Type.JITOwner` or the source object's checked allocation
descriptor, then use the JIT allocator. This covers allocations initiated
later by host code using a JIT type or owner-associated slice/string. For
pointer-free data copied into detached host storage, no owner is required
after the copy no longer refers to JIT code, types, or image data.

#### JIT allocation and scan source closure

| Existing source path | Required change for the JIT allocation class/descriptor |
| --- | --- |
| `runtime/mheap.go`, `mcache.go`, `mcentral.go`, `mgcsweep.go` | Expand the class tables and preserve normal two-variant class values; decode the appended JIT range consistently in allocation, central lists, sweep, and inline-mark metadata. Keep per-size statistics indexed by decoded size class. |
| `runtime/malloc.go`, `msize.go`, generated allocation stubs | Add an owner-aware allocation entry that uses a JIT header for every small object, selects matching size classes, writes the descriptor before publication, shades owner during mark, and never takes the tiny allocator. Preserve generated normal allocation encodings. |
| `runtime/mbitmap.go`, both `mgcmark` scanners | Recognize JIT headers before the small heap-bitmap path, scan the exact user bitmap, and expose the descriptor owner separately for GC marking. Cover barriers, checkmark, interior-pointer lookup, GreenTea inline scanning, and free-object filtering. |
| `runtime/mfinal.go` | Identify the data address after the forced header even for small JIT spans. Carry the owner in the finalizer special and transfer it into the queued finalizer record; manually scan the special owner, include the queue owner in `finptrmask`, and retain it through callback completion without retaining the target. |
| `runtime/cgocall.go`, `mcleanup.go`, `heapdump.go`, `traceallocfree.go` | Make object-pointer scanning understand the descriptor and its user type. Keep custom owner shading restricted to GC marking; expose the semantic user type, not the synthetic scan descriptor, to heap profiles, dumps, cleanup callbacks, and tracing. Version any trace encoding that exposes the new class range. |
| `runtime/arena.go`, `pinner.go`, `mcache.go` reusable-object paths | Preserve current arena rejection and pin semantics. Ensure JIT objects never enter no-scan reuse/tiny paths; verify pinner indexing remains based on the containing allocation slot. |

This is a bounded source inventory, not a proof by filename. Before release,
search every `spanClass` construction/decoder and every
`typePointersOfUnchecked`/`largeType` consumer after the change. A path that
cannot represent the JIT descriptor must reject the JIT object or return a
semantic type view; it must not interpret the synthetic descriptor as a Go
type or dereference it after retirement.

The owner holds typed roots to all dynamic type cells, cache cells,
pointerful state cells, and owner dependencies referenced by its code. Internal
cycles are intentional and collectible: when no external edge reaches the
owner/cells, GC collects the whole cycle and its one retirement special. An
escaping dynamic type cell also points to its owner, so a `reflect.Type` by
itself keeps code and metadata alive. Cross-unit relocations install a strong
dependency edge in the source owner before publication.

Dynamic type metadata uses an explicit ABI change, not a hidden owner word
after a variable-sized type descriptor. Add a fixed `JITOwner` pointer field
to `publicinternal/abi.Type`; ordinary static descriptors use nil, while
dynamic type cells are GC heap objects and their owner field is scanned. Do
not add an owner field to serialized `abi.Name`: names are offsets into
immutable metadata, not independently owned values. Internal resolution
returns an owner-bearing `NameRef`/`TypeRef` lease, and public reflection
strings copy bytes before returning. Keep variable-sized names, GC programs,
and immutable tables in the owner's mapping. Pointer-bearing type dependencies
that live in heap cells are also held in the owner's typed dependency list;
the mapped payload is never relied on as a GC root.

`resolveNameOff`, `resolveTypeOff`, and `textOff` use the owner context and
resolve offsets only within that generation's immutable metadata ranges, with
checked bounds. Some existing call sites pass a pointer to a name record or
name bytes rather than a type cell, so publish a second metadata-address index
in the same immutable epoch snapshot as the PC index. It covers every JIT
metadata range and returns an owner-bearing `NameRef`/`TypeRef`/`CodeLoc`;
callers may not keep a naked pointer after its owner context ends. A JIT type
relocation to a type in another owner is a direct typed reference and installs
an owner dependency before publication. Dynamic paths never use the
process-global `reflectOffs` integer namespace. A `textOff` result stays
inside an owner-aware runtime operation; any public raw-PC result goes through
the pin rule above. This is a hard-fork ABI change, so every compiler, linker,
runtime, reflection, and object-schema consumer must use the same
fingerprinted layout.

`AddCleanup` and `Pinner` keep their ordinary behavior. A JIT allocation's
scan descriptor retains the owner while the object is normally live. Finalizer
processing is separate: sweep can revive a previously unmarked target after
ordinary mark scanning. Use the existing finalizer root path with an explicit
owner handoff, rather than a side table keyed by a recyclable special address:

1. Resolve the target's JIT owner from its checked allocation descriptor.
   Store it in `specialfinalizer.jitOwner`, which is non-GC memory. Extend
   `gcScanFinalizer` to scan that owner pointer as a root, without marking the
   finalizable target itself. When `SetFinalizer` installs a special during
   concurrent marking, the existing `addfinalizer` barrier must immediately
   shade both the finalizer function and `jitOwner`.
2. When sweep queues the finalizer, copy `jitOwner` into a pointer field on
   the persistent finalizer queue record before freeing the special. Add that
   field to the finalizer queue pointer mask and its layout assertions. Queue
   records are already scanned by `markroot`; the owner field keeps code and
   dynamic type metadata alive while the callback can run or trigger another
   GC.
3. Keep the queued owner field through callback execution and clear it with
   the same ordering used for the existing `fn`/`arg` roots. If the callback
   resurrects the target, its allocation descriptor shades the owner on the
   next collection. If `SetFinalizer(obj, nil)` removes the special before it
   is queued, removing the special also removes its owner root. If sweep has
   already queued it, preserve the queue root until callback completion, as
   the existing finalizer queue owns the pending callback.

This avoids a global owner-root table, special-address ABA, and an extra
root-list synchronization protocol. The special owner field is manually
scanned because specials are outside the Go heap; the queued owner field is
covered by the queue's precise pointer mask. The runtime finalizer mechanism
must retain its existing target resurrection and queue ordering semantics.
A dynamic type's owner field remains another edge. JIT cleanup function values
retain owners through their closure environments. The internal owner has no
user finalizer. `Pinner` can pin dynamic Go data but cannot pin RX code; use
`Pin` for code addresses.

User arenas are rejected for `Auto`; arena memory is not traced or cleanup-
eligible. `Manual` may use it only while a pin remains held through
`Arena.Free` and external quiescence. Unknown foreign/unsafe effects also
reject `Auto`. The binary carries transitive effect summaries for every
linked Go symbol; unknown is unsafe, never implicitly harmless.

#### 4. Cache disposition: no process-global strong edge to a JIT owner

Static-module cache behavior remains unchanged. The rule for every cache
containing a JIT type, itab, module, or code address is: use an owner-held
typed cache, a weak-value cache with pointer-free keys, or bypass it. A new
cache must be classified before use.

| Cache or metadata path | JIT policy |
| --- | --- |
| `runtime.itabTable` / `getitab` | Static types use the existing table. Dynamic types/interfaces bypass it and use typed JIT itab cells. The global table never receives JIT cells. |
| ABI type-assert/interface-switch caches | Static inputs keep current behavior. Dynamic inputs bypass unscanned uintptr caches. JIT call sites use owner-held typed caches with scanned type/itab pointers. |
| `pinnedTypemaps`, `moduleToTypelinks`, `activeModules`, `firstmoduledata.next` | Static/plugin modules only. JIT modules do not enter process-lifetime collections; dynamic resolution is owner-relative. |
| `runtime.reflectOffs` and negative reflect offsets | JIT uses owner-relative offsets with generation/range checks. Reflect-created types that depend on JIT types are owner-bearing. No JIT pointer enters the global strong offset map. |
| Per-M `pcvalue` cache | Bypass JIT PCs; never cache generationless JIT PC/table values. |
| `reflect.ptrMap`, `lookupCache` (`ArrayOf`, `ChanOf`, `MapOf`, `SliceOf`), `funcLookupCache`, `structLookupCache`, `layoutCache` | Existing global path only for static types. Dynamic lookup uses owner-local construction/layout caches, then the global weak structural canonicalizer for Go type identity. Dynamic `structLookupCache` entries never enter its append-only strong slices. |
| Dynamic type canonicalization | Pointer-free structural key plus a weak value (`weak.Pointer` to the GC type cell). A live cell carries its owner/dependency edges; the global key never holds a type pointer. Entries include the canonical cell's generation. `Reclaim` prunes nil weak values, and retirement removes keys for that generation. |
| Dynamic itab canonicalization | Same weak-value/pointer-free-key policy, keyed by interface and concrete type identities. A live cell references its type cells and owners. Its generation index lets `Reclaim` and retirement delete stale keys without retaining the cell. |
| JIT module symbol registry | No global strong owner root. Cross-Eval references require owner-bearing `Code`/type handles and a dependency edge before publication; no lookup by bare PC or bare JIT symbol name. |

Weak-cache keys contain copied structural bytes and generation-tagged type
identities, never string views or pointers into executable metadata. The
reaper deletes key-index entries for its generation only after the unit is
unreachable; `Reclaim` also removes entries whose weak values have cleared.
If a weak value can still be promoted to a strong type cell, that cell's owner
edge keeps the generation live and its key remains canonical.

Named types declared in Eval are function-local and have identity
`(callSiteID, evalID, declarationOrdinal)`; repeating source creates a
distinct named type. This checkout has no public `reflect.TypeByName` API; do
not add one as part of this work. If the JIT API offers name lookup, it returns
an owner-bearing type handle and searches only explicitly live generations.
Unnamed types use Go structural identity: reuse a matching static descriptor,
otherwise intern by a canonical structural key in the weak table. A live type
cell retains its helper LinkUnit; once no value or code owner uses it, the weak
table cannot keep it alive.

#### 5. Lifecycle: transactional publication and exactly-once retirement

| State | Meaning and linearization point |
| --- | --- |
| `building` | Private RW/NX mapping; no runtime index or user value can observe it. |
| `ready` | Relocations, schema/ABI validation, roots, and stack/GC/unwind/type metadata are complete; text is RX and data is R/NX. |
| `published` | JIT sweep special is attached while the constructor roots owner; immutable index root publishes the range. Handles return only after this store. |
| `queued` | Sweep finds owner unreachable and transitions `published -> queued` exactly once; the preallocated control record enters the allocation-free JIT queue. |
| `sweep-drained` | The full sweep generation that discovered the dead owner has completed; no GC scan can still consult a descriptor from this LinkUnit. |
| `unpublished` | Reaper publishes a root without the range and flips epoch; new readers miss it while old readers finish. |
| `readers-drained` | Old-epoch count is zero; no reader can hold the old snapshot. Owner references are absent because sweep discovered the owner dead. |
| `unregistered` | Remove runtime type/PC registrations; prove no live allocation or runtime operation can dereference this generation's scan descriptors. Stale headers in free slots may remain only if sweep ignores them and allocation overwrites them before publication. |
| `unmapped` | The single complete mapping is successfully unmapped; address/generation may be reused and byte accounting is released. |
| `tombstone` | Any post-queue failure keeps mapping/control metadata non-executable and unreused, charges its bytes, and reports failure. Never guess that failed unmap/unregister is safe. |

Before publication, reserve one contiguous LinkUnit mapping whose control page
remains RW/NX and contains a `jitRetireRecord` and embedded `specialJIT`. The
record contains its generation, mapping bounds, index token, GC descriptor
bounds, and state, but no Go pointer to `jitOwner` or its children. `addspecial`
links the embedded runtime special to the Go-heap owner; `freeSpecial` sees it
only after the owner is unreachable and the special has been unlinked. It then
appends the record to a dedicated JIT queue without allocating or unmapping.
Use a short runtime mutex and a single reaper G for this queue. The reaper
releases the lock before waiting, unpublishing, or calling the OS. This avoids
reusing a lock-free node while another consumer may still hold it and avoids
making a finite pointer-tag wraparound part of the JIT lifetime proof. The
runtime change must audit lock ordering and the sweep call context; queue
insertion must remain bounded and must not allocate, invoke user code, or
block on a condition that requires the reaper.

The reaper processes each record only after a generation-aware sweep barrier
confirms that all spans for the relevant GC sweep are complete. It then
publishes an index without the generation, waits for the old reader epoch to
drain, removes owner-local/weak-cache keys, and proves that no live allocation
or runtime operation can dereference the LinkUnit's synthetic scan
descriptors. Stale free-slot headers may remain only under the sweep/overwrite
invariant above.
Only then does it unmap the one contiguous reservation. A single reservation
allows one checked `munmap` result and avoids partially retiring separately
mapped text/data/control segments. It includes code, metadata, scan maps,
and the control record; permissions are managed per page with W^X. If any
step fails, keep the mapping and accounting in a non-executable tombstone.

The runtime sweep barrier is an explicit implementation change, not an
assumption that seeing the owner special means all sweeping is done. Do not
use `mheap.sweepgen` alone as the JIT barrier: this checkout stores it in a
32-bit field and increments it by two, so it is a span-state encoding that
eventually wraps. Add a separate monotonic `uint64` JIT sweep sequence and a
completed-sequence high-water mark:

1. Under the heap lock in `gcSweep`, increment `jitSweepSeq` once for the new
   sweep cycle, check that it did not wrap, then update `mheap.sweepgen` and
   reset `activeSweep` with that sequence. Store the sequence in
   `activeSweep`; each valid `sweepLocker` carries the same value. A new GC
   cannot start a sweep generation until `finishsweep_m` has completed the
   previous one.
2. When the JIT owner special is freed, `freeSpecial` copies the current
   `jitSweepSeq` into its preallocated retirement record and queues that
   record exactly once. There is one record per LinkUnit, allocated before
   publication, so queue capacity is bounded by published units and enqueue
   never allocates. The reaper never holds the queue mutex while waiting.
3. In `activeSweep.end`, the transition to `sweepDrainedMask` is the exact
   point at which the final sweeper has left. After the existing cleanup-queue
   flush, publish the completing `sweepLocker`'s sequence to
   `jitSweepDone` using a release-ordered atomic max/CAS, then wake the JIT
   reaper through a preinitialized runtime `note`. Use max/CAS because a later
   sweep can advance while an earlier end call is finishing its post-drain
   work; an older completion must never move the high-water mark backward.
   This transition includes the extra sweep token used while stale mcache
   spans are flushed.
4. A record with sweep sequence `n` is eligible for descriptor retirement
   only after an acquire load observes `jitSweepDone >= n`. The 64-bit
   sequence never wraps; overflow is a runtime-fatal invariant violation.
   The reaper then removes the PC/metadata range, waits for the reader epoch,
   unregisters descriptors, and only then unmaps.

This high-water barrier cannot mistake a later sweep for the requested one,
and it does not confuse the span-local `uint32` sweep state with a JIT lifetime
generation. Queue wakeup has no user callback or allocation path. The reaper
does not keep the owner reachable; its control record is outside the Go heap
and is unmapped only after the sweep barrier, reader epoch, and allocator
metadata conditions have all completed. Process exit may leave mappings to
the OS, as already stated in the lifetime contract.

Pin acquisition roots owner before returning an address. `Release` removes
only that pin root under the GC root/write-barrier protocol and keeps a
temporary owner reference across the operation. Multiple pins use unique IDs;
duplicate release is a no-op. A unit reaches `queued` only after every pin
root and ordinary owner edge is gone.

Any pre-publication error synchronously rolls back private metadata and
mapping. Failure to attach the JIT sweep special aborts. A partial unit is never
executable. Concurrent compiles reserve byte budget before mapping;
`Reclaim` serializes index publication but not compilation. Bytes stay charged
through queued, unpublished, and drained states, and are debited only after
successful unmap. If live/pinned bytes exceed the limit, compilation returns a
typed budget error.

`Reclaim(ctx)` is ordinary-G API only. It rejects signal, GC, cleanup,
finalizer, or reaper contexts. Concurrent callers join one generation-scoped
request. It triggers complete GC/sweep, waits for eligible JIT queue records
and reader grace periods, and does not wait for arbitrary application
cleanups. Cancellation returns `ErrReclaimPending` without canceling safe
retirement; reentrant calls return `ErrReentrant`. It never unmaps early to
meet a timeout.

#### 6. Contract and supported build matrix

`Auto` is the default Eval path: callbacks, goroutines, interfaces, maps,
channels, reflection, panic/defer, and ordinary Go heap references retain
owners automatically. Unknown effects fail before publication. `Manual` adds
a root for raw PCs, foreign addresses, or unsupported unsafe behavior but
still uses correct Go stack maps, typed heap pointers, and GC barriers.
`Scoped` is a compile-time certified synchronous no-escape call, unmaps only
after the call and defers finish, and promotes to Auto before rethrowing an
escaping panic. It still registers PC/stack metadata while executing.

The regex object stores one owner-bearing `jit.Code` for its compiled matcher.
This Owned path permits no source captures or callbacks; dropping the Regex's
last reference makes its owner eligible for collection once in-flight calls
finish. It still uses normal owner tracing and sweep retirement. The separate
`Scoped` path applies to one synchronous invocation proven to have no escaping
values. General Eval remains Auto and supports arbitrary owner-carrying escapes.

| Facility | Linux/amd64 initial contract |
| --- | --- |
| Optimized binary with matching compiler/runtime/linker fingerprint | Supported. It embeds compiler/link metadata, not another Go installation or package archives. |
| GC, finalizers, `AddCleanup`, `Pinner`, stack growth, panic/defer, profiling, async preemption | Supported through owner edges, generated maps, and the JIT index protocol. |
| cgo in statically linked host packages | Allowed only with complete effect summaries. Values retained via `runtime/cgo.Handle` retain owners. Raw JIT addresses need `Pin` and deregistration. Unknown C/assembly effects reject Auto. |
| `unsafe` in dynamic source or transitive host call | Auto rejects unless the serialized effect summary proves no untracked code/data reference. Manual Pin preserves executable mapping but does not make invalid heap pointers safe. |
| User arenas | Auto rejects. Manual requires a pin through `Arena.Free` and external quiescence. |
| Race, ASan, MSan, Valgrind, coverage, plugins contributing JIT-visible symbols, unlisted runtime `linkname` | Fail closed until each has an explicit metadata/ownership design and fingerprint. Static plugins remain process-lifetime but do not provide JIT symbols. |
| Linux/arm64 and other OSes | Not included. Each needs its own ABI, atomic, relocation, unwind, cache-maintenance, W^X, and source-path closure. |

The embedded metadata schema fingerprint covers compiler, linker, runtime,
ABI type descriptors, relocation set, GC map encoding, and enabled build
modes. A mismatch is a load error. Effect summaries are transitive over
selected linked callees and include assembly, linkname, and cgo boundaries:
`MayRetainGoValue` is safe through owner-bearing values. `MayExposeRawPC` is
allowed only at a runtime API that pins before returning; `MayEscapeToForeign`
requires a rooted handle or explicit pin/deregistration pair. `MayUseUnsafe`
is recorded but is not by itself a rejection: the summary must prove it does
not hide a Go pointer, owner, code PC, or metadata address in an unscanned or
foreign location. `MayAllocateArena` and `Unknown` reject Auto. The effect
schema also distinguishes `MayStoreOwnerlessAddress` and
`MayEscapeJITMetadata`; either rejects Auto unless it is covered by a pin.

#### 7. Traceable safety argument

The proof target is a set of source-level invariants, not a claim that tests
prove an entire runtime correct:

1. Every live JIT executable reference is a typed owner edge, a manual pin, or
   an active JIT frame whose stack map roots its owner.
2. Every JIT index-record dereference is inside an announced epoch; a reader
   that exports metadata first exports a strong owner edge.
3. Every dynamic type/value that outlives Eval has an owner edge through its
   descriptor, allocation scan metadata, function environment, container
   header, or explicit pin.
4. No process-global strong cache, module list, profile record, or cleanup
   argument points back to an owner that should become unreachable.
5. Exactly one `published -> queued` transition wins. Unpublication precedes
   grace completion; grace precedes metadata removal; removal precedes unmap;
   successful unmap precedes address reuse.
6. Every mapped byte is charged to a live owner, active pin, queued record, or
   explicit tombstone until unmap succeeds.
7. Unknown runtime/build/effect paths fail closed.

Implementation review produces a source-to-invariant matrix for the complete
`findfunc` caller set above; every raw-PC producer; every dynamic allocator
(`newobject`, typed `mallocgc`, `newarray`, `growslice`, map/bucket creation,
`makechan`, reflect constructors); every function-value and goroutine entry;
all caches in the disposition table; error/trace/panic/profile/finalizer/
cleanup/cgo/unsafe paths; and every control-record transition. Review traces
owners and pointer maps through compiler output and runtime source, not only
fixture results. Forced-GC, callback, interface, type, PC, panic, budget, and
failure-injection fixtures remain regression/counterexample tools.

#### 8. Source audit: answers to the design questions

This audit is against Go revision `d6f6cf8156d3dc7e5a9a3c3497cdef8132dbc90a`
in this workspace. It inspected source and did not build or run tests.

| Question | What the checked-in source says | Design answer |
| --- | --- | --- |
| Why does the runtime need PCs and module metadata? | `runtime.findfunc` calls `findmoduledatap`, which walks the static `moduledata.next` chain. The result drives PC-to-function lookup, stack maps, traceback, panic/defer, preemption, profiling, and symbolization. `funcInfo` currently contains only `*_func` and `*moduledata`; `FuncForPC` can return a pointer into that metadata. | Keep JIT ranges in a separate immutable generation index. Extend internal `funcInfo` and exported wrappers with an owner or copied metadata. Inventory and convert every `findfunc` reader; STW by itself cannot protect readers such as signal-time profiling. |
| What is a PC lifetime problem? | A PC is an address, often stored as `uintptr`; it has no GC edge and no observable last-use. Public raw-PC surfaces include `Caller`, `Callers`, `CallersFrames.Frame.PC`, `Func.Entry`, function-valued `reflect.Value.Pointer`/`UnsafePointer`, and profile/stack records such as `MemProfileRecord`, `BlockProfileRecord`, `MutexProfileRecord`, and `StackRecord`. | New APIs return owner-bearing `CodePC`/function handles. Compatibility APIs that return JIT PCs install a permanent generation pin. Internal profiles/traces use generation-relative `CodeLoc` plus copied strings so sampling does not pin code forever. |
| Can `AddCleanup` be the destructor? | `runtime.AddCleanup` is asynchronous, arbitrarily delayed, not guaranteed before process exit, can be blocked by user cleanup, and can be prevented by references from its callback/argument. It runs through shared cleanup machinery. | No. Unreachable means eligible for retirement, not already unmapped. Use a dedicated runtime special and JIT queue. If no GC occurs, or at process exit before retirement, the external mapping remains mapped; it is not marked free. A completed GC plus JIT drain can reclaim eligible units. |
| Do interface/type/reflection caches release dynamic metadata? | `runtime.itabTable` allocates itabs with `persistentalloc`; method slots are raw PCs. ABI type-assert and interface-switch caches store type/itab addresses as `uintptr`. `reflect.ptrMap`, `lookupCache`, `funcLookupCache`, `structLookupCache`, and `layoutCache` are global strong caches; the StructOf comment explicitly says it pins memory. | Use the static type catalog plus the canonical identity protocol below. Dynamic types and itabs bypass static global paths or use owner-local cells. Any shared dynamic canonicalizer uses copied full-identity keys and weak values. Add owner-aware type/name-offset resolution and never add JIT entries to module typelinks, `reflectOffs`, or the static module chain. |
| Can the current mapper participate in `GOMEMLIMIT`? | `src/jit/image_map_linux_amd64.go` calls raw `mmap`/`munmap`. Runtime `sysAlloc`/`sysFree` update `memstats.other_sys` and `gcController.mappedReady`; `memoryLimitHeapGoal` uses `mappedReady`. The current JIT mapping is therefore invisible to these counters. | Add a runtime-owned JIT map/unmap API that accounts the page-rounded full reservation, exposes mapping bytes to runtime stats/pacing, and triggers the pressure/reclaim path before budget exhaustion. Keep accounting until successful unmap; current `sysFreeOS` has no error result, so the JIT API needs checked unmap semantics and a charged tombstone on failure. |
| What do `unsafe` and C references mean for ownership? | GC traces typed Go pointer fields, not integers containing addresses. A `runtime/cgo.Handle` roots the Go value stored in the handle table, but does not make an arbitrary code address safe. | Auto rejects unknown address storage or foreign retention. A Go callback passed through a rooted handle keeps its owner-bearing function value alive. A raw JIT PC stored by C or in an integer needs `Pin` until the foreign registration and all copied addresses are removed and quiesced. `runtime.Pinner` pins Go heap objects, not executable mappings. |
| Which package/import context does Eval receive? | Go imports are file-scoped. The package path is package-wide, but an alias imported in a different source file is not in scope at this call site. | Parse a statement/function body with the containing package path, the imports from the file that contains the Eval call, package declarations and type information, and explicit capture descriptors. The snippet has no `package` or `import` clause. Resolve names against the existing executable's host symbols; do not rebuild/import package archives at runtime. |

The consequences are concrete: ownership is a traced Go object graph, while
the executable image and its GC scan descriptors are separately accounted
resources. Allocation descriptors add a runtime-traced owner edge without
changing user object bytes or type identity. Collection decides when an
owner is unreachable; a JIT-specific runtime hook queues retirement; a
generation-aware sweep barrier protects allocator metadata; the PC-index
grace period protects lock-free readers; and only then can the runtime unmap
and debit the reservation. A leaked or pinned owner remains real mapped
memory and remains charged. It is never silently treated as free memory.

For the regex object, retain one owner-bearing `jit.Code` from the Regex
object. This is the **Owned** fast path: no source captures or callbacks are
allowed, and dropping the Regex's last reference makes its owner eligible for
collection once in-flight calls finish. It still uses the ordinary owner
edge and sweep retirement path. `Scoped` is a different specialization for a
single synchronous invocation that proves no value, type, string, slice,
callback, or PC escapes; it can retire after the call, defers, and reader grace
period without waiting for GC. Neither mode replaces general `Auto` Eval.

The architecture questions above now have source-backed answers. Production
safety is still conditional on implementing the hooks and completing the
source-to-invariant audit. In particular, the JIT span-class scanner path,
allocation-time owner shading, finalizer owner-root lifecycle, stale
scan-descriptor references, and generation-aware sweep barrier are
implementation proof obligations, not properties already supplied by this
runtime. The same is true of complete coverage for every JIT-visible
`findfunc` caller, every dynamic allocator and
reflection cache, every raw-PC producer, and checked mapping failure/accounting.

#### Research notes

V8 describes closures as references to code and lexical context and collects
unused compiled code; this supports explicit owner edges and capture metadata,
not untracked executable addresses ([V8 lazy parsing](https://v8.dev/blog/preparser),
[V8 bytecode flushing](https://v8.dev/blog/v8-release-74)). LLVM ORC makes
resource removal explicit and separates bookkeeping from expensive cleanup
([ResourceTracker](https://llvm.org/doxygen/classllvm_1_1orc_1_1ResourceTracker.html),
[ResourceManager](https://llvm.org/doxygen/classllvm_1_1orc_1_1ResourceManager.html)).
Go documents that `AddCleanup` can be delayed arbitrarily and is not guaranteed
before exit, motivating a dedicated sweep queue and `Reclaim` barrier
([runtime cleanup source](https://go.dev/src/runtime/mcleanup.go)). Go's weak
pointer implementation coordinates weak-to-strong conversion with sweep,
which fits the weak canonicalization tables
([weak pointer source](https://go.dev/src/weak/pointer.go)). None is a drop-in
Go implementation: this fork still needs its own compiler ABI, GC layouts,
runtime readers, and source audit.

### Production implementation sequence

1. **Freeze source and escape semantics.** Specify statement grammar,
   package/file scope, all capturable locals, result behavior, repeated
   evaluation identity, unit-level retention, unsafe/C/raw-PC policy, and
   Auto/Manual/Scoped plus Release/Pin semantics.
2. **Complete package metadata and normal-link roots.** Keep export data and
   runtime type-check metadata separate from object code. Root Eval-visible
   packages before dead-code elimination. Emit a relocation-backed host
   symbol manifest and a static type identity catalog from the normal linker's
   selected symbols and complete Eval-visible type closure.
3. **Add compiler call-site descriptors and capture ABI.** Remove runtime
   path guessing. Pass package/import/type scope and typed capture slots.
   Promote every visible local dynamic source can name. Preserve variable
   identity, writes, loops, and closure aliasing after the caller returns.
4. **Complete the in-memory object linker.** Cover the declared Linux/amd64
   relocation and symbol profile, resolve static type references through the
   final type catalog, and produce complete pcln/stack/GC/unwind/type metadata,
   private helpers, ABI wrappers, methods, generic dictionaries, and host
   globals. Reject malformed or unsupported objects before mapping.
5. **Create the mapping and module registry.** Use a runtime-owned, accounted
   reservation API; map RW/NX, relocate, flush instruction cache, seal W^X,
   and atomically publish an immutable snapshot under the two-epoch reader
   protocol. Keep pointerful globals out of module data/BSS and do not mutate
   the static module chain.
6. **Implement owner edges.** Add owners to closure environments and frames,
   dynamic type/itab cells, captured locals, maps, channels, package state,
   and dependencies. Add the JIT span-class/header path, allocation-side scan
   descriptors, explicit collector shading, `growslice`/reflection owner
   recovery, and finalizer-record owner retention. Keep user object layouts
   exact; verify pointer maps, barriers, checkmark, GreenTea scanning, and
   allocator reuse invariants.
7. **Partition runtime caches.** Implement the canonical type identity
   protocol above and keep host static caches unchanged. Apply the exact
   owner-held, weak-value, or bypass disposition from this document to JIT
   dynamic types, itabs, reflection layouts, type assertions, and typelinks.
8. **Add sweep cleanup and reaper.** Attach a JIT sweep special to every owner
   before publication, linked to its RW/NX control-page record. Sweep performs
   a bounded, allocation-free enqueue under a short runtime lock. A worker
   waits for the exact sweep generation, drains PC-index readers, unregisters
   metadata, verifies allocator descriptor references are unreadable or
   overwritten before reuse, and unmaps. Add
   checked runtime map/unmap accounting and a JIT-specific wait/drain API. A
   Manual owner remains in the typed pin-root table; final explicit release
   removes only that root. Pin roots are observable and never removed by
   finalizers.
9. **Add the scoped certificate after general semantics.** Reuse compiler
   escape summaries; add callback-retention and raw-storage effects those
   summaries omit. Verify the compiled object. Unknown calls use general Eval.
10. **Close the Linux/amd64 safety argument.** Produce the source-to-invariant
    coverage record from “Source-grounded design decisions” and review each runtime,
    compiler, linker, and reflection path against it. Forced-GC, callback,
    capture, dynamic-type, cleanup, reader, budget, panic, pin, and foreign
    fixtures are regression and counterexample tools; they do not establish
    the safety argument or cover unenumerated paths.
11. **Port independently.** Add Linux/arm64 only with its own relocations,
    calling convention, cache maintenance, unwind/stack maps, W^X, reader,
    and cleanup proofs. Add other OS/architecture pairs separately.

### Runtime behavior and regression gates

- A callback stored in a host global, map, channel, interface, or imported
  package remains callable after Eval returns and keeps its code mapped.
- A goroutine blocked inside JIT code or waiting to invoke a callback
  survives forced GC; owner and runtime metadata remain available.
- Captured host locals remain the same storage after the Eval caller returns;
  assignments through Eval and the host are mutually visible.
- Dynamic type and itab values survive reflection, interfaces, maps, channels,
  type assertions, type switches, generic calls, and method values without
  entering a permanent global cache. Removing the last value eventually
  removes every cache/index reference.
- Cleanup may free the target owner before the reaper finishes because the
  reaper touches only its independent control record. The mapping remains valid
  until readers drain and disappears only after unpublication.
- Legacy raw-PC APIs pin until process exit; CodePC handles carry owners;
  profile/trace/panic records detach symbol data. Reused addresses never
  alias old generations.
- A manual pin remains a strong root even if its Go token becomes unreachable;
  only explicit release removes it. Release with an active raw-PC or foreign
  user is documented as a caller contract violation, and never guessed safe
  by the runtime.
- A live closure or dynamic type retains its complete LinkUnit in the initial
  production contract. Link-time dead-code elimination removes unused code;
  finer per-function/page-unit reclamation is separately measured and proved.
- No registration, cleanup argument, cache, module index, or runtime callback
  forms a strong path back to the owner that is supposed to become unreachable.
- Under a configured byte limit, pressure forces collection and waits for
  eligible cleanup. Live or pinned modules produce a deterministic error.
- The no-escape path is selected only with a complete certificate; every
  uncertain effect selects general Eval, and a crossing panic pins its owner.
- Unsupported OS/runtime modes fail closed. No partial module is executable
  or visible after an error.

The current tree still has manual Code.Close, incomplete host manifests and
relocation coverage, no owner propagation, no dynamic type/cache teardown,
and no JIT-specific cleanup/reaper or manual pin root table. The selected
architecture uses module-level LinkUnits first and leaves per-function
reclamation as a compatible optimization. The design choices are now closed
for Linux/amd64; the implementation is not production-ready until it satisfies
the source inventory and invariants above. No tests were run while revising
this plan.
