# Single-Binary Go Toolchain: Production Design and Migration Plan

## Purpose

This document defines the desired end state and the migration needed to reach it. It is a design decision, not a prototype specification. The objective is a production Go distribution in which one host executable contains the Go command, compiler, assembler, linker, standard library build inputs, and the other Go-owned tools, and can use them without a separately installed Go tree. The compiler and linker must also be callable in-process through supported Go APIs.

The executable is host-specific. “One binary” means one executable for a given host OS and architecture, not one machine-code executable that runs natively on every host. A release artifact embeds the complete Go-owned toolchain and source inputs for that Go release. It does not embed every possible C compiler, SDK, system library, kernel interface, or third-party module a user's programs may need. Those are external inputs and must be detected and reported explicitly.

## Decisions

These decisions are the basis for implementation; they are not open design questions.

1. **No mandatory `GOROOT`.** The embedded distribution is the default and works when `GOROOT` is unset and no other Go installation exists. An explicit `GOROOT` continues to select an installed tree for compatibility and development. The bundled installation must not silently fall back to another Go found on disk or on `PATH`.
2. **One executable per host, all compiler-supported targets.** A release executable includes the Go compiler, assembler, linker, and Go-owned support data for every target supported by that release's toolchain, subject to the release's documented support matrix. Host-only operations such as cgo and external linking retain host prerequisites. The product will not claim that bundled cross-compilation removes a target SDK or C toolchain requirement.
3. **Bundle source inputs, not prebuilt standard libraries for every target.** Include the exact standard-library and tool source/build inputs needed for builds. Build standard-library packages for the selected target using the normal cache. This avoids a combinatorial set of precompiled libraries and keeps build tags and target configuration correct.
4. **Use a virtual, read-only toolchain filesystem first.** Go-owned source, templates, headers, and static data are addressed by logical path through an injected filesystem interface. Materialize an individual resource in a private content-addressed cache only when an OS interface or external program requires a real path. Do not unpack a complete GOROOT at startup.
5. **Reusable APIs are the implementation; commands are adapters.** `go`, `compile`, `asm`, `link`, `vet`, and other Go-owned commands retain their command-line interfaces but become thin frontends over reusable packages. In-process and command-line use must share the same implementation.
6. **Build state is per invocation.** Mutable flags, diagnostics, temporary paths, symbol tables, caches with mutable entries, and cancellation belong to a build/tool invocation or an explicitly shared synchronized service. Immutable architecture tables and generated read-only data can remain package globals. “Remove globals” means removing hidden mutable cross-invocation state, not banning all package-level variables.
7. **Preserve existing command behavior by default.** Existing flags, environment variables, build constraints, module semantics, output formats, and supported workflows remain compatible unless a separately reviewed proposal changes them. `GOROOT` is a compatibility/development override, not a required runtime mechanism.
8. **Separate embedding API from Go command compatibility.** The Go command aims for normal CLI compatibility. The in-process API is new and versioned with the toolchain; it provides structured configuration, diagnostics, cancellation, and results, and does not promise that internal compiler data structures are stable across Go releases.
9. **Reproducible, signed release artifacts.** The embedded resource manifest is deterministic, content-hashed, versioned, and covered by release provenance and signing. The bundle generator is part of the Go release build and is itself tested.

## Product behavior

### Normal builds

With no `GOROOT`, the executable locates its embedded manifest and resources independently of its installation directory. `go env GOROOT` returns a documented virtual-root identifier (not a fabricated usable host path); new machine-readable environment output exposes the embedded mode and resource identity. Existing commands that require an actual path receive a materialized path only for the needed resource. `go env GOROOT` compatibility and scripts that expect a physical directory need explicit migration documentation.

The standard library and toolchain sources are read from the embedded filesystem. Build outputs, module downloads, and caches remain external, configurable user data. Defaults follow platform conventions and existing Go cache behavior. No module content is embedded in the toolchain. Offline operation works for the standard library and already available dependencies; uncached third-party modules still require vendoring or a module cache.

The embedded toolchain is selected deterministically. `GOROOT` explicitly opts into an installed tree. Toolchain auto-download or switching must never download or switch behind the user's back in embedded mode; any existing `GOTOOLCHAIN` behavior must be preserved through an explicit policy and surfaced in command output. A bundled compiler cannot claim to be replaced by a downloaded version while still claiming the operation used only the embedded release.

### Host tools and cgo

Pure Go builds need no external Go tools. cgo, external linking, and platform-specific packaging may require a C compiler, linker, SDK, system headers, or platform utilities. The toolchain detects these before work where possible, reports the exact missing dependency and target, and preserves established environment overrides (`CC`, `CXX`, `CGO_*`, and linker flags). These tools are invoked through a controlled process runner; they are not silently substituted with bundled tools. In-process APIs can use cgo only when supplied a host-process capability and explicit tool configuration.

### In-process API

The initial public embedding surface is at a new standard-library-style package boundary, tentatively `toolchain` (final import path is selected after repository package review). Its stable surface should remain small:

```go
type Distribution interface {
    Version() Version
    Targets() []Target
    NewBuild(context.Context, BuildConfig) (*Build, error)
}

type Build interface {
    Build(context.Context, []Package) (Result, error)
    Close() error
}
```

This is illustrative rather than a commitment to these exact names. The API must define ownership and concurrency: a `Distribution` is immutable and safe for concurrent use; each build has isolated mutable state; callers may share only documented cache and module services. Diagnostics are structured events with source positions and stable codes where practical, with a formatter supplied for CLI-compatible output. Cancellation stops scheduling, terminates owned subprocesses, and cleans temporary outputs. APIs return errors and results; they never call `os.Exit`, print to process-global streams, or read `os.Args`/ambient environment implicitly. Environment access is captured in config or a supplied environment provider.

Do not initially expose compiler IR, linker internals, or package-private symbol structures as stable API. Provide advanced hooks only after a concrete embedder requirement and compatibility policy are documented.

## Architecture

### Distribution resource layer

Introduce a resource manifest generated at release build time. Each entry has a normalized logical path, byte length, cryptographic digest, resource class, and applicable host/target constraints. Classes include standard-library source, compiler/linker support source, headers, templates, generated tables, and documentation/licenses needed by shipped tools. The manifest has a schema version and Go toolchain version.

Resources are read through an interface that supports open, stat, directory enumeration where required, and optional materialization. The embedded implementation reads from the executable's generated resource table. The installed-tree implementation adapts a physical GOROOT to the same interface. Tests use an in-memory implementation. Resource access is read-only; build writes go only to configured work/cache directories.

Packaging should favor compressed, independently addressable chunks with per-resource hashes, allowing lazy decompression and bounded memory. Repeated bytes should be deduplicated during bundle generation. A startup integrity check validates manifest structure and release identity; each chunk is verified when read. Corrupt data fails closed with a diagnostic that names the logical resource and expected/actual digest. Do not make startup hash the entire bundle if that makes every invocation unacceptably slow.

Materialization uses a private cache keyed by toolchain version plus resource digest. It uses atomic creation, locking or equivalent race-safe publication, restrictive permissions, symlink-safe path handling, and verification both before and after writes. Cache deletion or corruption causes regeneration, never different build semantics. A `--clear-cache` operation must not affect embedded resources or user build outputs.

### Build context and dependency injection

Define explicit interfaces at package boundaries for:

- toolchain resources (`ToolchainFS`);
- project and module filesystem access, while retaining OS filesystem support;
- process execution for required external tools;
- environment lookup and target configuration;
- diagnostics/output sinks;
- temporary files and cache services;
- clocks or randomness only where determinism/tests require them.

Avoid a universal dependency container passed through every function. Introduce narrow interfaces where a package needs a capability, with a build/session context owning them. Preserve ordinary `context.Context` for cancellation and deadlines, not as an untyped bag for configuration.

### Tool boundaries

For each command, split argument parsing/CLI policy from an invocation API. The invocation accepts typed options and a build context, performs work, and returns structured diagnostics/results. The CLI translates flags and environment to options and formats results. Initially retain existing package paths and internal APIs where possible; move package boundaries only when required to break process coupling or allow public API ownership.

The `go` command's build planner calls compiler, assembler, and linker APIs in process when they are Go-owned components. This removes executable lookup and process startup for those boundaries. Keep external commands for C compilers, platform tools, plugins or user-configured tools whose contract is executable invocation. Commands that need process isolation for memory containment or crash recovery can remain process backed behind the same invocation interface, but in-process use is the supported default after hardening.

### Mutable state migration

Build an inventory and assign ownership to every mutable global. Typical migration targets include global flag sets, current compilation configuration, diagnostics writers, counters, temporary directories, mutable caches, and package-level singleton state modified during a build. Replace these with invocation structs or synchronized immutable/shared services. Generated target tables, immutable interned constants, and process-wide runtime facilities do not need artificial instance conversion.

Do not use a broad mutex around the compiler/linker as the end state. Temporary serialization can keep intermediate commits correct, but the architecture must identify and remove each shared mutation so independent builds can run concurrently. Where a process-wide facility cannot safely be isolated, document the constraint and enforce it at the API boundary until resolved.

## Migration sequence

Each phase lands in reviewable increments and keeps the ordinary Go distribution buildable. Do not start by converting every global or moving all packages at once.

### Phase 0: Baseline, inventory, and compatibility contract

- Document current command behavior, build graphs, environment variables, generated assets, filesystem reads/writes, and subprocess boundaries.
- Inventory mutable globals and classify their ownership and concurrency requirements.
- Record baseline binary sizes, startup time, standard-library build time, representative application build time, memory, and test/build results on representative host OSes.
- Publish the product contract and supported host/target matrix; define exact meaning of bundled and external requirements.

**Exit:** reviewers can trace all required Go-owned build inputs and can identify every mutable global that crosses invocation boundaries.

### Phase 1: Explicit resource access with installed-tree compatibility

- Add `ToolchainFS` and resource identity/version types.
- Route compiler, linker, assembler, `cmd/go`, and relevant support code through the interface for toolchain-owned files.
- Implement the physical GOROOT adapter and preserve current behavior as the default during this phase.
- Add test filesystems and tests that fail if toolchain code escapes the supplied filesystem.

**Exit:** an installed tree can be replaced by a test resource filesystem for toolchain-owned inputs without changing generated results.

### Phase 2: Invocation contexts and state ownership

- Add typed invocation/config objects and explicit diagnostics, process, cache, and filesystem capabilities.
- Move mutable global flags/configuration and output state into invocation ownership, package by package.
- Make temp paths unique per build and caches synchronized or build-local as appropriate.
- Add race/concurrency tests that execute separate builds with different target/flags concurrently.

**Exit:** independent invocations do not exchange options, diagnostics, temp paths, or mutable compiler/linker state; the race detector is clean for supported concurrent API use.

### Phase 3: Reusable compiler, assembler, and linker APIs

- Extract callable APIs while retaining existing CLIs as adapters.
- Remove process exits and implicit argument/environment reads from reusable paths.
- Define cancellation, cleanup, diagnostics, error, and result contracts.
- Prove command/API parity by comparing outputs, exit status, and diagnostics for a representative compiler/linker corpus.

**Exit:** a host program can compile and link a package graph without launching Go-owned compiler/assembler/linker executables, with the same semantics as CLI invocation.

### Phase 4: `cmd/go` consumes toolchain APIs

- Make tool resolution use a distribution interface rather than path concatenation.
- Invoke Go-owned tool APIs directly; preserve process execution for external tools and deliberately isolated operations.
- Make environment capture and toolchain selection explicit; implement embedded versus installed selection without ambient fallback.
- Preserve module, workspace, build cache, build tags, overlays, race, coverage, PGO, and build-mode behavior.

**Exit:** normal CLI builds pass with no installed Go tree and no child processes for Go-owned compiler/linker tools. External tool needs are diagnosed and tested.

### Phase 5: Embedded bundle format and deterministic packager

- Finalize manifest schema, chunking/compression, verification, resource classes, and cache behavior.
- Generate resource tables from the exact release source/build outputs; reject missing, duplicate, unlicensed, or target-mismatched inputs at packaging time.
- Add bundle inspection tooling for release engineering and security review.
- Build/rebuild the same release inputs twice and verify byte-for-byte deterministic bundle resources and stable manifest identity.

**Exit:** a generated bundle can be mounted through `ToolchainFS`, verifies its integrity, and yields the same toolchain behavior as the installed-tree adapter.

### Phase 6: Unified executable and host/target support

- Add the single executable frontend and embed the complete selected-release manifest.
- Implement version, tool, environment, and normal `go` command dispatch without extracting the whole distribution.
- Add lazy safe materialization only at external path boundaries.
- Publish and test the authoritative host/target support matrix, including cgo and external-linking constraints.

**Exit:** the executable builds projects from a clean machine with no Go installation for every claimed workflow/target; unsupported configurations fail early with actionable diagnostics.

### Phase 7: Hardening, compatibility, and release adoption

- Run all existing Go tests and toolchain/distribution tests against both embedded and installed modes.
- Add fuzzing for manifest parsing, path normalization, archive/chunk decoding, and cache publication.
- Validate permissions, symlink attacks, corrupt/truncated bundles, concurrent cache population, interruption, read-only install locations, relocation, and low-disk-space behavior.
- Review module download and process execution boundaries, supply-chain provenance, license inventory, and security update procedures.
- Test upgrades, rollback, cache invalidation, build reproducibility, and real projects across supported platforms.
- Establish performance and size budgets from measured baselines; optimize resource encoding, lazy loading, or API boundaries if budgets are exceeded without weakening correctness.
- Document migration from conventional `GOROOT`, environment behavior, embedding API stability, external prerequisites, and troubleshooting.

**Exit:** release engineering can produce, verify, sign, install, update, and support the artifact under the stated contract, with no known correctness or security gap in the supported matrix.

## Validation and acceptance gates

The work is not complete on a successful hello-world build. Required gates include:

1. **Functional parity:** Go command and toolchain suites pass in embedded and installed modes; selected outputs and diagnostics match for controlled inputs.
2. **No ambient installation:** integration tests run with `GOROOT` unset, `PATH` containing no Go tools, and no conventional GOROOT directory; pure Go builds succeed.
3. **Concurrency:** multiple in-process builds with distinct targets/configuration execute concurrently without state leakage, race reports, or corrupted caches.
4. **Target correctness:** every advertised GOOS/GOARCH pair passes standard-library and representative module builds; unsupported host requirements are detected before obscure downstream failures.
5. **External boundary:** cgo, external linker, SDK, and system tool behavior is tested with present, missing, and misconfigured dependencies.
6. **Resource integrity:** tampering is detected; cache interruption/corruption is recoverable; relocation and read-only installation work.
7. **Reproducibility/provenance:** deterministic bundle generation is demonstrated and release artifacts have verifiable source/version provenance and signatures.
8. **Operational quality:** startup, build time, peak memory, disk cache use, and final artifact size are measured against published budgets on representative hosts.
9. **API quality:** embedding examples cover configuration, diagnostics, cancellation, errors, concurrency, and cleanup; API stability policy is documented.
10. **Compatibility:** environment and CLI behavior changes, especially the meaning of `GOROOT`, are documented with tested migration guidance.

The project should not advertise production readiness while any claimed host/target workflow depends on an undocumented fallback to an installed Go tree.

## Principal risks and controls

| Risk | Control |
|---|---|
| Hidden filesystem reads survive behind the new resource layer | Trace file opens in tests; run with a deliberately incomplete toolchain filesystem; inventory path joins and `GOROOT` references. |
| Mutable compiler/linker globals corrupt concurrent builds | State ownership inventory; per-invocation contexts; race tests; no broad-lock end state. |
| Bundled sources omit generated or license files | Build manifest from release inputs; package-time completeness checks; compare against installed distribution behavior. |
| Single artifact becomes too large or slow to start | Measure early; deduplicate and compress addressable chunks; lazy verify/read; publish size/startup budgets. |
| Existing scripts assume GOROOT is a real directory | Explicit virtual-root semantics, compatibility override, migration docs, tests for `go env` and path consumers. |
| Embedded mode gives a false impression of self-contained cgo | Clear capability reporting and early validation of system prerequisites. |
| Cache extraction creates a security or reliability boundary | Avoid full extraction; content addressing, digest verification, private permissions, safe atomic publication, corruption recovery. |
| API extraction forks behavior from CLI | CLI must be an adapter over the same APIs; parity tests on commands and diagnostics. |
| Refactor stalls due to scope | Keep the ordinary toolchain working at each phase; land narrow package migrations with explicit exits; do not combine global cleanup, package relocation, and format redesign in one change. |

## Repository execution guidance

Implementation should begin with Phase 0 and Phase 1, not with a binary packer. Keep this document as the architectural reference. For each subsequent change, state which phase and exit criterion it advances, include tests for the newly explicit boundary, and record intentional compatibility deviations. The first implementation decision to revisit is the proposed public package location and exact API naming; the semantics and ownership rules above should remain fixed while names are settled.
