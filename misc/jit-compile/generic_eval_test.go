// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "jit"

// evalWithGenericLocalInSeparateFile exercises a call site whose file does not
// import the concrete argument type's package. Runtime type reconstruction
// must add that package from the host build's embedded export metadata.
func evalWithGenericLocalInSeparateFile[T any](value T, source string) (T, error) {
	code, err := jit.Eval(source)
	if err != nil {
		return value, err
	}
	if err := code.Close(); err != nil {
		return value, err
	}
	return value, nil
}
