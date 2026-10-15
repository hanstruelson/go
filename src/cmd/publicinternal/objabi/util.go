// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package objabi

import (
	"fmt"
	"strings"

	"publicinternal/buildcfg"
)

const (
	ElfRelocOffset   = 256
	MachoRelocOffset = 2048    // reserve enough space for ELF relocations
	GlobalDictPrefix = ".dict" // prefix for names of global dictionaries
)

// HeaderString returns the toolchain configuration string written in
// Go object headers. This string ensures we don't attempt to import
// or link object files that are incompatible with each other. This
// string always starts with "go object ".
func HeaderString() string {
	return HeaderStringFor(buildcfg.GOOS, buildcfg.GOARCH)
}

// HeaderStringFor returns the object header for the specified target. If the
// target architecture differs from the compiler process architecture, it uses
// that architecture's default feature level because no target-specific feature
// selection was supplied.
func HeaderStringFor(goos, goarch string) string {
	archExtra := ""
	k, v := goarchSetting(goarch)
	if k != "" && v != "" {
		archExtra = " " + k + "=" + v
	}
	return fmt.Sprintf("go object %s %s %s%s X:%s\n",
		goos, goarch,
		buildcfg.Version, archExtra,
		strings.Join(buildcfg.Experiment.Enabled(), ","))
}

func goarchSetting(goarch string) (string, string) {
	if goarch == buildcfg.GOARCH {
		return buildcfg.GOGOARCH()
	}
	switch goarch {
	case "386":
		return "GO386", buildcfg.DefaultGO386
	case "amd64":
		return "GOAMD64", buildcfg.DefaultGOAMD64
	case "arm":
		return "GOARM", buildcfg.DefaultGOARM
	case "arm64":
		return "GOARM64", buildcfg.DefaultGOARM64
	case "mips", "mipsle":
		return "GOMIPS", buildcfg.DefaultGOMIPS
	case "mips64", "mips64le":
		return "GOMIPS64", buildcfg.DefaultGOMIPS64
	case "ppc64", "ppc64le":
		return "GOPPC64", buildcfg.DefaultGOPPC64
	case "riscv64":
		return "GORISCV64", buildcfg.DefaultGORISCV64
	case "wasm":
		return "", ""
	default:
		return "", ""
	}
}
