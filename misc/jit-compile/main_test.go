// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"jit"
	jitcounter "jitcounter"
)

type genericCaptureBox[T any] struct {
	Value T
}

type genericCapturePair[A, B any] struct {
	First  A
	Second B
}

func init() {
	// Sampled memory-profile stacks store raw PCs for process lifetime. Disable
	// sampling so profile records do not intentionally pin test modules.
	runtime.MemProfileRate = 0
}

func TestEvalUsesHostPackageStateAndCanUnload(t *testing.T) {
	oldGCPercent := debug.SetGCPercent(1)
	defer debug.SetGCPercent(oldGCPercent)

	jitcounter.Value = 0
	localPackageCounter = 0
	if err := runDemo(); err != nil {
		t.Fatal(err)
	}
	if got := jitcounter.Value; got != 2 {
		t.Fatalf("shared jitcounter.Value = %d after Eval, want 2", got)
	}
	if got := localPackageCounter; got != 6 {
		t.Fatalf("same-package counter = %d after static and JIT increments, want 6", got)
	}
}

func evalAndMutateCallerLocal() (int, error) {
	value := 10
	code, err := jit.Eval(`value += 32`)
	if err != nil {
		return value, err
	}
	if err := code.Close(); err != nil {
		return value, err
	}
	return value, nil
}

func TestEvalReadsAndWritesCallerLocal(t *testing.T) {
	got, err := evalAndMutateCallerLocal()
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("captured caller local = %d, want 42", got)
	}
}

func evalWithLabelAndCapturedNameCollision() (int, error) {
	label := 1
	value := 10
	code, err := jit.Eval(`goto label; value = 100; label: value += label`)
	if err != nil {
		return value, err
	}
	if err := code.Close(); err != nil {
		return value, err
	}
	return value, nil
}

func TestEvalCaptureRewritePreservesLabels(t *testing.T) {
	got, err := evalWithLabelAndCapturedNameCollision()
	if err != nil {
		t.Fatal(err)
	}
	if got != 11 {
		t.Fatalf("captured value after labeled Eval = %d, want 11", got)
	}
}

type jitFieldCapture struct{ value int }

func evalFieldCapture() (*jit.Code, *jitFieldCapture, error) {
	value := &jitFieldCapture{value: 40}
	code, err := jit.Eval(`value.value++`)
	return code, value, err
}

func TestFieldSelectionDoesNotPinOwner(t *testing.T) {
	code, value, err := evalFieldCapture()
	if err != nil {
		t.Fatal(err)
	}
	if value.value != 41 {
		t.Fatalf("captured field value = %d, want 41", value.value)
	}
	pc, err := code.PC()
	if err != nil {
		t.Fatal(err)
	}
	entry := pc.Address()
	pc = jit.CodePC{}
	if err := code.Close(); err != nil {
		t.Fatalf("Close field-selector code: %v", err)
	}
	code = nil
	runtime.GC()
	if !runtime.JITWaitForRetirements(5e9) {
		t.Fatal("field-selector module did not finish retirement")
	}
	if runtime.JITModuleRegistered(entry) {
		t.Fatal("ordinary field selection pinned its JIT module")
	}
	runtime.KeepAlive(value)
}

func evalLocalConstant() (int, error) {
	const increment = 40
	before := localPackageCounter
	code, err := jit.Eval(`localPackageCounter += increment`)
	if err != nil {
		return 0, err
	}
	if err := code.Close(); err != nil {
		return 0, err
	}
	return localPackageCounter - before, nil
}

func TestEvalCapturesLocalConstant(t *testing.T) {
	localPackageCounter = 2
	got, err := evalLocalConstant()
	if err != nil {
		t.Fatal(err)
	}
	if got != 40 {
		t.Fatalf("increment from captured local constant = %d, want 40", got)
	}
}

func evalUntypedConstants() error {
	const runeValue = 'r'
	const floatValue = 1.0 / 3.0
	const complexValue = complex(1.0/3.0, 1.0/2.0)
	code, err := jit.Eval(`
	if _, ok := any(runeValue).(rune); !ok { panic("rune constant default type changed") }
	if got, ok := any(floatValue).(float64); !ok || got != 1.0/3.0 { panic("float constant category or value changed") }
	if got, ok := any(complexValue).(complex128); !ok || got != complex(1.0/3.0, 1.0/2.0) { panic("complex constant category or value changed") }
	`)
	if err != nil {
		return err
	}
	return code.Close()
}

func TestEvalPreservesUntypedConstantCategories(t *testing.T) {
	if err := evalUntypedConstants(); err != nil {
		t.Fatal(err)
	}
}

func evalLocalTypeAlias() (int, error) {
	type value = int
	x := value(10)
	code, err := jit.Eval(`var copied value = x; x = copied + 32`)
	if err != nil {
		return x, err
	}
	if err := code.Close(); err != nil {
		return x, err
	}
	return x, nil
}

func TestEvalCapturesLocalTypeAlias(t *testing.T) {
	got, err := evalLocalTypeAlias()
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("value after Eval using captured local type alias = %d, want 42", got)
	}
}

func evalWithFunctionLocalDefinedType() error {
	type value int
	var captured value
	_, err := jit.Eval(`_ = captured`)
	return err
}

func TestEvalRejectsFunctionLocalDefinedTypeIdentity(t *testing.T) {
	err := evalWithFunctionLocalDefinedType()
	if err == nil || !strings.Contains(err.Error(), "type identity") {
		t.Fatalf("Eval with a function-local defined type error = %v, want explicit type-identity rejection", err)
	}
}

func evalImportedTypeLocal() (string, error) {
	var output bytes.Buffer
	code, err := jit.Eval(`output.WriteString("dynamic")`)
	if err != nil {
		return "", err
	}
	got := output.String()
	if err := code.Close(); err != nil && !strings.Contains(err.Error(), "type or itab") {
		return got, err
	}
	return got, nil
}

func TestEvalCapturePreservesImportedTypeAndImportScope(t *testing.T) {
	got, err := evalImportedTypeLocal()
	if err != nil {
		t.Fatal(err)
	}
	if got != "dynamic" {
		t.Fatalf("captured bytes.Buffer contents = %q, want %q", got, "dynamic")
	}
}

func evalImportedTypeAliasLocal() (string, error) {
	type outputType = bytes.Buffer
	var output outputType
	code, err := jit.Eval(`output.WriteString("alias")`)
	if err != nil {
		return "", err
	}
	got := output.String()
	if err := code.Close(); err != nil && !strings.Contains(err.Error(), "type or itab") {
		return got, err
	}
	return got, nil
}

func TestEvalCapturesImportedLocalTypeAlias(t *testing.T) {
	got, err := evalImportedTypeAliasLocal()
	if err != nil {
		t.Fatal(err)
	}
	if got != "alias" {
		t.Fatalf("captured imported alias contents = %q, want %q", got, "alias")
	}
}

func evalSeesOuterLocalInShortDeclarationInitializer() (int, error) {
	value := 7
	{
		shadow, err := jit.Eval(`value += 5`)
		if err != nil {
			return value, err
		}
		if err := shadow.Close(); err != nil {
			return value, err
		}
	}
	return value, nil
}

func TestEvalCaptureRespectsShortDeclarationScope(t *testing.T) {
	got, err := evalSeesOuterLocalInShortDeclarationInitializer()
	if err != nil {
		t.Fatal(err)
	}
	if got != 12 {
		t.Fatalf("outer local after Eval in shadowing initializer = %d, want 12", got)
	}
}

func evalWithGenericLocal[T any](value T, source string) (T, error) {
	code, err := jit.Eval(source)
	if err != nil {
		return value, err
	}
	if err := code.Close(); err != nil {
		return value, err
	}
	return value, nil
}

func TestEvalCapturesConcreteGenericLocalTypes(t *testing.T) {
	gotInt, err := evalWithGenericLocal(7, `value++`)
	if err != nil {
		t.Fatalf("Eval with a concrete int generic local: %v", err)
	}
	if gotInt != 8 {
		t.Fatalf("generic int local after Eval = %d, want 8", gotInt)
	}

	gotBuffer, err := evalWithGenericLocalInSeparateFile(bytes.Buffer{}, `value.WriteString("dynamic")`)
	if err != nil {
		t.Fatalf("Eval with a concrete imported generic local: %v", err)
	}
	if got := gotBuffer.String(); got != "dynamic" {
		t.Fatalf("generic bytes.Buffer local after Eval = %q, want dynamic", got)
	}

	gotMap, err := evalWithGenericLocal(map[string]int{}, `value["dynamic"]++`)
	if err != nil {
		t.Fatalf("Eval with a concrete generic map local: %v", err)
	}
	if gotMap["dynamic"] != 1 {
		t.Fatalf("generic map local after Eval = %#v, want dynamic:1", gotMap)
	}

	initialStruct := struct {
		Count int
		Label string `json:"label"`
	}{Count: 3, Label: "before"}
	gotStruct, err := evalWithGenericLocal(initialStruct, `value.Count++; value.Label += "!"`)
	if err != nil {
		t.Fatalf("Eval with a concrete anonymous struct generic local: %v", err)
	}
	if gotStruct.Count != 4 || gotStruct.Label != "before!" {
		t.Fatalf("generic anonymous struct local after Eval = %#v, want Count=4 Label=before!", gotStruct)
	}

	writer := &bytes.Buffer{}
	gotInterface, err := evalWithGenericLocal[interface {
		WriteString(string) (int, error)
	}](writer, `value.WriteString("interface")`)
	if err != nil {
		t.Fatalf("Eval with a concrete anonymous interface generic local: %v", err)
	}
	if got := gotInterface.(interface{ String() string }).String(); got != "interface" {
		t.Fatalf("generic interface local wrote %q, want interface", got)
	}
}

func TestEvalCapturesInstantiatedNamedGenericType(t *testing.T) {
	got, err := evalWithGenericLocal(genericCaptureBox[int]{Value: 7}, `value.Value++`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != 8 {
		t.Fatalf("generic named local after Eval = %#v, want Value=8", got)
	}

	gotImported, err := evalWithGenericLocalInSeparateFile(genericCaptureBox[*bytes.Buffer]{Value: &bytes.Buffer{}}, `value.Value.WriteString("nested")`)
	if err != nil {
		t.Fatalf("Eval with a named generic type containing an imported type argument: %v", err)
	}
	if got := gotImported.Value.String(); got != "nested" {
		t.Fatalf("generic named local with imported type argument wrote %q, want nested", got)
	}

	gotPair, err := evalWithGenericLocalInSeparateFile(genericCapturePair[int, *bytes.Buffer]{Second: &bytes.Buffer{}}, `value.First++; value.Second.WriteString("pair")`)
	if err != nil {
		t.Fatalf("Eval with multiple generic arguments and an imported type: %v", err)
	}
	if gotPair.First != 1 || gotPair.Second.String() != "pair" {
		t.Fatalf("generic pair after Eval = %#v, want First=1 Second=pair", gotPair)
	}

	gotAnonymousArg, err := evalWithGenericLocal(genericCaptureBox[struct {
		Value int `json:"value"`
	}]{Value: struct {
		Value int `json:"value"`
	}{Value: 9}}, `value.Value.Value++`)
	if err != nil {
		t.Fatalf("Eval with an anonymous struct generic argument: %v", err)
	}
	if gotAnonymousArg.Value.Value != 10 {
		t.Fatalf("generic anonymous struct argument after Eval = %#v, want Value=10", gotAnonymousArg)
	}
}

func evalWithGenericFunctionLocalType() error {
	type local int
	_, err := evalWithGenericLocal(local(7), `_ = value`)
	return err
}

func TestEvalRejectsUnnameableGenericCaptureType(t *testing.T) {
	err := evalWithGenericFunctionLocalType()
	if err == nil || !strings.Contains(err.Error(), "local to a function") {
		t.Fatalf("Eval with a function-local generic type error = %v, want an explicit unnameable-type error", err)
	}

}

func evalWithoutUsingGenericLocal[T any]() error {
	var unused T
	_ = unused
	code, err := jit.Eval(`localPackageCounter++`)
	if err != nil {
		return err
	}
	return code.Close()
}

func TestEvalIgnoresUnusedUnsupportedCaptures(t *testing.T) {
	if err := evalWithoutUsingGenericLocal[int](); err != nil {
		t.Fatalf("Eval rejected an unused generic local: %v", err)
	}
	if err := func() error {
		type local int
		var unused local
		_ = unused
		code, err := jit.Eval(`localPackageCounter++`)
		if err != nil {
			return err
		}
		return code.Close()
	}(); err != nil {
		t.Fatalf("Eval rejected an unused function-local defined type: %v", err)
	}
}

func evalClosureCapturingCallerLocal() (*jit.Code, error) {
	values := []int{40}
	return jit.Eval(`retainedJITCallback = func() int { return values[0] + 2 }`)
}

func TestRepeatedEvalRequiresExplicitUnload(t *testing.T) {
	jitcounter.Value = 0
	localPackageCounter = 0
	first, err := jit.Eval(`jitcounter.Increment()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := jit.Eval(`jitcounter.Increment()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if first == second {
		t.Fatal("separate Eval calls returned the same code handle")
	}
	if got := jitcounter.Value; got != 2 {
		t.Fatalf("shared counter after two evaluations = %d, want 2", got)
	}
	if err := first.Run(); err != nil {
		t.Fatalf("first code was not retained for explicit reuse: %v", err)
	}
	if err := second.Run(); err != nil {
		t.Fatalf("second code was not retained for explicit reuse: %v", err)
	}
	if got := jitcounter.Value; got != 4 {
		t.Fatalf("shared counter after rerunning both modules = %d, want 4", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Run(); err == nil {
		t.Fatal("Run succeeded after explicit Close")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close was not idempotent: %v", err)
	}
}

func TestEscapedClosureRetainsAndReleasesJITModule(t *testing.T) {
	retainedJITCallback = nil
	code, err := evalClosureCapturingCallerLocal()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := code.PC()
	if err != nil {
		t.Fatal(err)
	}
	entry := pc.Address()
	pc = jit.CodePC{}
	if err := code.Close(); err != nil {
		t.Fatalf("Close with a live owner-bearing closure: %v", err)
	}
	code = nil
	for i := 0; i < 4; i++ {
		runtime.GC()
		runtime.Gosched()
	}
	if !runtime.JITModuleRegistered(entry) {
		t.Fatal("escaped closure did not retain its JIT module after GC")
	}
	if got := retainedJITCallback(); got != 42 {
		t.Fatalf("escaped JIT closure returned %d, want 42", got)
	}
	retainedJITCallback = nil
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		if !runtime.JITModuleRegistered(entry) {
			return
		}
	}
	t.Fatal("JIT closure module remained registered after the callback was released")
}

func installJITFinalizer(done chan struct{}) (uintptr, error) {
	code, err := jit.Eval(`
		target := new(int)
		runtime.SetFinalizer(target, func(*int) { close(done) })
	`)
	if err != nil {
		return 0, err
	}
	pc, err := code.PC()
	if err != nil {
		_ = code.Close()
		return 0, err
	}
	entry := pc.Address()
	pc = jit.CodePC{}
	if err := code.Close(); err != nil {
		return entry, err
	}
	return entry, nil
}

func installJITCleanup(done chan struct{}) (uintptr, error) {
	code, err := jit.Eval(`
		target := new(int)
		runtime.AddCleanup(target, func(chan struct{}) { close(done) }, done)
	`)
	if err != nil {
		return 0, err
	}
	pc, err := code.PC()
	if err != nil {
		_ = code.Close()
		return 0, err
	}
	entry := pc.Address()
	pc = jit.CodePC{}
	if err := code.Close(); err != nil {
		return entry, err
	}
	return entry, nil
}

func waitForJITCallback(t *testing.T, entry uintptr, done <-chan struct{}, kind string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-done:
			return
		default:
			runtime.Gosched()
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatalf("%s callback did not run for JIT module %#x", kind, entry)
}

func waitForJITModuleRetirement(t *testing.T, entry uintptr, kind string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for runtime.JITModuleRegistered(entry) {
		if err := jit.Reclaim(ctx); err != nil {
			t.Fatalf("reclaim %s JIT module: %v", kind, err)
		}
		runtime.Gosched()
		select {
		case <-ctx.Done():
			t.Fatalf("%s callback's JIT module remained registered after its record was cleared", kind)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestFinalizerRetainsJITCallbackAndAllowsRetirement(t *testing.T) {
	done := make(chan struct{})
	entry, err := installJITFinalizer(done)
	if err != nil {
		t.Fatal(err)
	}
	waitForJITCallback(t, entry, done, "finalizer")
	waitForJITModuleRetirement(t, entry, "finalizer")
}

func TestCleanupRetainsJITCallbackAndAllowsRetirement(t *testing.T) {
	done := make(chan struct{})
	entry, err := installJITCleanup(done)
	if err != nil {
		t.Fatal(err)
	}
	waitForJITCallback(t, entry, done, "cleanup")
	waitForJITModuleRetirement(t, entry, "cleanup")
}
