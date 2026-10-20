// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import "testing"

func TestJITManifestUnnamedTypeName(t *testing.T) {
	tests := []struct {
		symbol string
		want   string
	}{
		{symbol: "type:func(int) string", want: "func(int) string"},
		{symbol: "type:map[string][]byte", want: "map[string][]byte"},
		{symbol: "type:", want: ""},
		{symbol: "pkg.func()", want: ""},
	}
	for _, test := range tests {
		if got := jitManifestUnnamedTypeName(test.symbol); got != test.want {
			t.Errorf("jitManifestUnnamedTypeName(%q) = %q, want %q", test.symbol, got, test.want)
		}
	}
}

func TestJITManifestIdentity(t *testing.T) {
	included := map[string]bool{
		"runtime":           true,
		"runtime/debug":     true,
		"example.org/p":     true,
		"example.org/p/sub": true,
	}
	tests := []struct {
		owner, symbol, wantPkg, wantName string
	}{
		{owner: "runtime", symbol: "runtime/debug.SetTraceback", wantPkg: "runtime/debug", wantName: "SetTraceback"},
		{owner: "runtime", symbol: "runtime.SetGCPercent", wantPkg: "runtime", wantName: "SetGCPercent"},
		{owner: "example.org/p", symbol: "example.org/p/sub.F", wantPkg: "example.org/p/sub", wantName: "F"},
		{owner: "runtime", symbol: "type:runtime/debug.traceType", wantPkg: "runtime/debug", wantName: "traceType"},
		{owner: "runtime", symbol: "type:func(int) string", wantPkg: "runtime", wantName: ""},
	}
	for _, test := range tests {
		gotPkg, gotName := jitManifestIdentity(test.owner, test.symbol, included)
		if gotPkg != test.wantPkg || gotName != test.wantName {
			t.Errorf("jitManifestIdentity(%q, %q) = (%q, %q), want (%q, %q)", test.owner, test.symbol, gotPkg, gotName, test.wantPkg, test.wantName)
		}
	}
}
