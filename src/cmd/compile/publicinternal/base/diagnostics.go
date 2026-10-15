// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"cmd/publicinternal/obj"
	"cmd/publicinternal/src"
	"cmd/publicinternal/telemetry/counter"
	"publicinternal/buildcfg"
	"publicinternal/types/errors"
)

// Diagnostics owns compiler diagnostics for one compilation. It is safe for
// concurrent parser workers to report errors through the same instance.
type Diagnostics struct {
	mu      sync.Mutex
	flushMu sync.Mutex
	bugStack *counter.StackCounter

	Ctxt  *obj.Link
	Flags CmdFlags
	Debug DebugFlags
	Out   io.Writer
	// PanicOnExit turns process exits into ExitError panics for embedding APIs.
	PanicOnExit bool

	errorMsgs       []errorMsg
	numErrors       int
	numSyntaxErrors int
	lasterror       struct {
		syntax src.XPos
		other  src.XPos
		msg    string
	}
}

// ExitError is raised instead of terminating the process by an embedded
// compiler request when diagnostics abort the compilation.
type ExitError int

func (e ExitError) Error() string { return "compiler exited" }

type errorMsg struct {
	pos  src.XPos
	msg  string
	code errors.Code
}

type byPos []errorMsg

func (x byPos) Len() int           { return len(x) }
func (x byPos) Less(i, j int) bool { return x[i].pos.Before(x[j].pos) }
func (x byPos) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

// NewDiagnostics creates a fresh diagnostic context writing to out. A nil
// writer selects os.Stdout.
func NewDiagnostics(out io.Writer) *Diagnostics {
	if out == nil {
		out = os.Stdout
	}
	return &Diagnostics{Out: out, bugStack: counter.NewStack("compile/bug", 16)}
}

// Configure associates diagnostics with the output and options for one target.
func (d *Diagnostics) Configure(ctxt *obj.Link, flags CmdFlags, debug DebugFlags, out io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Ctxt = ctxt
	d.Flags = flags
	d.Debug = debug
	if out != nil {
		d.Out = out
	}
}

func (d *Diagnostics) writer() io.Writer {
	if d.Out != nil {
		return d.Out
	}
	return os.Stdout
}

func (d *Diagnostics) Errors() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.numErrors
}

func (d *Diagnostics) SyntaxErrors() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.numSyntaxErrors
}

// Printf writes target-local compiler output without using process-global
// standard streams. It is safe for concurrent compiler workers.
func (d *Diagnostics) Printf(format string, args ...any) {
	if d == nil {
		return
	}
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.writer(), format, args...)
}

func (d *Diagnostics) FmtPos(pos src.XPos) string {
	if d.Ctxt == nil {
		return "???"
	}
	return d.Ctxt.OutermostPos(pos).Format(d.Flags.C == 0, d.Flags.L == 1)
}

func (d *Diagnostics) sameLine(a, b src.XPos) bool {
	if d.Ctxt == nil || !a.IsKnown() || !b.IsKnown() {
		return false
	}
	p := d.Ctxt.PosTable.Pos(a)
	q := d.Ctxt.PosTable.Pos(b)
	return p.Base() == q.Base() && p.Line() == q.Line()
}

func (d *Diagnostics) addErrorMsg(pos src.XPos, code errors.Code, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if pos.IsKnown() {
		msg = fmt.Sprintf("%v: %s", d.FmtPos(pos), msg)
	}
	d.errorMsgs = append(d.errorMsgs, errorMsg{pos: pos, msg: msg + "\n", code: code})
}

// ErrorfAt reports a formatted error at pos.
func (d *Diagnostics) ErrorfAt(pos src.XPos, code errors.Code, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	d.mu.Lock()
	if strings.HasPrefix(msg, "syntax error") {
		d.numSyntaxErrors++
		if d.sameLine(d.lasterror.syntax, pos) {
			d.mu.Unlock()
			return
		}
		d.lasterror.syntax = pos
	} else {
		if d.sameLine(d.lasterror.other, pos) && d.lasterror.msg == msg {
			d.mu.Unlock()
			return
		}
		d.lasterror.other = pos
		d.lasterror.msg = msg
	}
	d.addErrorMsg(pos, code, "%s", msg)
	d.numErrors++
	tooMany := d.numErrors >= 10 && d.Flags.LowerE == 0
	d.mu.Unlock()

	if d.Flags.LowerH != 0 {
		d.FlushErrors()
		if d.Flags.LowerO != "" && d.Flags.ExportFD <= 0 {
			os.Remove(d.Flags.LowerO)
		}
		panic("-h")
	}
	if tooMany {
		d.FlushErrors()
		fmt.Fprintf(d.writer(), "%v: too many errors\n", d.FmtPos(pos))
		d.ErrorExit()
	}
}

// WarnfAt reports a warning at pos.
func (d *Diagnostics) WarnfAt(pos src.XPos, format string, args ...any) {
	d.mu.Lock()
	d.addErrorMsg(pos, 0, format, args...)
	flush := d.Flags.LowerM != 0
	d.mu.Unlock()
	if flush {
		d.FlushErrors()
	}
}

// UpdateErrorDot rewrites the most recent matching undefined-name error.
func (d *Diagnostics) UpdateErrorDot(line, name, expr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.errorMsgs) == 0 {
		return
	}
	e := &d.errorMsgs[len(d.errorMsgs)-1]
	if strings.HasPrefix(e.msg, line) && e.msg == fmt.Sprintf("%v: undefined: %v\n", line, name) {
		e.msg = fmt.Sprintf("%v: undefined: %v in %v\n", line, name, expr)
	}
}

// FlushErrors prints queued diagnostics and resets the queue.
func (d *Diagnostics) FlushErrors() {
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	d.mu.Lock()
	if d.Ctxt != nil && d.Ctxt.Bso != nil {
		d.Ctxt.Bso.Flush()
	}
	msgs := d.errorMsgs
	d.errorMsgs = nil
	if d.Flags.LowerU == 0 {
		sort.Stable(byPos(msgs))
	}
	d.mu.Unlock()
	for i, err := range msgs {
		if i == 0 || err.msg != msgs[i-1].msg {
			fmt.Fprint(d.writer(), err.msg)
		}
	}
}

// FatalfAt reports an internal compiler error and aborts this compilation.
func (d *Diagnostics) FatalfAt(pos src.XPos, format string, args ...any) {
	d.FlushErrors()
	d.bugStack.Inc()
	if d.Debug.Panic != 0 || d.Errors() == 0 {
		fmt.Fprintf(d.writer(), "%v: internal compiler error: ", d.FmtPos(pos))
		fmt.Fprintf(d.writer(), format, args...)
		fmt.Fprintln(d.writer())
		if d.Debug.Panic == 0 && strings.HasPrefix(buildcfg.Version, "go") && !strings.Contains(buildcfg.Version, "devel") {
			fmt.Fprintln(d.writer())
			fmt.Fprintln(d.writer(), "Please file a bug report including a short program that triggers the error.")
			fmt.Fprintln(d.writer(), "https://go.dev/issue/new")
		} else {
			fmt.Fprintln(d.writer())
			_, _ = d.writer().Write(debug.Stack())
			fmt.Fprintln(d.writer())
		}
	}
	if d.Flags.LowerH != 0 {
		panic("-h")
	}
	d.ErrorExit()
}

// Fatalf reports an internal compiler error without a source position.
func (d *Diagnostics) Fatalf(format string, args ...any) {
	d.FatalfAt(src.NoXPos, format, args...)
}

// ErrorExit aborts the current compilation with status 2.
func (d *Diagnostics) ErrorExit() {
	d.FlushErrors()
	if d.Flags.LowerO != "" && d.Flags.ExportFD <= 0 {
		os.Remove(d.Flags.LowerO)
	}
	if d.PanicOnExit {
		panic(ExitError(2))
	}
	os.Exit(2)
}

// ExitIfErrors aborts the current compilation if it has reported errors.
func (d *Diagnostics) ExitIfErrors() {
	if d.Errors() > 0 {
		d.ErrorExit()
	}
}

// Assert reports an internal compiler error if condition is false.
func (d *Diagnostics) Assert(condition bool) {
	if !condition {
		d.Fatalf("assertion failed")
	}
}

// Assertf reports a formatted internal compiler error if condition is false.
func (d *Diagnostics) Assertf(condition bool, format string, args ...any) {
	if !condition {
		d.Fatalf(format, args...)
	}
}

// AssertfAt reports a formatted internal compiler error at pos if condition is false.
func (d *Diagnostics) AssertfAt(condition bool, pos src.XPos, format string, args ...any) {
	if !condition {
		d.FatalfAt(pos, format, args...)
	}
}

// Diagf adapts linker diagnostics to this context.
func (d *Diagnostics) Diagf(format string, args ...any) {
	d.ErrorfAt(src.NoXPos, 0, format, args...)
}
