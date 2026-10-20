# In-memory JIT linker

## Implementation status

The worktree implements an in-process JIT path for a deliberately bounded
Linux/amd64 subset. Direct `jit.Eval` call sites carry package/file context and
typed addresses for capturable locals. The compiler emits code-free export
metadata and a normal-linker manifest; the in-memory linker consumes selected
object symbols and an explicit relocation subset. Unsupported object forms
fail before publication.
The current capture descriptor is still name-keyed and uses rendered type
expressions for some runtime reconstruction; call-site binding IDs and the
compiler type-graph protocol specified below are target design, not implemented
ABI.

The runtime uses a separate JIT PC index, GC-visible module owners, a
pointer-free per-span owner side table, explicit pins for raw-address escape,
sweep-queued retirement, reader draining, checked unmapping, and byte
accounting. Reflect-created descriptors and dynamic itabs use owner-aware
metadata and weak caches. Pointerful JIT globals are scanned while their owner
is live. Finalizer and cleanup callback PCs are included in GC root scanning.
Race, MSan, ASan, coverage, and non-Linux/amd64 execution fail closed.

The architecture decisions and the remaining implementation gates are recorded
below. The current Linux/amd64 verification pass completed for the recorded
subset; the full runtime package has only
environment-blocked failures in socket and ptrace tests. The owner side-table
design is the implementation choice; older sections below that propose custom
span classes, hidden object headers, or an `abi.Type.JITOwner` field are
superseded design candidates, not current requirements. Current implementation
limits include function-local defined-type and type-parameter captures, some
generic type reconstruction, incomplete relocation and host-symbol coverage,
conservative process-lifetime pins in several public profiling/tracing APIs,
and compiler-emitted unnamed type identity across independently loaded JIT
modules. The `Owned` and certified `Scoped` APIs, complete effect-summary
generation, and the source-to-invariant proof are not implemented. The source
audit below records paths inspected; it is not proof that all external
linkname, unsafe, or foreign callers obey the ownership contract. Do not treat
prior narrow fixtures or this source description as evidence of production
readiness.

## Runtime source audit coverage

The following implementation paths were traced in this worktree. This table
records where the current mechanism is implemented and its boundary; it does
not claim formal verification or exhaustive coverage of unsupported callers.

| Invariant | Source path inspected | Current mechanism and remaining boundary |
| --- | --- | --- |
| Marked JIT-dependent heap objects retain their owner/type edge | `src/runtime/jitobject.go`: `jitSetHeapOwnerEdge`, `jitMarkSpanOwner`; `src/runtime/mgcmark.go`: `greyobject`, `shade`, `jitMarkOwnerAddr`; `src/runtime/mgcmark_greenteagc.go`; `src/runtime/mgcmark_nogreenteagc.go` | Side-table edges are shaded from ordinary/checkmark marking, Green Tea marking, pointer scanning, and write-barrier paths. This relies on generated Go heap pointers and the described supported allocation paths. |
| Reused allocation slots cannot inherit a stale owner edge | `src/runtime/malloc.go`: `mallocgc`, `jitFinishHeapAllocation`; `src/runtime/jitobject.go`: `jitClearHeapObjectOwner`; `src/runtime/mheap.go`: `freeSpanLocked`, span initialization | Allocation installs or clears the side-table slot; freeing a span releases its pointer-free table. Unusual allocator modes and unsupported build modes remain fail-closed or unverified. |
| External writable JIT globals are roots only while their module owner is live | `src/runtime/jitindex.go`: `jitScanGlobals`; `src/runtime/jitmodule.go`: global bitmap validation and publication; `src/runtime/mgc.go`: mark-done integration | Validated pointer maps are scanned under an index lease only after the owner is independently marked. Mutation after the scan relies on the ordinary termination/write-barrier protocol. |
| Runtime-held callback PCs retain their code owner | `src/runtime/jitobject.go`: `jitMarkFuncvalOwner`, `jitMarkCleanupOwners`; `src/runtime/mfinal.go`; `src/runtime/mcleanup.go` | Finalizer and cleanup callback function values are included in root scans. Other runtime or foreign structures that encode PCs as integers require pinning or a separate owner-bearing representation. |
| PC/module metadata is not freed while an index reader uses it | `src/runtime/jitindex.go`: `jitIndexReadEnter`, `jitPublishIndex`, `unpublishJITModule`; `src/runtime/symtab.go`: `findfunc`; `src/runtime/jitretire.go`: `jitRetire` | Immutable snapshots use two reader epochs; tombstones are checked before metadata dereference, and retirement waits for the prior epoch. `findfunc` returns an ownerless `funcInfo` after its lease, so only in-tree paths that pin the module or execute on an owner-bearing JIT stack are supported. Arbitrary external/linkname retention is not covered. |
| Owner retirement waits for sweep and index readers before unmapping | `src/runtime/mheap.go`: `_KindSpecialJIT` and `freeSpecial`; `src/runtime/mgcsweep.go`: sweep-generation completion; `src/runtime/jitretire.go`; `src/runtime/jitmemory_linux_amd64.go` | Sweep queues a pointer-free control-page record; the reaper waits for the recorded sweep generation, unpublishes the module, drains readers, frees runtime tables, and only then unmaps/debits. Failed teardown remains charged and queued for retry. OS failure recovery and shutdown behavior still require dedicated verification. |
| Raw PC APIs do not silently export an unowned reclaimable address | `src/runtime/symtab.go`: `FuncForPC`, `CallersFrames`; `src/runtime/jitindex.go`: `PinJITModule`, `pinJITStack`, `pinAllJITModules`; `src/reflect/value.go`: pointer/address export paths | Known compatibility paths pin before returning raw PCs or metadata handles. This is conservative and can retain modules for process life; an exhaustive inventory of external linkname users is impossible from this tree. |
| Dynamic reflection caches do not strongly root reclaimable types | `src/runtime/jitmodule.go`: weak structural type/itab caches and pruning; `src/runtime/iface.go`; `src/reflect/type.go`, `src/reflect/map.go` | Dynamic caches use owner-local typed cells or weak values with copied keys; some reflect-created composites are globally canonicalized. Compiler-emitted unnamed descriptors across separate JIT modules are not generally interned, and every future cache must follow the policy. |

The audit found no remaining in-tree `jitIndexReadLeave` call that dereferences
the encoded lease as a snapshot pointer. It also checked the heap-dump reader,
which must strip the low-bit lease token before testing the snapshot pointer.
These checks and the new concurrent-reader fixture are regression coverage,
not a proof against every runtime interleaving or external consumer.

## Current source path

The source tree contains an implementation attempt for the general path below.
This summary is based on code inspection, not proof that arbitrary snippets,
imports, captures, or unloading work. A separate TypeScript Go-target client
exercises a narrow Linux/amd64 dynamic-RegExp path through jit.Eval; its scope
is summarized below.

Direct source calls to `jit.Eval` are rewritten in Unified IR to call
`jit.EvalAtCaptured` with the resolved caller package path, physical source
file, and descriptors for visible locals, parameters, receivers, named
results, constants, and local aliases. Runtime compilation carries the
original file identity forward for nested direct Eval calls. Calls through a
function value still use the older `runtime.Caller` fallback and cannot capture
caller locals. The runtime path parses statements, compiles in-process, links
compiler output in memory, maps the result, registers runtime function
metadata, and executes through an amd64 trampoline. The snippet has no package
clause or imports; its generated function uses the caller package identity,
and imported symbols bind to host symbols retained by the normal executable
link. Capture support is deliberately fail-closed for unsupported type
identity and generic cases.

The worktree contains a versioned object-file root record, a linker pass that
roots the Eval caller package and imported package closures, and a
relocation-backed manifest builder for linker-selected symbols and unnamed
type descriptors. Runtime lookup uses package/name/ABI identities; direct
relocations to exact structural type aliases bind to the host descriptor.
End-to-end tests cover `fmt`, the imported
`jitcounter` package, and same-package package storage. They do not establish
arbitrary imported-package behavior or dynamic type and itab ownership.

The Linux/amd64 image mapper uses a runtime-owned mapping token for RW/NX
creation, page protection, checked unmapping, and runtime memory accounting.
JIT modules now use a separate pointer-free PC index with two-epoch reader
leases and module generations;
they are not appended to the static module chain. A sweep special queues a
pointer-free retirement record from the mapping's control page. The reaper
unpublishes metadata, waits for index readers, frees runtime tables, and then
unmaps. The runtime retries failed teardown without dropping its charged
reservation. A configurable byte limit covers image mappings, module tables,
and index snapshots; an over-limit allocation forces GC and waits for eligible
retirements before retrying once.

`Code.Close` rejects a close while a `Code.Run` call is active and drops the
caller handle; it does not unmap synchronously. Owner-aware entry functions,
function literals, and escaped pointers into a JIT mapping keep a GC-visible
owner alive. Runtime metadata is retired asynchronously after GC finds the
owner unreachable. Unsupported raw-address and foreign paths are pinned.
`Code.Pin` supplies an explicit raw-PC handle; callers must quiesce foreign
users before `Pin.Release`. `runtime.FuncForPC`, `Callers`, stack profiles, and
tracing pin affected modules for process lifetime because their public records
cannot carry an owner. Memory-profile samples pin only modules whose PCs they
retain. JIT dynamic types and itabs use owner edges and weak caches, and the
reflect constructors updated in this fork use descriptor-local offsets and
owner-aware identities. These lifetime changes still require the current test
pass and a source-complete audit.

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

Same-package declaration scope is implemented in the source path: saved
declarations are supplied to the in-process compiler, while existing package
symbols resolve back to the host binary. Direct Eval calls have an explicit
package/file identity path. The separate runtime module index, owner special,
reaper, weak owner lookup, preallocated raw-PC pin slots, and GC marking hook
for typed pointers into JIT mappings are now present in source, together with
lexical capture for supported variables, constants, and aliases, typed GC
scanning for pointerful image globals, plus mapping/runtime-table/index
accounting. Owner-local dynamic itabs are implemented, but dynamic type
construction, reflection-cache ownership, full owner propagation, broad linker
coverage, and end-to-end proof remain
unfinished. The partial-mapping error path returns a retryable `Code` handle
if OS unmapping fails; it remains an important failure-injection case. Each
stage needs fixtures that check identity and runtime behavior, not only
successful compilation.

For a lexical capture whose static type is an enclosing type parameter, the
compiler records `reflect.TypeFor[T]()` through the generic `CaptureValue`
constructor. Runtime snippet generation reconstructs a Go type expression and
adds package imports from the build's embedded export set as needed. Coverage
includes scalar and composite types, imported named types, anonymous structs
with tags, anonymous interfaces, and instantiated named generic types whose
arguments can be reconstructed from the runtime type name and embedded package
metadata. The renderer rejects local named types, unexported external types or
fields, missing export data, ambiguous package names, and unsupported runtime
type-name syntax. This is bounded support, not general type reconstruction.
The type-name grammar is tied to the compiler/runtime version embedded in the
binary; a format change must update the renderer and its compatibility tests.

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
ranges, stack maps, types, itabs, function tables, and typed pointerful global
roots in one version-checked descriptor. Runtime-created package state can
live in typed owner-held heap cells; compiler-emitted pointerful data carries
its exact GC type descriptor. Registration publishes
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

The schema/importer path removes imported `.a` archives from runtime metadata.
Build-time extraction reads the exact selected package archive for Go export
data; the runtime compiler receives that export payload. The compiler emits
linker-readable package roots, and the linker emits a relocation-backed symbol
manifest. The Linux/amd64 toolchain build and focused integration tests
exercise these paths, including shared imported package state. Their tested
coverage remains narrower than arbitrary Go imports, symbol kinds, and
relocations.

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
   descriptors, itabs, GC-described pointerful globals, and any required
   runtime tables. New runtime-created package state is owner-held typed heap
   data; static image globals are scanned only through their compiler type
   maps and only while their module owner is independently live.
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
   builds runtime tables, and records exact GC types for pointerful image
   globals. Runtime-created state uses typed owner-held heap objects. Unknown
   relocation
   or symbol kinds fail before registration; there is no fallback subprocess, plugin,
   temporary source/object file, or package archive.
8. **Register complete runtime metadata atomically.** The runtime descriptor
   covers text and pointer-free metadata ranges, function/stack maps, types,
   typelinks, itabs, and tables used by stack walkers,
   traceback, panic, interfaces, reflection, and the collector. Registration
   follows the runtime synchronization protocol (including stop-the-world
   when required) and publishes only after validation. Pointer-bearing image
   globals carry their exact type maps and are scanned at the mark-done barrier
   only for independently reachable owners. Reuse runtime module validation
   and type/itab initialization where it applies, but do not copy the plugin
   file loader or assume plugin registration is itself a JIT API.
9. **Keep unload conservative.** The runtime cannot discover arbitrary
   callbacks, goroutines, function values, or type/cache references unless the
   compiler gives them owner edges. General Eval exposes `Code.Close`, which
   drops its handle edge for the owner-aware subset; GC cleanup retires the
   module after all owner-carrying values disappear. The prototype has no
   compiler-certified scoped invocation. Raw-PC and foreign escapes require
   an explicit pin or conservative process pin. Never unmap code that a Go
   value, active frame, runtime cache, or external reference can still name.
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

#### Compiler-to-linker root and manifest protocol

Carry linker roots in the compiler object, not in a runtime-only metadata
string and not in a sidecar file. Extend the versioned `goobj` format with a
`JITScope` block and bump its magic/schema in lockstep with the compiler and
linker. Each Go package object that has at least one file importing `jit`
records the sorted unique resolved import paths visible in those files. The
runtime `BuildMetadata.Files` record remains the per-file source of aliases and
scope; the object block is only the linker root union. The object reader
validates the schema, bounds, duplicate paths, and package fingerprints before
the linker uses it. Older or mismatched object readers fail closed.

After all libraries are loaded and their imports resolved, but before
dead-code elimination, `cmd/link` collects these records. It verifies every
root path is present in the selected `LibraryByPkg` graph, then traverses
`Library.Imports` for the implementation dependency closure. For every
library in this set it marks all Go definitions reachable, including text,
data, package initialization records, type descriptors, method wrappers,
generic helpers, and private functions. Rooting the whole imported package is
the deliberate first contract: it avoids trying to predict which exported
function, interface method, or generic body an unknown future Eval statement
will use. Packages imported only by non-JIT files are not added unless they
are dependencies of a JIT-visible package. Duplicate symbols still use the
normal linker's resolution rules. If a required package is available only
through `-linkshared`, a plugin, or another dynamic Go module, the JIT-capable
link fails rather than claiming that its definitions are in the executable.

Once ordinary symbol resolution, ABI-wrapper selection, and dead-code
analysis finish, the linker emits one immutable `runtime.jitmanifest` table
from that exact retained set. Entries contain the canonical package path,
resolved linker symbol name, ABI/version, symbol kind, type identity when
applicable, and a normal relocation to the selected function entry, variable
storage, or static type descriptor. The table has a header with manifest
schema, object-format version, compiler/linker/runtime fingerprint, GOOS,
GOARCH, and enabled build modes. Link-time duplicates are collapsed only by
the linker's existing canonical symbol identity; ambiguous or missing
references are fatal link errors. The standard final relocation step supplies
ASLR-correct process addresses. At startup, the JIT validates the header and
indexes entries by full identity; it does not rediscover package archives,
inspect process symbol tables, or resolve names by `dlsym`.

The static type catalog is the type subset of this same table, including every
Go type descriptor retained from the JIT-visible package closure. Type hash
and display name are lookup accelerators only. The canonical identity record
and exact descriptor relocation are authoritative. This ensures the dynamic
type builder can reuse a host descriptor even when the host value carrying it
arrives through `any` or an internal reflection path. The table is ordinary
read-only link output containing relocations, not a copied package object
archive or a second compiled copy of package code.

The initial supported executable is a statically linked Go package graph on
Linux/amd64 using the matching internal linker. `-linkshared`, dynamically
loaded Go plugins as JIT symbol providers, and external Go package definitions
fail closed. A cgo boundary may be present only under the separate complete
effect-summary and explicit-pin rules; it does not add dynamically linked Go
symbols to the manifest. This mode boundary is part of the binary fingerprint
and is checked before Eval is enabled.

I am confident this is the right architecture for a self-contained
executable on a fixed Go toolchain and target. The current source implements
package roots, manifest generation, a supported lexical-capture subset, and
an owner-based module-retirement path. Complete runtime type/GC registration,
full owner propagation, and safe cache-aware unloading remain substantive
work. A production claim requires the completion gates above to pass on a
clean build; source inspection alone cannot establish that.

### Research findings from this checkout

These findings are from tracing the source at the current repository revision;
they describe concrete seams and missing contracts. They are not build/test
results.

| Area | Source evidence | Consequence for the design |
| --- | --- | --- |
| Reusable compiler | `gc.Compile(Request)` creates request-local target/frontend/type/linker objects. `gc/api_test.go` contains concurrent cross-target requests and checks legacy process state is unchanged. | The compiler context migration is a foundation already present in source. Verify the test and audit mutable caches; do not spend time removing immutable globals. |
| Source visibility | Go import scope is per file. Compiler metadata records imports per source file and permits unused imports only for files importing `jit`. | Direct `jit.Eval` calls carry the exact package/file identity; calls through a function value still use the `runtime.Caller` fallback and do not get lexical capture metadata. |
| Call-site identity | Direct `jit.Eval` calls are rewritten with compiler-emitted package, file, and capture descriptors. | The direct path no longer guesses from runtime filenames; indirect calls remain on the legacy fallback. |
| Export import | The in-process request accepts package export records and `noder` validates target header, export version, and pkgbits fingerprint before using the existing decoder. | This is the correct code-free package type-checking path. Build-time extraction may read the chosen archive; no archive belongs in the runtime snapshot. |
| Link root point | `cmd/link` runs `loadlib`, `inittasks`, then `deadcode`; the dead-code pass has an explicit initial-root phase. `sym.Library` retains `Imports`, and loader symbols carry package identity. | A compiler-produced root record can be consumed before dead-code elimination. The linker can traverse its already-resolved library graph and root symbols for only Eval-visible packages plus their implementation dependencies. |
| Host binding | `gc/jitmeta.go` writes a versioned JITScope block; the linker roots each Eval caller package and imported package dependency closure before dead-code elimination, then emits a relocation-backed manifest for selected reachable package symbols. Runtime lookup indexes package/name/ABI identities. | This replaces source-generated references as the sole retention mechanism and binds to linker-selected addresses. Exact structural ABI hashes and type aliases are present; broad symbol identity and package closure coverage remain incomplete. |
| Runtime module publication | `runtime.moduledata` carries text, function/PC tables, data/BSS ranges and GC masks, types, itabs, and module links. JIT registration carries compiler type metadata for pointerful writable globals, range-based type offset lookup, and an owner-conditional global scan. | Pointerful globals and dynamic type metadata use owner edges; on-demand type masks fail closed. The current change adds a scan at publication during concurrent mark to cover a newly published module missed by an earlier index shard. |
| Runtime type caches | Static `typelinksinit` and `moduleToTypelinks` remain static-module paths. Dynamic itabs use owner-local weak caches; reflect-created layouts use a process-wide weak-value structural cache with pointer-free keys; host type-assertion caches bypass JIT descriptors. | This avoids strong process-lifetime roots. Canonical identity for compiler-emitted unnamed descriptors across separate JIT modules is not implemented and remains a language-correctness gap. |
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
publication. It stores the generation, mapping address/length, metadata
allocation, weak-handle/pin-slot identifiers, and teardown state. It contains
no Go pointer to jitOwner or any object that points back to jitOwner. The
pointer-free runtime index stores numeric ranges and addresses to non-GC
moduledata and weak-handle records; it does not keep the owner alive. Thus
runtime code can continue to read mapped PC metadata while retirement is
pending without making the owner a GC root.

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
| Mutable JIT package state | Compiler object data symbols plus `AuxGotype`; runtime-created state uses typed heap allocation | Pointerful writable image globals carry their exact type descriptor and are scanned while their owner is marked. Runtime-created state uses typed heap objects reached from the owner. |
| Dynamic types and itabs | Compiler/runtime type builder | GC heap ABI cells with precise maps and owner/dependency fields. Name, text, and GC program payloads can live in the external mapping while the cell's owner is live. |
| Cleanup/control record | Runtime JIT loader before publication | The mapping's RW/NX control page contains a pointer-free `jitRetireRecord` with generation, metadata and mapping ranges, weak-handle/pin-slot identifiers, and sweep/retry state. A separate `specialJIT` fixalloc record stores only that control-page address. A lock-free retirement stack links control records; no record points back to `jitOwner` or its Go children. Reader counts live in the two-epoch index protocol. The whole reservation is unmapped last. |
| Type/reflection caches | Runtime and reflect dynamic-type paths | Owner-local weak itab entries and a process-wide weak-value cache for reflect-created structural types. Static host types retain existing cache paths. Cross-module canonicalization of compiler-emitted unnamed descriptors remains unfinished. |
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
| JIT-created local captured by an escaping closure | Escape analysis promotes it to typed heap storage. `mallocgc` associates allocations whose descriptor is JIT-backed or JIT-dependent in the per-span owner side table; the marker shades that edge while the object is live. |
| JIT-defined type or reflect.Type | Image descriptors remain in the module mapping and are retained when a typed pointer reaches them. Reflect-created descriptors are Go heap objects with side metadata that retains all source owners. Named Eval type identity is generation-scoped; unsupported identity cases fail closed. |
| Interface and itab | Scanned interface fields that point to image descriptors shade their owners. Dynamic itabs are scanned heap objects whose descriptor pointers retain the relevant owners, and they are cached weakly under an owner rather than in the process-wide strong table. |
| Pointer to a JIT-created object or backing store | Normal Go allocation layout remains unchanged. A pointer-free per-span slot records the additional owner/type edge; allocation fallback pins if that edge cannot be installed. |
| String or slice backed by image constants | A scanned string data pointer into the image retains its owner. Slice backing arrays allocated with JIT-dependent descriptors get a side-table edge. Any API that converts such a pointer into an untracked raw address pins. |
| Map or channel with JIT-defined types | No map/channel header ABI field is added. The live dynamic type descriptor, interface contents, typed backing allocations, and JIT closures carry the necessary edges; each path still requires runtime cache and GC-scan audit. |
| Host package function/global | Resolve through the linker-emitted manifest to the exact executable symbol or storage address. |
| JIT-to-JIT relocation | The source owner strongly retains the target owner. Relocated PCs alone are not GC dependencies. |
| Profile, trace, or panic symbol record | Store generation-relative CodeLoc and copied symbols/lines. Live CodePC/Func handles carry an owner. Legacy raw uintptr results pin the generation until process exit. |
| unsafe, uintptr, or foreign C reference | Reject from unloadable mode or pin until an explicit owner-aware deregistration proves the external reference is gone. |

Owner-local cycles are fine: a module owner may own type cells and state, and
those cells may point back to the owner. They become collectible when no
application root reaches the cycle. A process-global cache entry that points
strongly to any member is a real root and prevents cleanup.

#### Runtime type and cache design

Do not add unloadable JIT modules to ordinary `activeModules` root scanning.
At the GC mark-done ragged barrier, after ordinary roots reach a fixed point,
the runtime scans declared pointerful globals only for modules whose owners
were independently marked through ordinary Go references. Newly shaded heap
objects restart marking, so a newly discovered live owner gets its own global
scan before mark termination. This avoids both racing ordinary root marking
and turning a module's own state into a permanent owner root. Host globals
continue to resolve to their original linked addresses.

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
whose inputs are static descriptors may use the existing process-lifetime
caches. If an input descriptor is dynamic, reflection uses a process-wide
weak-value structural cache whose copied keys contain no descriptor pointers;
reflect-created descriptors retain their source owners through per-span
metadata. Dynamic itabs use owner-local weak entries. JIT dynamic descriptors
are kept out of `ptrMap`, `lookupCache`, `funcLookupCache`,
`structLookupCache`, `layoutCache`, `reflectOffs`, and the process-wide
`itabTable`. The implementation currently canonicalizes reflect-created
composites, but it does not canonicalize equal unnamed type descriptors
emitted by two separately loaded JIT modules. That leaves interface type
identity and `reflect.Type` equality across those modules unresolved; the
complete linker type-interning step below is still required.

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
Entries contain pointer-free addresses to runtime moduledata, mapped ranges,
and weak owner handles. They do not contain GC pointers to jitOwner.

Every lookup uses the two-epoch protocol in “Source-grounded design decisions.”
The immutable index snapshot is protected until a reader releases its lease.
The current runtime's `funcInfo` does not carry an owner; public interfaces
that retain its metadata install a process-lifetime module pin, while live
stack paths rely on the frame's owner edge. Retirement publishes a snapshot
without the range, flips epoch, waits for the old reader count, and only then
frees the snapshot and runtime tables. New lookups continue to see other
active JIT units.

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
   before exposing an entry point. The control record is independent of the
   owner allocation. The runtime index contains no GC pointers; its numeric
   fields refer to module metadata and weak owner handles.
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

The current implementation path has moved beyond direct unmapping from
`Code.Close`: an owner special queues an independent retirement record, and a
runtime worker unpublishes the module before unmapping after PC-index readers
drain. The present pass adds dynamic type/itab ownership, callback release,
cross-module weak-cache, structural ABI identity, and concurrent-mark
publication cases; the regression suites for these cases now pass. Generic-
dependent capture, full relocation semantics, and complete owner/cache
auditing remain unfinished.

## Current limits

The current implementation is a prototype. Runtime registration carries
function/PC/stack metadata in a separate generation index, weak owner handles,
preallocated raw-PC pin slots, a GC sweep special, and a retirement worker.
The capture ABI supports runtime variables, constants, and local aliases for
the compiler-supported subset. It passes addresses of the original cells and
keeps them reachable through the module owner. Generic locals whose concrete
runtime types can be reconstructed are supported; unnameable generic types and
function-local defined types are rejected when referenced. Unused unsupported
names do not prevent unrelated Eval. Runtime registration scans pointerful writable
globals when the object linker supplies an exact type and direct GC mask, and
on-demand type masks fail closed. Reflect-created type descriptors carry all
source module owners and resolve their local name/type/text offsets through
descriptor-local metadata. Their structural cache is process-wide with weak
values and pointer-free keys; dynamic itabs use owner-local weak values. Equal
unnamed descriptors emitted by separate JIT modules still are not interned to
a single Go type identity. The covered GC/cache fixtures pass; a complete
audit of runtime caches and type-dependent allocation paths remains open.
The linker roots the Eval caller package and imported package dependency
closure and emits a relocation-backed manifest; its symbol and relocation
coverage is a declared Linux/amd64 subset, not every Go object form.
`CompileSnippet` applies the saved language version, architecture features,
and checkptr level. Race, MSan, ASan, and coverage builds fail closed until
generated code can reproduce their instrumentation.

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
  declarations and package initialization are not supported. Direct Eval
  call-sites capture visible runtime variables, constants, and local aliases;
  representable generic values are supported, while unnameable generic types
  and function-local defined types fail closed when used.
  Calls through function values do not have the compiler call-site capture
  descriptor.
- The build metadata records package declarations, imports, and linker-visible
  package symbols. The compiler-generated Eval call carries runtime storage
  addresses for receivers, parameters, named results, locals, short-declared
  variables, loop/range variables, and type-switch/select variables in scope.
  Taking the address forces escaping cells into GC-managed storage; owner
  capture slots keep those cells and their pointer maps live. Constants and
  supported aliases are recreated as source declarations. Generic concrete
  types are reconstructed from runtime metadata where representable;
  function-local defined type identity and ambiguous generic type expressions
  are rejected. Remaining capture work includes complete lexical binding
  analysis, unsupported generic/type identity cases, and broader
  escape/closure conformance.
- The runtime receives each generated function's PC-to-file and PC-to-line
  tables and the corresponding compilation-unit file table. JIT descriptors
  are resolved through the separate module index; reflect-created descriptors
  and dynamic itabs use owner metadata and weak caches. These paths are
  implemented for the covered forms but still need broader cache and runtime
  caller audit.
- The runtime bypasses generationless type-assertion/interface-switch caches
  for dynamic descriptors. Dynamic `getitab` pairs use owner-local weak caches;
  reflect-created layouts use a process-wide weak structural cache. Traced
  pointers to descriptors shade associated owners. The cache/lifetime fixtures
  pass; all other runtime type-cache paths still require source audit.
- `CompileSnippet` gives its generated entry and function literals an owner
  reference, and the GC marks an owner when a typed heap pointer points into
  its JIT mapping. Pointerful writable globals in object images carry exact
  type maps and are scanned only when their owner is independently live. This
  does not cover every function-value representation or references encoded as
  integers. Unsupported paths are pinned or require an explicit `Code.Pin`
  handle.
- `Code.Close` drops the caller handle and returns; it does not synchronously
  unregister or unmap. For owner-aware modules, the GC sweep special queues a
  pointer-free record and the reaper waits for sweep completion and PC-index
  readers before freeing runtime tables and unmapping. Failed teardown remains
  charged and is retried. Several raw-PC, profile, and tracing compatibility
  paths conservatively pin modules for process lifetime. The configured
  budget includes image mappings, runtime tables, published index snapshots,
  and owner side tables; retained Go heap objects such as owner/capture slots
  are outside that byte count.
- This is not yet a general Go module loader. New package-level declarations,
  package initialization, arbitrary type descriptors, and every possible
  compiler relocation are not supported.
- Unloading is asynchronous and follows owner reachability only for the
  owner-aware subset. `Code.Close` rejects active calls and explicit pins, but
  it cannot make an unsupported code path owner-aware. Unsafe use, unresolved
  reflection/address escapes, raw-PC consumers, and unsupported dynamic type
  forms remain pinned or fail closed.

## Current verification snapshot (2026-10-20)

The recorded Linux/amd64 subset has now been built and exercised. A forced
`go run -a` completed and ran the integration program with `GOGC=1`. The
current JIT and transpiler suites also pass together with `-count=1`. Compiler
metadata, Go object, object-format, and short linker tests pass. The full
runtime package was attempted; its only reported failures were sandboxed
network-listener tests (`TestNetpollWaiters`, `TestEINTR`) and the `strace`
ptrace check (`TestUsingVDSO`). Focused runtime GC-map and atomic-alignment
tests pass. These results cover the supported subset and do not prove arbitrary
Eval safety or production unload correctness.

Use a GOPATH distinct from GOROOT so subprocess build output does not include
the GOROOT/GOPATH warning. The temporary GOPATH needs the toolchain's vendored
`x/tools` package mapped for runtime's `TestMkmalloc` subprocess:

```sh
JIT_TMP="$(mktemp -d)"
mkdir -p "$JIT_TMP/gopath/src/golang.org/x" "$JIT_TMP/tmp"
ln -s "$PWD/src/cmd/vendor/golang.org/x/tools" "$JIT_TMP/gopath/src/golang.org/x/tools"
GOROOT="$PWD" GOPATH="$JIT_TMP/gopath" TMPDIR="$JIT_TMP/tmp" GOFLAGS=-buildvcs=false GO111MODULE=off GODEBUG=panicnil=0 bin/go test -count=1 -timeout=5m ./src/jit ./misc/jit-compile
GOROOT="$PWD" GOPATH="$JIT_TMP/gopath" TMPDIR="$JIT_TMP/tmp" GOFLAGS=-buildvcs=false GO111MODULE=off GOGC=1 bin/go run -a ./misc/jit-compile
GOROOT="$PWD" GOPATH="$JIT_TMP/gopath" TMPDIR="$JIT_TMP/tmp" GOFLAGS=-buildvcs=false GO111MODULE=off bin/go test -a -timeout=5m ./src/cmd/compile/publicinternal/gc ./src/cmd/publicinternal/goobj ./src/cmd/publicinternal/obj
GOROOT="$PWD" GOPATH="$JIT_TMP/gopath" TMPDIR="$JIT_TMP/tmp" GOFLAGS=-buildvcs=false GO111MODULE=off bin/go test -short -count=1 -timeout=5m ./src/cmd/link/publicinternal/ld
GOROOT="$PWD" GOPATH="$JIT_TMP/gopath" TMPDIR="$JIT_TMP/tmp" GOFLAGS=-buildvcs=false GO111MODULE=off GODEBUG=panicnil=0 bin/go test -a -run '^(TestAtomicAlignment|TestGCInfo)$' -timeout=5m ./src/runtime
```

The JIT package suite checks PC publication, `CodePC` retention, mapped-pointer
ownership, supported-module retirement after forced GC, object relocation
coverage, and close/run behavior. The integration suite checks repeated Eval,
same-package and imported `jitcounter` storage, `fmt` calls, stack growth with
live pointer-bearing locals, explicit handle release, escaped closures,
finalizer/cleanup callback roots, and retirement. Linker coverage includes a
`go:linkname` public symbol whose object owner package differs from its source
identity (`runtime/debug.SetTraceback`), plus structural hashes for builtin
type imports. The linker manifest now selects the longest included
package-qualified linker name, preserving aliases generically. Legacy
`runtime.FuncForPC` lookup pins its module. Neither these fixtures nor a
successful low-GC run prove arbitrary Go Eval or production unload safety.

Generic lexical capture coverage also exercises runtime reconstruction for a
concrete scalar, imported `bytes.Buffer` from a different source file, a map,
an anonymous tagged struct, an anonymous interface, `Box[int]`, and
`Box[*bytes.Buffer]`. Function-local defined types remain explicitly rejected.

An extra final forced `go test -a` of both integration packages could not start
because the temporary filesystem ran out of space while linking. Its scratch
build directories were cleared, and the same current suites passed without
forcing redundant rebuilds; the separate low-GC `go run -a` completed from a
clean rebuild.

Compiler-generated linker coverage tests in
`src/jit/linker_coverage_test.go` passed as part of `go test -a ./src/jit`.
This table lists exercised fixtures, not all language/runtime behavior:

| Area | Cases exercised | Coverage |
| --- | --- | --- |
| Local code and data | Direct helper calls, package globals, strings, slices, closures, generics | Compiler relocations and local symbol closure |
| Runtime operations | Map lookup, mutation, delete, clear, iteration and pointer writes; interface assertions, type switches, method dispatch and method values; channels, select, goroutines, defer, panic, string concatenation and byte conversion | Runtime imports, function metadata, stack/GC maps, closure code, and write barriers |
| Local code and closures | Capturing and recursive closures, function values, method values, generic instantiation, named results and deferred calls | Reachable local code and closure environment references |
| Relocation handling | Absolute addresses, PC-relative data, direct calls, addroffsets, method offsets, weak references, indirect-call markers and GOT slots | Compiler object to image relocation mapping; an explicit supported/rejected relocation table guards the Linux/amd64 contract |
| GOT | Compiler object relocation fixture plus mapped GOT pointer slot | GOTPCREL displacement and pointer initialization |
| Host package reuse | `fmt` and shared `jitcounter` package state | `misc/jit-compile` integration test |
| Lexical capture types | Concrete generic values, imported named types added from a different file's export metadata, maps, anonymous structs with tags, anonymous interfaces, instantiated named generics with nested imported arguments; function-local named type rejection | Runtime `reflect.Type` to source reconstruction; unsupported identities fail closed |
| Runtime type identity | Builtin/composite runtime types, reflected composites, local interface types, and cross-module itabs | Owner-aware descriptors and weak dynamic caches; these covered fixtures pass, with broader canonicalization still open |
| Close lifecycle | Active-run guard, rejection of runs after close, idempotent close | Prevents unmapping during a tracked call; escaped goroutines and callbacks remain the caller's responsibility |

This matrix is not a claim that every relocation emitted by every Go
architecture, build mode, or assembly source is supported. The linker rejects
forms outside its declared Linux/amd64 Go-object subset.

## Next implementation gates

1. **Compiler/link context isolation: implemented and exercised.** Each
   `gc.Compile` request has fresh target/frontend/link state. The concurrent
   cross-architecture compiler test passes. Continue auditing any process
   state outside the request API and make build flags/features part of the
   request snapshot.
2. **Code-free export snapshot: implemented for the selected package closure.**
   The importer receives export records, not `.a` archives. Keep auditing the
   closure and package fingerprints; make the build-selected language version,
   architecture feature flags, sanitizer, race, and coverage settings part of
   the snapshot.
3. **Linker roots and host manifest: implemented for a tested subset.** The
   normal linker roots JIT-visible package closures and emits a relocation-
   backed symbol manifest. Tests prove `fmt`, `jitcounter`, and same-package
   package storage reuse. Finish generic dictionaries, wrappers, private
   helpers, exact type identity, and broad package closure coverage.
4. **Runtime Eval scope: implemented for package scope and supported lexical
   captures.** Direct calls carry package/file identity; parsing, type
   checking, compilation, linking, and execution happen at runtime without a
   package or import clause in the snippet. Supported variables, constants,
   and aliases retain or recreate their caller meaning; representable concrete
   generic types work, while unnameable generic and function-local defined
   types fail closed.
5. **Linux/amd64 object linker and module registration: partial.** Supported
   relocation cases, stack metadata, stack growth, panic paths, and explicit
   close behavior have fixtures. JIT runtime tables use exact funcdata ranges
   and sorted immutable indexes. Pointerful writable globals use compiler type
   maps. Dynamic interface pairs use owner-local weak caches; reflect-created
   composites use a process-wide weak structural cache and avoid generationless
   host cache insertion. Compiler-emitted unnamed descriptors are not yet
   canonicalized across JIT modules. Covered cache fixtures pass. Finish type
   identity, package closure, and emitted relocation coverage before broad
   support.
6. **GC lifetime: partial.** Owner edges cover entry frames, source function
   literals, typed pointers into mappings, dynamic type/itab values, reflected
   type descriptors, and pointerful package globals. The runtime queues
   retirement after sweep and drains index readers before unmapping. Lexical
   capture covers a supported subset; unrepresentable generic types, unsafe/foreign
   escapes, all runtime object layouts, and a source-complete cache audit remain
   open.
   Unsupported paths must continue to pin or fail closed.
7. **Conformance and portability: subset verified; production gates remain.**
   The current Linux/amd64 tests and low-GC run pass for the declared subset;
   the full runtime suite is blocked only by sandbox network and ptrace tests.
   JIT execution remains Linux/amd64 only; arm64 needs a separate backend and
   execution evidence. Broad linker, GC/cache, and semantic coverage is still
   incomplete.

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
- the owner argument and stack-map/defer roots in active JIT Eval frames;
- JIT-created type and itab cells, and values whose layout or method code
  depends on a JIT type;
- JIT-created heap objects and slice backing arrays whose per-span side-table
  entries carry the GC-traced owner/type edge; scanned pointers into a JIT
  mapping also shade that mapping's owner;
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

Go interfaces point at runtime type or itab records. JIT-defined descriptors
remain in the mapped module; the GC's scanned pointer path must shade the
module owner whenever an interface or itab points to one. Reflect-created
descriptors are heap objects with per-span metadata edges to all dependencies.
Keep ABI record layout, method offsets, name offsets, equality functions, and
GC bitmaps exact. A runtime equality function that points into JIT text must
remain covered by the descriptor's owner metadata.

Use `(callSiteID, evalGeneration, declarationOrdinal)` for named Eval types.
Structurally identical unnamed types share Go identity through the weak-value
canonical type table specified below. Reuse the exact host descriptor when a
dynamic type is identical to a static host type.

All JIT-associated heap allocations retain their ordinary Go object layout.
The per-span side table stores the extra owner or descriptor edge. Slice
backing storage, map/channel contents, dynamic descriptors, finalizers, and
zero-sized escaping values must follow ordinary Go allocation semantics plus
that side-table edge, or fail closed if a safe edge cannot be installed.

Mutable state created by Eval is an ordinary typed owner-held object. The
runtime scans declared JIT data/BSS roots only when ordinary roots already
mark that module's owner, so module state cannot make its owner reachable from
the registry itself. Existing host globals remain direct manifest
references. Pointer-free code, constant tables, names, file/line
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
chain. Entries contain pointer-free addresses to runtime moduledata, mapped
ranges, and weak owner handles; they never contain GC pointers to jitOwner.

Every lookup uses the selected two-epoch protocol: readers announce before
loading an immutable snapshot and release their lease after lookup. The
current runtime's `funcInfo` does not carry an owner; interfaces that retain
its metadata install a process-lifetime pin, while active stack paths rely on
the executing frame's owner edge. Retirement removes the range from the next
snapshot and waits for old readers before freeing control data. Other live
JIT modules remain visible during that grace period.

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

The API should make the unsafe action explicit. The prototype implements
`Code.PC()` returning an owner-bearing `CodePC`, `CodePC.Address()` for
short-lived address access, `Code.Pin()` returning `*Pin`, `Pin.PC()` returning
its single entry address, `Pin.Release()` as an idempotent operation, and
`Code.ActivePins()` for diagnostics. Keep the `CodePC` value alive while using
its address; a copied integer is a raw PC and requires a `Pin`. `Pin.PC` is
valid only while that pin remains active. The current source keeps `Code`
handles in a process-global pin-root map keyed by pin ID;
the runtime also preallocates a strong owner slot for APIs that expose raw PCs
without an owner-bearing result. A foreign registration helper is not
implemented; it should pair registration with a pin and require deregistration
before release. Releasing a pin does not claim that arbitrary C code, an
integer, or an unsafe memory location has stopped using the address; that is
the caller's explicit precondition. If the caller cannot prove quiescence, it
must keep the pin. Release drops the manual root; it does not promise immediate
unmapping if ordinary owner-bearing Go values still exist or collection and
reaping have not completed. The retirement hook is installed before
publication; if that setup fails, compilation fails before any address is
exposed.

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

This uses two narrow runtime hooks: the marker treats a precise pointer into a
published JIT mapping as an edge to its owner, and the sweep special queues an
already-dead owner's external resource record for a separate reaper. The GC
does not scan the image bytes or run a second graph pass. Preserve the
independent-record rule so target sweep and resource teardown can race safely.

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
   The frame contains every visible binding, not only names found by a build-
   time scan of the eventual source string. Each entry carries a call-site-local
   binding ID, source name, binding kind, exact type identity, and either a
   typed storage address, a constant value, or a type descriptor. The ID is
   derived from compiler object identity (source file/declaration position and
   enclosing function or generic instantiation) and a frame slot; it is not the
   variable name. At one call site, ordinary Go lookup yields at most one
   visible binding for a spelling: an outer binding hidden by a nearer binding
   is not addressable by that spelling. The runtime parser resolves free names
   against this call-site environment after parsing; declarations inside the
   snippet shadow captures according to Go scope rules. The implementation may
   omit unreferenced value addresses from the generated function's active
   frame only after runtime parsing has resolved the source.
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
6. Lexical Eval is defined for a compiler-recognized direct call to
   `jit.Eval(source)`. A call through a function value uses only the invoking
   file's package/import context and package declarations; it does not inherit
   the invoking function's local variables. A wrapper containing a direct
   `jit.Eval` call captures the wrapper's own call-site environment, not the
   wrapper's caller's locals. Forwarding a caller's lexical environment through
   a helper is outside this API contract; the caller must evaluate at its own
   direct call site. No path guesses local variables from the runtime stack.
   `Eval`'s ordinary Go function body remains the package-scope fallback for
   older objects.
7. The source is a statement list executed once as a void function body.
   `Eval` returns its `Code` lifetime handle, not the snippet's completion
   value. A bare `return` exits the snippet; a valued return is a type error.
   New `var`, `const`, and local type declarations belong to that invocation
   and do not add bindings to the caller or to later Eval calls. Assignments
   to captured variables update the original cells. `defer`, panic/recover,
   and loop control follow ordinary function-body boundaries. Values are
   communicated through captured assignments or owner-bearing values stored
   into host state; expression Eval and persistent declaration scope require a
   separate API and are not implied by this contract.
8. Type spelling is diagnostic/source information, never type identity. Each
   capture refers to a compiler type-graph entry and the exact linker-selected
   descriptor when one exists. Package and function-local defined types use
   the descriptor emitted for the original declaration; Eval must not recreate
   a new named type from printed source. Type aliases retain the identity of
   their target. A generic call-site descriptor carries both the declared
   type-parameter constraint and its active type arguments: runtime checking
   uses the declared constraint, and the JIT compiler instantiates the
   resulting function for that call site's concrete arguments. Anonymous types
   use the canonical structural type protocol below. If the compiler cannot
   serialize a binding's type graph or resolve its descriptor, a snippet that
   references that binding fails before publication; unrelated snippets remain
   usable. For a captured interface, the normal link also emits a closed-world
   target catalog for every concrete type in the executable that implements
   that interface, with the exact type descriptor, itab requirements, method
   symbols, and wrappers rooted. The JIT resolves the runtime concrete type
   through this catalog before interface dispatch. A missing target fails
   before publication; an opaque or forged dynamic type is `Unknown` and is
   rejected by Auto. Manual pinning cannot restore host method code that the
   normal linker discarded. To preserve those methods, the compiler places
   capture-interface type IDs in the versioned JIT scope record. `cmd/link`
   resolves them before dead-code elimination and seeds the existing interface-
   method reachability fixpoint (the mechanism in
   `src/cmd/link/publicinternal/ld/deadcode.go`) with their method signatures.
   The fixpoint retains each matching method implementation and its type/itab
   dependencies for every reachable concrete host type, including types from
   packages outside the Eval file's import closure. The final manifest includes
   these additional retained symbols. Runtime-created JIT types contribute
   owner-scoped type/method metadata; external dynamic modules and forged
   unsafe interface values remain outside Auto.
9. `Auto` is the default and rejects effects it cannot track. `Manual` is an
   explicit whole-LinkUnit pin selected before the first JIT instruction can
   run. A caller using the split Compile/Load/Run API may acquire `Pin` after
   Load but before Run. Because `Eval` runs immediately, it needs an explicit
   manual-mode option if the caller wants to permit an otherwise unknown raw
   address escape. A pin released after execution is safe only after every
   foreign registration and copied address has been removed and quiesced;
   release is never inferred from garbage collection. Pinning protects code
   and metadata lifetime, not arbitrary unsafe memory behavior.

#### Status of the Eval-scope decisions

These choices close the design questions about environment capture; they do
not claim that the current code implements the full ABI.

| Topic | Selected contract | Current implementation boundary |
| --- | --- | --- |
| Which values are supplied? | Every binding visible at a direct call site is supplied before the runtime source is known. The runtime parser then selects the free names actually referenced. | `jitEvalCaptures` in `src/cmd/compile/publicinternal/noder/writer.go` enumerates visible bindings; `src/jit/snippet.go` currently indexes them by source name. |
| What is a binding? | A source name plus a call-site-local binding ID and slot, kind, exact type identity, and typed address/value/descriptor. Name is a lookup spelling, not identity. | No binding ID is serialized yet; same-spelling hidden outer bindings are correctly unavailable under ordinary lexical lookup. |
| What scope does it see? | A direct call sees its own lexical scope. A wrapper sees its own direct call site. A function-value call sees package/file scope only. | `src/jit/eval.go` has direct-call and package-scope entrypoints; the fallback does not capture locals. |
| What happens to declarations/results? | The body runs once as a void function body. New local declarations do not persist; captured assignments are visible to the caller. The returned `Code` is a lifetime handle, not an Eval result value. | This is the current API shape; expression results and persistent Eval declarations are outside this API. |
| How are types identified? | Compiler type-graph and descriptor IDs are authoritative, including local named types and generic constraints/instantiations. Captured interfaces also root every concrete implementor, its descriptor, itab requirements, method symbols, and wrappers in the closed executable through the linker's interface-method fixpoint. Printed type syntax is never an identity key. | `src/jit/capture.go` records a runtime `reflect.Type`, while `src/jit/capture_type.go` reconstructs source syntax; `writer.go` rejects some local/type-parameter cases. Capture type roots and the interface-method seed are not yet in JIT scope metadata. |
| How are unknown effects handled? | Per-symbol summaries are transitive. `Unknown` rejects Auto; Manual pinning is explicit and protects mapping lifetime only. Scoped may fall back to Auto only for Auto-safe effects. | The source metadata has no emitted per-symbol effect records yet; summary emission, call-graph closure, and runtime enforcement remain unimplemented. |

The implementation sequence below is therefore work to realize a decided
contract, not a request to rediscover these semantics. The safety proof and
supported-object/runtime coverage remain implementation gates; the source
audit is not a proof of the entire external Go ecosystem.

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
snapshot publishes both indexes using pointer-free addresses for runtime-owned
metadata and weak-handle tokens for owners; GC does not scan the index.
Every range has a 64-bit generation and cannot be reused until that generation
has retired.

Each published LinkUnit has a private synthetic `moduledata` view in separately
accounted runtime system memory, populated only with the fields consumed by
supported function lookup, traceback, PC-value, stack-map, and unwind paths.
The JIT index points to this view and records the module generation; it is
never linked through `moduledata.next`,
`lastmoduledatap`, or `activeModules`. Pointerful globals, init tasks, and
typelinks remain absent. Before publication, the implementation builds a
field-use matrix for every runtime consumer and rejects a unit if any required
field or table is missing. Any consumer that requires global module-chain
membership is changed to use the JIT index or is rejected for Linux/amd64.

Use a two-epoch reader protocol, not a mutex, STW-only update, per-M lock, or
unspecified hazard-pointer scheme. The current implementation encodes the
lease marker and epoch in the low two bits of the page-aligned snapshot
pointer, so the lookup path can return one opaque `uintptr` token without an
allocation. The reader is allocation-free and `nosplit`, using only
lock-free atomics:

1. Read the current epoch with sequentially consistent ordering.
2. Increment that epoch's reader count with a checked CAS. Saturation is a
   runtime fatal error; counters never wrap.
3. Re-read the epoch. If it changed, decrement and retry before loading the
   root. This closes the race where a reader is interrupted between seeing an
   epoch and announcing itself.
4. Load the immutable root and find the PC or metadata range. Readers that
   return a long-lived result must transfer it to an owner-bearing handle
   before decrementing the reader count. Signal-time and stack-local readers
   may return raw metadata only when an active-frame or prior-pin witness is
   present; those results must not outlive that witness.
5. A miss returns the ordinary static result or invalid result. A lookup on an
   old snapshot finishes normally; its epoch announcement keeps that snapshot
   and its records alive.

One serialized publisher builds a replacement snapshot, publishes the root,
flips the epoch, and waits for the old epoch count to reach zero before
freeing the old snapshot or any control record reachable only from it. The
writer lock remains held through that grace period, so the epoch cannot flip
twice while an old reader is outstanding. New readers continue on the new
snapshot while old readers finish. Removing a range precedes freeing its
metadata or executable pages.

The runtime's existing `funcInfo` has no owner field. General public PC
surfaces that can retain `_func` or `moduledata` pin the module before lookup;
stack-local paths retain an active-frame witness, and signal-time lookup uses
only a bounded lease while the stack is stopped. This is a compatibility
fallback, not the intended owner-bearing `funcInfo` design. Runtime callers
must not let raw module metadata outlive the corresponding pin or active-frame
witness. Unlisted `linkname` access to `findfunc` or its result layout is
unsupported in this hard fork.

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

The implementation decision is to keep executable mappings outside the Go
heap and preserve every user object's ordinary layout and type identity. A
pointer-free side table is attached to an `mspan` only when a heap object
needs a JIT lifetime edge. The word for an object slot is tagged as one of:
the direct module owner, a dynamic type descriptor, or a heap metadata object
that can retain several owners and reflection offsets. The side table is
charged to the JIT budget, cleared when a slot is reused, and released when
its span is freed. If the runtime cannot install an edge, it pins the
referenced module or descriptor rather than exposing an untracked reference.

`mallocgc` associates allocations whose type descriptor is in a JIT image or
depends on one. The ordinary marker shades the side-table edge before scanning
the object's fields, including noscan objects. The GreenTea inline marker and
span scanner do the same for objects they mark directly. Independently,
scanned pointer slots that point into a JIT image shade that image's owner;
this covers interface type/itab pointers, string data, and other typed
references. Pointerful writable JIT globals are scanned only while their
owner is already live. Reflect-created descriptors carry side metadata with
all source owners and descriptor-local reflection offsets. Finalizer and
cleanup root scanners also retain JIT callback PCs stored as raw code words.

This design adds no JIT span classes, object headers, map/channel ABI fields,
or `abi.Type.JITOwner` field. Those are superseded alternatives. Exact user
type bitmaps continue to scan the object; the side-table edge supplies only
the missing code/type lifetime. Function literals explicitly capture their
module owner, and active Eval entry frames receive it as an argument. The
static linker, reflection, and runtime paths that return an unowned raw
address pin the corresponding module.

The source audit has covered the in-tree marking, allocation/reuse, cleanup,
index-reader, raw-PC, and cache paths listed above. Remaining work is to turn
that inspection into a complete source-to-invariant proof and close the actual
implementation gaps: compiler-emitted unnamed type interning across modules,
all supported relocation and host-symbol forms, generic captures whose runtime
type names cannot be reconstructed safely, function-local defined types, and
complete effect summaries. The runtime source
inspection does not certify arbitrary `linkname`, unsafe, or foreign use.
Unsupported allocation, type, or relocation forms must continue to fail
closed.

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
remains RW/NX and contains a `jitRetireRecord`. The record contains its
generation, mapping bounds, GC descriptor bounds, and state, but no Go pointer
to `jitOwner` or its children. A separate `specialJIT` fixalloc record stores
the control-page address; `freeSpecial` sees it only after the owner is
unreachable and the special has been unlinked. It then pushes the record onto
a lock-free JIT retirement stack without allocating or unmapping. A single
reaper G drains the stack. There is no queue lock and the reaper does not reuse
a retirement record until it has completed or requeued that record. The
runtime change must audit index-writer lock ordering and the sweep call
context; queue insertion must remain bounded and must not allocate, invoke
user code, or block on a condition that requires the reaper.

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
never allocates. The reaper never reuses a record until that record completes
or is requeued.
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
| Optimized statically linked Go binary using the matching internal linker and compiler/runtime/linker fingerprint | Supported. It embeds compiler/link metadata, not another Go installation or package archives. |
| `-linkshared`, dynamically loaded Go plugins as JIT symbol providers, external Go package definitions | Fail closed; their symbol/type identities are not in the executable's relocation-backed manifest. |
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
The normal build emits a versioned effect record for each host-manifest symbol
and computes transitive summaries over the resolved call graph, including
strongly connected components. Interface and indirect calls use the union of
all possible targets in the executable's Eval-visible closure. Assembly,
cgo, linkname, runtime entry points without a verified summary, and unresolved
indirect targets are `Unknown` unless they have an explicit reviewed summary.
`Auto` rejects an unknown or ownerless escape before publication. `Manual`
may pin the complete Eval image when the remaining risk is an untracked
code/metadata address; it does not legalize invalid Go pointers, memory
corruption, or arbitrary foreign heap access. `Scoped` is selected only with a
complete certificate. Failure to prove `Scoped` may retry as `Auto` only when
the effect record is Auto-safe; it never converts `Unknown` into permission to
run.

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
in this workspace. The source investigation was followed by the build and
fixture results recorded above; those results do not close every audit item.

| Question | What the checked-in source says | Design answer |
| --- | --- | --- |
| Why does the runtime need PCs and module metadata? | `runtime.findfunc` calls `findmoduledatap`, which walks the static `moduledata.next` chain. The result drives PC-to-function lookup, stack maps, traceback, panic/defer, preemption, profiling, and symbolization. `funcInfo` currently contains only `*_func` and `*moduledata`; `FuncForPC` can return a pointer into that metadata. | Keep JIT ranges in a separate immutable generation index. Extend internal `funcInfo` and exported wrappers with an owner or copied metadata. Inventory and convert every `findfunc` reader; STW by itself cannot protect readers such as signal-time profiling. |
| What is a PC lifetime problem? | A PC is an address, often stored as `uintptr`; it has no GC edge and no observable last-use. Public raw-PC surfaces include `Caller`, `Callers`, `CallersFrames.Frame.PC`, `Func.Entry`, function-valued `reflect.Value.Pointer`/`UnsafePointer`, and profile/stack records such as `MemProfileRecord`, `BlockProfileRecord`, `MutexProfileRecord`, and `StackRecord`. | New APIs return owner-bearing `CodePC`/function handles. Compatibility APIs that return JIT PCs install a permanent generation pin. Internal profiles/traces use generation-relative `CodeLoc` plus copied strings so sampling does not pin code forever. |
| Can `AddCleanup` be the destructor? | `runtime.AddCleanup` is asynchronous, arbitrarily delayed, not guaranteed before process exit, can be blocked by user cleanup, and can be prevented by references from its callback/argument. It runs through shared cleanup machinery. | No. Unreachable means eligible for retirement, not already unmapped. Use a dedicated runtime special and JIT queue. If no GC occurs, or at process exit before retirement, the external mapping remains mapped; it is not marked free. A completed GC plus JIT drain can reclaim eligible units. |
| Do interface/type/reflection caches release dynamic metadata? | `runtime.itabTable` allocates itabs with `persistentalloc`; method slots are raw PCs. ABI type-assert and interface-switch caches store type/itab addresses as `uintptr`. `reflect.ptrMap`, `lookupCache`, `funcLookupCache`, `structLookupCache`, and `layoutCache` are global strong caches; the StructOf comment explicitly says it pins memory. | Use the static type catalog plus the canonical identity protocol below. Dynamic types and itabs bypass static global paths or use owner-local cells. The reflect-created type canonicalizer uses copied structural keys and weak values. Compiler-emitted unnamed JIT descriptors still need a linker/runtime interning protocol with owner dependencies. Add owner-aware type/name-offset resolution and never add JIT entries to module typelinks, `reflectOffs`, or the static module chain. |
| Can the current mapper participate in `GOMEMLIMIT`? | The current build and image tests exercise `runtime.JITMapMemory`/`JITMapping`: it charges the full mapping to `memstats.other_sys` and `gcController.mappedReady`, and only debits after checked `munmap`. Failure injection and budget pressure are not covered. | Keep the runtime token, add budget reservation and a reclaim-before-limit path, and retain a charged tombstone/control record after any failed teardown. Accounting alone does not guarantee reclamation or establish owner lifetime. |
| What do `unsafe` and C references mean for ownership? | GC traces typed Go pointer fields, not integers containing addresses. A `runtime/cgo.Handle` roots the Go value stored in the handle table, but does not make an arbitrary code address safe. | Auto rejects unknown address storage or foreign retention. A Go callback passed through a rooted handle keeps its owner-bearing function value alive. A raw JIT PC stored by C or in an integer needs `Pin` until the foreign registration and all copied addresses are removed and quiesced. `runtime.Pinner` pins Go heap objects, not executable mappings. |
| Which package/import context does Eval receive? | Go imports are file-scoped. The package path is package-wide, but an alias imported in a different source file is not in scope at this call site. | Parse a statement/function body with the containing package path, the imports from the file that contains the Eval call, package declarations and type information, and explicit capture descriptors. The snippet has no `package` or `import` clause. Resolve names against the existing executable's host symbols; do not rebuild/import package archives at runtime. |

The consequences are concrete: ownership is a traced Go object graph, while
the executable image and its runtime metadata are separately accounted
resources. Per-span side-table edges add owner reachability without changing
user object bytes or type identity. Collection decides when an owner is
unreachable; a JIT-specific runtime hook queues retirement; the sweep barrier
and PC-index grace period protect runtime readers; and only then can the
runtime unmap and debit the reservation. A leaked or pinned owner remains
real mapped memory and remains charged. It is never silently treated as free
memory.

For the regex object, retain one owner-bearing `jit.Code` from the Regex
object. This is the **Owned** fast path: no source captures or callbacks are
allowed, and dropping the Regex's last reference makes its owner eligible for
collection once in-flight calls finish. It still uses the ordinary owner
edge and sweep retirement path. `Scoped` is a different specialization for a
single synchronous invocation that proves no value, type, string, slice,
callback, or PC escapes; it can retire after the call, defers, and reader grace
period without waiting for GC. Neither mode replaces general `Auto` Eval.

The architecture questions above have source-backed answers, but production
safety is still conditional on closing the implementation gaps and completing
the source-to-invariant proof. The source paths inspected are recorded above;
the current tests exercise side-table reuse, owner shading, finalizer/cleanup
roots, retirement, reflection/cache behavior, and raw-PC pinning for selected
cases. Generation-aware sweep interleavings, every `findfunc` consumer, all
runtime caches, and checked mapping failure/accounting still need broader
coverage and review. A fixture matrix is evidence against known counterexamples,
not proof of completeness.

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

1. **Implement the Eval contract above.** Direct calls carry the complete
   lexical environment; indirect calls are package-scope only. Preserve
   statement-body completion semantics, repeated-evaluation identity,
   unit-level retention, and Auto/Manual/Scoped plus Release/Pin behavior. Do
   not silently use runtime stack inspection for missing captures.
2. **Complete package, type, effect metadata, and normal-link roots.** Keep
   export data and runtime type-check metadata separate from object code. Root Eval-visible
   packages and captured interface method signatures before dead-code elimination. Emit a relocation-backed host
   symbol manifest and a static type identity catalog from the normal linker's
   selected symbols and complete Eval-visible type closure, including the
   closed-world concrete implementor/method catalog for captured interfaces.
   Serialize compiler type graphs, type-parameter constraints/arguments, and
   per-symbol effect summaries; printed type names are not identities.
3. **Complete compiler call-site descriptors and capture ABI.** Remove runtime
   path guessing. Pass package/import/type scope and a complete set of typed
   binding slots with binding IDs. Promote every visible local dynamic source
   can name. Preserve variable identity, writes, loops, and closure aliasing
   after the caller returns. Resolve source identifiers to slots at runtime.
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
6. **Complete owner edges.** Keep owners in generated function environments
   and Eval frames; use the per-span owner side table for JIT-dependent heap
   allocations and reflected descriptors; use traced pointers into images to
   shade their owners; and retain callback PCs held in runtime finalizer and
   cleanup records. Keep user object layouts exact. Audit barriers,
   checkmark, GreenTea scanning, allocator reuse, maps/channels, and every
   runtime allocation path against this implementation rather than adding
   custom headers or span classes.
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
   escape summaries and manifest effect records; add callback-retention and
   raw-storage effects those summaries omit. Verify the compiled object. A
   failed Scoped certificate falls back to Auto only when Auto accepts every
   reachable effect. Unknown or ownerless escapes remain rejected unless the
   caller explicitly chose Manual pinning.
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
- The no-escape path is selected only with a complete certificate; a failed
  certificate falls back to Auto only when all effects are Auto-safe, and a
  crossing panic pins its owner.
- Unsupported OS/runtime modes fail closed. No partial module is executable
  or visible after an error.

The current tree has an incomplete host-symbol manifest and Linux/amd64
relocation coverage, not a complete Go linker. The source includes lexical
capture for a supported subset, mapped-address owner marking, weak-owner
lookup, an owner-local weak dynamic itab cache, a global weak structural cache
for reflect-created types with byte-threshold pruning and backing-map rebuild,
descriptor-local reflection offsets, bypasses for
generationless host type-assertion caches, a separate generation index,
sweep-triggered retirement, a reaper, raw-PC pin slots, byte accounting, and
typed GC roots for pointerful writable image globals. Equal unnamed type
descriptors emitted by separate JIT modules still have no canonicalization
protocol. The latest callback, cross-module itab, structural ABI identity,
and concurrent-publication changes pass their focused JIT and integration
fixtures. Generic capture reconstruction remains bounded as described above;
complete object relocation semantics, all runtime cache paths, and a
source-to-invariant safety argument remain unfinished. The
explicit Pin API keeps its owner rooted until `Pin.Release`; legacy raw-PC APIs
pin for process life. Runtime module registration does not append JIT modules
to the static module chain. Linux/amd64 remains the only executable target.
The implementation is not production-ready until those contracts and runtime
invariants are completed and reviewed.
