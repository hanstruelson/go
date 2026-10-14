// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

const (
	// MaxStackVarSize is the default maximum size variable which we will
	// allocate on the stack. A target may override it for smallframes.
	MaxStackVarSize = int64(128 * 1024)

	// MaxImplicitStackVarSize is the default maximum size of implicit variables
	// that we will allocate on the stack. A target may override it for smallframes.
	//
	//   p := new(T)          allocating T on the stack
	//   p := &T{}            allocating T on the stack
	//   s := make([]T, n)    allocating [n]T on the stack
	//   s := []byte("...")   allocating [n]byte on the stack
	MaxImplicitStackVarSize = int64(64 * 1024)

	// MaxSmallArraySize is the maximum size of an array which is considered small.
	// Small arrays will be initialized directly with a sequence of constant stores.
	// Large arrays will be initialized by copying from a static temp.
	// 256 bytes was chosen to minimize generated code + statictmp size.
	MaxSmallArraySize = int64(256)
)

// StackVarSizeLimit reports the explicit stack allocation limit for target.
func (target *Package) StackVarSizeLimit() int64 {
	if target != nil && target.MaxStackVarSize > 0 {
		return target.MaxStackVarSize
	}
	return MaxStackVarSize
}

// ImplicitStackVarSizeLimit reports the implicit stack allocation limit for target.
func (target *Package) ImplicitStackVarSizeLimit() int64 {
	if target != nil && target.MaxImplicitStackVarSize > 0 {
		return target.MaxImplicitStackVarSize
	}
	return MaxImplicitStackVarSize
}
