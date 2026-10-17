# Compiler context and JIT status

## Compiler context

The work starts at upstream `31be5b6b178` (`compiler: isolate remaining environment state`). Compiler implementation packages used by the reusable compiler are under `cmd/compile/publicinternal` and can be imported as Go packages.

The reusable API `cmd/compile/publicinternal/gc.Compile` creates fresh target, IR, frontend, type-universe, and linker contexts for each request. It parses and type-checks complete package source, runs the compiler backend, and returns a Go object archive and diagnostics. The compiler itself is linked into the caller as ordinary Go code.

The context migration also fixed target-context handoffs in type checking and ABI signature construction. Production paths should keep passing an explicit `*ir.Package` or `*types.Context`. Shared immutable tables and context-free sentinels remain package-level. The migration should not be described as eliminating every package global in the Go toolchain.

## JIT implementation status

The runtime compiler parses and compiles complete Go source from memory. The metadata format embeds the host package's source declarations, file-local import scope, export data, and ordinary build-selected package archives. Extra `-dynlink` package copies and the `jitlinkpackagefile` directive have been removed; a second compiled copy would risk creating split package state.

The former temp-file/plugin loader and its external-linker workaround have been removed. `jit.Eval(source)` now identifies its caller, parses and compiles statements at runtime in that file's package/import context, sends the object archive to the in-memory linker, maps the function, and calls it. The Linux/amd64 linker currently accepts only a self-contained text function with no relocations. It does not register the function with the Go runtime, link imports, or access host package globals; source using imports fails explicitly at link time.

`misc/jit-compile` demonstrates only that narrow path by compiling and calling `func Eval() int { return 42 }`. It does not demonstrate `jit.Eval`, `fmt.Println`, the shared counter package, or a general Go JIT. The no-relocation restriction and missing runtime module registration are real limitations.

## Remaining work

- Implement request-scoped symbol resolution over compiler object archives and resolve imported functions/globals to the final executable's actual addresses.
- Extend the linker to place object sections and apply supported Go/amd64 relocations, including GOT and call trampolines, with ABI validation and precise unresolved-symbol diagnostics.
- Add runtime module registration for code, pclntab, funcdata, stack maps, types, itabs, GC masks, init tasks, and traceback/panic support.
- Expose `jit.Eval(source)` with runtime parsing/type checking in the call site's package/import scope, initially without lexical locals.
- Prove that dynamically executed source calls the host's imported `fmt.Println`, increments the same `jitcounter.Value`, and can be evaluated repeatedly.
- Add lexical local capture only after package-scope linking works. Keep mapped code for process lifetime until the runtime has a safe module reclamation protocol.

The detailed sequence and module format are in [jit-in-memory-linker-plan.md](jit-in-memory-linker-plan.md).
