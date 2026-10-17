# In-memory JIT linker

## Implemented path

`jit.Eval` identifies the importing source file, parses the statement string
at runtime, type-checks and compiles it with the in-process compiler, links the
compiler object archive in memory, maps the result, registers its Go function
tables with the runtime, and executes it through an amd64 trampoline. The
statement string does not need a package clause or imports. The wrapper gets
the caller's package name and only the import declarations required by the
snippet.

The linker resolves imported names through the metadata embedded in the host
binary. The metadata contains references to the actual imported package
functions, variables, and types retained by the ordinary static link. For
example, a generated call to `jitcounter.Increment` calls the linked
`jitcounter.Increment`, and a reference to `jitcounter.Value` uses the address
of the host variable. The object archive is compiler output held in memory;
it is not a shared library and is not passed to `cmd/link`, a native linker,
or the Go plugin loader.

The runtime owns the generated function names, PC tables, and stack/GC maps
until the returned `*jit.Code` is closed. `Eval` runs the code and returns that
handle. `Code.Close` unregisters the module and unmaps its memory. It waits for
calls made through that `Code` value to finish. The caller must keep the code
loaded while any goroutine, callback, or function value can still enter it;
the runtime cannot discover arbitrary references created by evaluated code.

The integration example is `misc/jit-compile`. It statically imports and uses
`fmt` and `jitcounter`, then evaluates statements that call `fmt.Println`,
format values repeatedly while GC is forced, recursively grow the goroutine
stack through pointer-bearing frames, and increment the same `jitcounter.Value`.
Its expected counter values are 1 before Eval and 2 after Eval.

## Current limits

- The custom object linker and trampoline currently target Linux/amd64.
- The linker supports the object relocations needed by the current examples:
  absolute 64-bit addresses, amd64 PC-relative calls/data, 32-bit Go
  addroffsets, and zero-width retention markers. GOT relocations, unsupported
  relocation widths, and other object forms fail with diagnostics.
- A snippet is compiled as a separate synthetic package with the caller's
  package name and file import scope. Imported package functions and globals
  are shared with the host process, but declarations and lexical locals from
  the caller's package are not in scope. Local-variable capture is not
  implemented.
- This is not yet a general Go module loader. New package-level declarations,
  package initialization, complete reflection/type registration, arbitrary
  type descriptors, and every possible compiler relocation are not supported.
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

## Next work

1. Add broader relocation fixtures and reject malformed compiler metadata
   before mapping executable pages.
2. Register generated types, typelinks, itabs, and any linker data required by
   reflection or interface conversion.
3. Define a capture ABI for caller locals and package declarations.
4. Define runtime quiescence for unloading code that may have escaped through
   callbacks or goroutines.
5. Add panic/traceback, concurrent Run/Close, repeated Eval, and malformed
   object coverage.
