// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect

import (
	"bytes"
	"testing"

	"publicinternal/abi"
)

func TestJITReflectCacheKeyUsesStructuralIdentity(t *testing.T) {
	typ := abi.TypeFor[struct {
		value []byte `json:"value"`
		Count int
	}]()
	copyType := *typ
	first := jitReflectCacheKey(17, []*abi.Type{typ}, "")
	second := jitReflectCacheKey(17, []*abi.Type{&copyType}, "")
	if !bytes.Equal(first, second) {
		t.Fatal("equivalent unnamed type descriptors produced different cache keys")
	}

	different := abi.TypeFor[struct {
		value []byte `json:"other"`
		Count int
	}]()
	third := jitReflectCacheKey(17, []*abi.Type{different}, "")
	if bytes.Equal(first, third) {
		t.Fatal("struct field tags were omitted from the cache key")
	}
}

func TestJITReflectCacheKeyDistinguishesCompositeShapes(t *testing.T) {
	tests := []struct {
		name string
		a, b *abi.Type
	}{
		{
			name: "array length",
			a:    abi.TypeFor[[2]int](),
			b:    abi.TypeFor[[3]int](),
		},
		{
			name: "pointer element",
			a:    abi.TypeFor[*int](),
			b:    abi.TypeFor[*int32](),
		},
		{
			name: "map element",
			a:    abi.TypeFor[map[string]int](),
			b:    abi.TypeFor[map[string]int32](),
		},
		{
			name: "channel direction",
			a:    abi.TypeFor[chan int](),
			b:    abi.TypeFor[<-chan int](),
		},
		{
			name: "function parameter",
			a:    abi.TypeFor[func(int)](),
			b:    abi.TypeFor[func(int32)](),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := jitReflectCacheKey(1, []*abi.Type{test.a}, "")
			b := jitReflectCacheKey(1, []*abi.Type{test.b}, "")
			if bytes.Equal(a, b) {
				t.Fatalf("different %s produced the same cache key", test.name)
			}
		})
	}
}
