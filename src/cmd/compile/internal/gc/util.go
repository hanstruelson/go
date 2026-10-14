// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	tracepkg "runtime/trace"
	"strings"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/internal/obj"
)

func profileName(ctxt *obj.Link, fn, suffix string) string {
	if strings.HasSuffix(fn, string(os.PathSeparator)) {
		err := os.MkdirAll(fn, 0755)
		if err != nil {
			base.Fatalf("%v", err)
		}
	}
	if fi, statErr := os.Stat(fn); statErr == nil && fi.IsDir() {
		fn = filepath.Join(fn, url.PathEscape(ctxt.Pkgpath)+suffix)
	}
	return fn
}

func startProfile(target *ir.Package) {
	// The reusable request API rejects profiling flags because profiling
	// changes process-wide runtime state. Do not let an unconfigured request
	// reset a host process's profile rates either.
	if base.InSession() {
		return
	}
	ctxt := target.Ctxt
	if target.Flags.CPUProfile != "" {
		fn := profileName(ctxt, target.Flags.CPUProfile, ".cpuprof")
		f, err := os.Create(fn)
		if err != nil {
			base.Fatalf("%v", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			base.Fatalf("%v", err)
		}
		base.AtExit(func() {
			pprof.StopCPUProfile()
			if err = f.Close(); err != nil {
				base.Fatalf("error closing cpu profile: %v", err)
			}
		})
	}
	if target.Flags.MemProfile != "" {
		if target.Flags.MemProfileRate != 0 {
			runtime.MemProfileRate = target.Flags.MemProfileRate
		}
		const (
			gzipFormat = 0
			textFormat = 1
		)
		// compilebench parses the memory profile to extract memstats,
		// which are only written in the legacy (text) pprof format.
		// See golang.org/issue/18641 and runtime/pprof/pprof.go:writeHeap.
		// gzipFormat is what most people want, otherwise
		var format = textFormat
		fn := target.Flags.MemProfile
		if strings.HasSuffix(fn, string(os.PathSeparator)) {
			err := os.MkdirAll(fn, 0755)
			if err != nil {
				base.Fatalf("%v", err)
			}
		}
		if fi, statErr := os.Stat(fn); statErr == nil && fi.IsDir() {
			fn = filepath.Join(fn, url.PathEscape(ctxt.Pkgpath)+".memprof")
			format = gzipFormat
		}

		f, err := os.Create(fn)

		if err != nil {
			base.Fatalf("%v", err)
		}
		base.AtExit(func() {
			// Profile all outstanding allocations.
			runtime.GC()
			if err := pprof.Lookup("heap").WriteTo(f, format); err != nil {
				base.Fatalf("%v", err)
			}
			if err = f.Close(); err != nil {
				base.Fatalf("error closing memory profile: %v", err)
			}
		})
	} else {
		// Not doing memory profiling; disable it entirely.
		runtime.MemProfileRate = 0
	}
	if target.Flags.BlockProfile != "" {
		f, err := os.Create(profileName(ctxt, target.Flags.BlockProfile, ".blockprof"))
		if err != nil {
			base.Fatalf("%v", err)
		}
		runtime.SetBlockProfileRate(1)
		base.AtExit(func() {
			pprof.Lookup("block").WriteTo(f, 0)
			f.Close()
		})
	}
	if target.Flags.MutexProfile != "" {
		f, err := os.Create(profileName(ctxt, target.Flags.MutexProfile, ".mutexprof"))
		if err != nil {
			base.Fatalf("%v", err)
		}
		runtime.SetMutexProfileFraction(1)
		base.AtExit(func() {
			pprof.Lookup("mutex").WriteTo(f, 0)
			f.Close()
		})
	}
	if target.Flags.TraceProfile != "" {
		f, err := os.Create(profileName(ctxt, target.Flags.TraceProfile, ".trace"))
		if err != nil {
			base.Fatalf("%v", err)
		}
		if err := tracepkg.Start(f); err != nil {
			base.Fatalf("%v", err)
		}
		base.AtExit(func() {
			tracepkg.Stop()
			if err = f.Close(); err != nil {
				base.Fatalf("error closing trace profile: %v", err)
			}
		})
	}
}
