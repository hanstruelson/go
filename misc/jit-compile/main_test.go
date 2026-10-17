// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"runtime/debug"
	"testing"

	jitcounter "jitcounter"
)

func TestEvalUsesHostPackageStateAndCanUnload(t *testing.T) {
	oldGCPercent := debug.SetGCPercent(1)
	defer debug.SetGCPercent(oldGCPercent)

	jitcounter.Value = 0
	if err := runDemo(); err != nil {
		t.Fatal(err)
	}
	if got := jitcounter.Value; got != 2 {
		t.Fatalf("shared jitcounter.Value = %d after Eval, want 2", got)
	}
}
