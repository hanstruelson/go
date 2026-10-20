// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux || !amd64

package runtime

func jitPlatformSupported() bool { return false }

func jitUnmapRetired(base, size uintptr) error {
	return JITModuleError("runtime: JIT retirement is unavailable on this platform")
}
