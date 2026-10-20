// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// jit-compile demonstrates runtime compilation, imported package linking,
// shared package state, Go stack metadata, and unloading in memory.
package main

import (
	"fmt"
	"jit"
	jitcounter "jitcounter"
)

var localPackageCounter int
var retainedJITCallback func() int

func incrementLocalPackageCounter() { localPackageCounter++ }

func addPackageValue[T ~int](value T) T { return value + 1 }

func main() {
	if err := runDemo(); err != nil {
		panic(err)
	}
}

func runDemo() error {
	fmt.Println("statically compiled code")
	localPackageCounter++
	incrementLocalPackageCounter()
	fmt.Println("package counter after static increment:", localPackageCounter)
	jitcounter.Increment()
	fmt.Println("counter after static increment:", jitcounter.Value)

	code, err := jit.Eval(`
		localPackageCounter++
		incrementLocalPackageCounter()
		localPackageCounter += int(addPackageValue(1))
		jitcounter.Increment()
		var roots [16]*int
		roots[0] = new(int)
		*roots[0] = 17
		stackTotal := jitcounter.GrowStack(64)
		for i := 0; i < 4096; i++ {
			_ = fmt.Sprintf("%d", i)
		}
		fmt.Println("dynamically compiled code")
		fmt.Println("package counter from eval:", localPackageCounter)
		fmt.Println("stack growth result:", stackTotal, *roots[0])
		fmt.Println("counter from eval:", jitcounter.Value)
	`)
	if err != nil {
		return err
	}
	fmt.Println("counter after eval:", jitcounter.Value)
	return code.Close()
}
