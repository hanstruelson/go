// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jitmeta

import "testing"

func TestBuildModeSnapshotRoundTrip(t *testing.T) {
	want := Bundle{Build: BuildMetadata{
		CompilerVersion: "go-test",
		GOOS:            "linux",
		GOARCH:          "amd64",
		GoVersion:       "go1.26",
		Race:            true,
		Coverage:        true,
		Checkptr:        2,
		PackagePath:     "example/app",
		PackageName:     "app",
		Files:           []File{{Name: "main.go"}},
	}}
	data, err := Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Build.Race != want.Build.Race || got.Build.MSan != want.Build.MSan ||
		got.Build.ASan != want.Build.ASan || got.Build.Coverage != want.Build.Coverage ||
		got.Build.Checkptr != want.Build.Checkptr {
		t.Fatalf("round-tripped build mode = %+v, want %+v", got.Build, want.Build)
	}
}

func TestBuildMetadataRejectsInvalidCheckptrLevel(t *testing.T) {
	bundle := Bundle{Build: BuildMetadata{
		PackagePath: "example/app",
		PackageName: "app",
		Checkptr:    3,
		Files:       []File{{Name: "main.go"}},
	}}
	if _, err := Marshal(bundle); err == nil {
		t.Fatal("Marshal accepted an invalid checkptr level")
	}
}
