// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"cmd/internal/obj"
	"cmd/internal/src"
	"fmt"
	"log"
	"os"
	"sync"
)

var sessionMu sync.Mutex
var sessionActive bool

// InSession reports whether compiler entry points are running in a scoped
// reusable session.
func InSession() bool { return sessionActive }

func commandFatal(args ...any) {
	if sessionActive {
		fmt.Fprintln(os.Stderr, args...)
		Exit(2)
	}
	log.Fatal(args...)
}

func commandFatalf(format string, args ...any) {
	if sessionActive {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		Exit(2)
	}
	log.Fatalf(format, args...)
}

// CommandErrorf reports a command-line configuration error and exits. During a
// reusable session it returns control to the caller instead of exiting.
func CommandErrorf(format string, args ...any) {
	commandFatalf(format, args...)
}

// ExitError is raised instead of terminating the process when the compiler is
// running inside a reusable session.
type ExitError int

func (e ExitError) Error() string { return "compiler exited" }

// Session scopes the command compiler's remaining base-package state so it
// can be restored after a compilation. Sessions are serialized while legacy
// compiler helpers still use package-level accessors.
type Session struct {
	closed bool

	flag  CmdFlags
	debug DebugFlags
	pos   src.XPos
	ctxt  *obj.Link

	errorMsgs       []errorMsg
	numErrors       int
	numSyntaxErrors int
	lastError       struct {
		syntax src.XPos
		other  src.XPos
		msg    string
	}
	atExitFuncs []func()

	hashDebug        *HashDebug
	convertHash      *HashDebug
	fmaHash          *HashDebug
	loopVarHash      *HashDebug
	pgoHash          *HashDebug
	literalAllocHash *HashDebug
	mergeLocalsHash  *HashDebug
	variableMakeHash *HashDebug
	debugSSA         func(phase, flag string, val int, valString string) string

	Errors       int
	SyntaxErrors int
}

// NewSession starts a fresh compiler session and saves the process-level
// adapter state for restoration by Close. The caller must close the session.
func NewSession() *Session {
	sessionMu.Lock()
	s := &Session{
		flag:             Flag,
		debug:            Debug,
		pos:              Pos,
		ctxt:             Ctxt,
		errorMsgs:        errorMsgs,
		numErrors:        numErrors,
		numSyntaxErrors:  numSyntaxErrors,
		atExitFuncs:      atExitFuncs,
		hashDebug:        hashDebug,
		convertHash:      ConvertHash,
		fmaHash:          FmaHash,
		loopVarHash:      LoopVarHash,
		pgoHash:          PGOHash,
		literalAllocHash: LiteralAllocHash,
		mergeLocalsHash:  MergeLocalsHash,
		variableMakeHash: VariableMakeHash,
		debugSSA:         DebugSSA,
	}
	s.lastError.syntax = lasterror.syntax
	s.lastError.other = lasterror.other
	s.lastError.msg = lasterror.msg

	Flag = CmdFlags{}
	Debug = DebugFlags{}
	Pos = src.NoXPos
	Ctxt = nil
	errorMsgs = nil
	numErrors = 0
	numSyntaxErrors = 0
	lasterror = struct {
		syntax src.XPos
		other  src.XPos
		msg    string
	}{}
	atExitFuncs = nil
	hashDebug = nil
	ConvertHash = nil
	FmaHash = nil
	LoopVarHash = nil
	PGOHash = nil
	LiteralAllocHash = nil
	MergeLocalsHash = nil
	VariableMakeHash = nil
	DebugSSA = nil
	sessionActive = true
	return s
}

// Close runs session cleanup hooks, records the diagnostic counts, and
// restores the state that was active before NewSession.
func (s *Session) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	defer sessionMu.Unlock()
	defer func() {
		Flag = s.flag
		Debug = s.debug
		Pos = s.pos
		Ctxt = s.ctxt
		errorMsgs = s.errorMsgs
		numErrors = s.numErrors
		numSyntaxErrors = s.numSyntaxErrors
		lasterror.syntax = s.lastError.syntax
		lasterror.other = s.lastError.other
		lasterror.msg = s.lastError.msg
		atExitFuncs = s.atExitFuncs
		hashDebug = s.hashDebug
		ConvertHash = s.convertHash
		FmaHash = s.fmaHash
		LoopVarHash = s.loopVarHash
		PGOHash = s.pgoHash
		LiteralAllocHash = s.literalAllocHash
		MergeLocalsHash = s.mergeLocalsHash
		VariableMakeHash = s.variableMakeHash
		DebugSSA = s.debugSSA
		sessionActive = false
	}()

	s.Errors = numErrors
	s.SyntaxErrors = numSyntaxErrors
	for len(atExitFuncs) > 0 {
		i := len(atExitFuncs) - 1
		f := atExitFuncs[i]
		atExitFuncs = atExitFuncs[:i]
		f()
	}
}
