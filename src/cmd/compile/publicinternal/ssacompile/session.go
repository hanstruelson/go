// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"cmd/compile/publicinternal/ssa"
	"cmd/compile/publicinternal/ssa/ssaconfig"
)

// Session scopes the command-line SSA debug options for one compiler request.
// The command-line parser still writes package-level options, so reusable
// compilation must reset and restore them while requests are serialized.
type Session struct {
	closed bool

	checkEnabled  bool
	checkRandSeed int
	passes        []ssa.Pass

	intrinsicsDebug   int
	intrinsicsDisable bool
	buildDebug        int
	buildTest         int
	buildStats        int
	buildDump         map[string]bool
	genssaDump        map[string]bool
}

var defaultPasses []ssa.Pass

func init() {
	defaultPasses = copyPasses(passes[:])
}

// NewSession saves the current SSA debug options and resets them to the
// compiler defaults. Close restores the saved values.
func NewSession() *Session {
	s := &Session{
		checkEnabled:      checkEnabled,
		checkRandSeed:     checkRandSeed,
		passes:            copyPasses(passes[:]),
		intrinsicsDebug:   ssaconfig.IntrinsicsDebug,
		intrinsicsDisable: ssaconfig.IntrinsicsDisable,
		buildDebug:        ssaconfig.BuildDebug,
		buildTest:         ssaconfig.BuildTest,
		buildStats:        ssaconfig.BuildStats,
		buildDump:         cloneBoolMap(ssaconfig.BuildDump),
		genssaDump:        cloneBoolMap(ssaconfig.GenssaDump),
	}
	checkEnabled = false
	checkRandSeed = 0
	setPasses(defaultPasses)
	ssaconfig.IntrinsicsDebug = 0
	ssaconfig.IntrinsicsDisable = false
	ssaconfig.BuildDebug = 0
	ssaconfig.BuildTest = 0
	ssaconfig.BuildStats = 0
	ssaconfig.BuildDump = make(map[string]bool)
	ssaconfig.GenssaDump = make(map[string]bool)
	return s
}

// Close restores the values saved by NewSession.
func (s *Session) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	checkEnabled = s.checkEnabled
	checkRandSeed = s.checkRandSeed
	setPasses(s.passes)
	ssaconfig.IntrinsicsDebug = s.intrinsicsDebug
	ssaconfig.IntrinsicsDisable = s.intrinsicsDisable
	ssaconfig.BuildDebug = s.buildDebug
	ssaconfig.BuildTest = s.buildTest
	ssaconfig.BuildStats = s.buildStats
	ssaconfig.BuildDump = s.buildDump
	ssaconfig.GenssaDump = s.genssaDump
}

func setPasses(src []ssa.Pass) {
	cloned := copyPasses(src)
	for i := range passes {
		passes[i] = cloned[i]
	}
}

func cloneBoolMap(src map[string]bool) map[string]bool {
	if src == nil {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
