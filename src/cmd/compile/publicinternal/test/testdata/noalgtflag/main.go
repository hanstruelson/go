// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"cmd/compile/publicinternal/test/testdata/noalgtflag/weak"
	"publicinternal/abi"
)

var strong any = [8]uint64{}

func main() {
	if weak.FoldedFlags() != abi.TypeOf(strong).TFlag {
		panic("folded flags disagree with linked descriptor")
	}
}
