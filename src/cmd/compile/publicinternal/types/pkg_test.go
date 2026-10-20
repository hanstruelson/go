// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"sync"
	"testing"
)

func TestPkgLookupConcurrentInterning(t *testing.T) {
	const callers = 128
	pkg := newPkg("example.test/pkg", "pkg")
	start := make(chan struct{})
	got := make(chan *Sym, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got <- pkg.Lookup("shared")
		}()
	}
	close(start)
	wg.Wait()
	close(got)

	var first *Sym
	for sym := range got {
		if first == nil {
			first = sym
			continue
		}
		if sym != first {
			t.Fatal("concurrent Lookup returned different symbols for the same name")
		}
	}
}
