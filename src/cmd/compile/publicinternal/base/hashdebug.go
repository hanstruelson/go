// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"bytes"
	"cmd/publicinternal/obj"
	"cmd/publicinternal/src"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"publicinternal/bisect"
	"strconv"
	"strings"
	"sync"
)

type hashAndMask struct {
	// a hash h matches if (h^hash)&mask == 0
	hash uint64
	mask uint64
	name string // base name, or base name + "0", "1", etc.
}

type HashDebug struct {
	mu   sync.Mutex // for logfile, posTmp, bytesTmp
	name string     // base name of the flag/variable.
	// what file (if any) receives the yes/no logging?
	// default is os.Stdout
	logfile          io.Writer
	posTmp           []src.Pos
	bytesTmp         bytes.Buffer
	matches          []hashAndMask // A hash matches if one of these matches.
	excludes         []hashAndMask // explicitly excluded hash suffixes
	bisect           *bisect.Matcher
	fileSuffixOnly   bool // for Pos hashes, remove the directory prefix.
	inlineSuffixOnly bool // for Pos hashes, remove all but the most inline position.
}

// SetInlineSuffixOnly controls whether hashing and reporting use the entire
// inline position, or just the most-inline suffix.  Compiler debugging tends
// to want the whole inlining, debugging user problems (loopvarhash, e.g.)
// typically does not need to see the entire inline tree, there is just one
// copy of the source code.
func (d *HashDebug) SetInlineSuffixOnly(b bool) *HashDebug {
	d.inlineSuffixOnly = b
	return d
}

// TODO: Delete when we switch to bisect-only.
func toHashAndMask(s, varname string) (hashAndMask, error) {
	l := len(s)
	if l > 64 {
		s = s[l-64:]
		l = 64
	}
	m := ^(^uint64(0) << l)
	h, err := strconv.ParseUint(s, 2, 64)
	if err != nil {
		return hashAndMask{}, fmt.Errorf("could not parse %s (=%s) as a binary number", varname, s)
	}

	return hashAndMask{name: varname, hash: h, mask: m}, nil
}

// NewHashDebug returns a new hash-debug tester for the
// environment variable ev.  If ev is not set, it returns
// nil, allowing a lightweight check for normal-case behavior.
func NewHashDebug(ev, s string, file io.Writer) *HashDebug {
	hd, err := NewHashDebugWithError(ev, s, file)
	if err != nil {
		panic(err)
	}
	return hd
}

// NewHashDebugWithError parses a hash-debug selector without terminating the
// process. Reusable compiler clients should use this form and return the error
// through their request result.
func NewHashDebugWithError(ev, s string, file io.Writer) (*HashDebug, error) {
	if s == "" {
		return nil, nil
	}

	hd := &HashDebug{name: ev, logfile: file}
	if !strings.Contains(s, "/") {
		m, err := bisect.New(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ev, err)
		}
		hd.bisect = m
		return hd, nil
	}

	// TODO: Delete remainder of function when we switch to bisect-only.
	ss := strings.Split(s, "/")
	// first remove any leading exclusions; these are preceded with "-"
	i := 0
	for len(ss) > 0 {
		s := ss[0]
		if len(s) == 0 || len(s) > 0 && s[0] != '-' {
			break
		}
		ss = ss[1:]
		mask, err := toHashAndMask(s[1:], fmt.Sprintf("%s%d", "HASH_EXCLUDE", i))
		if err != nil {
			return nil, err
		}
		hd.excludes = append(hd.excludes, mask)
		i++
	}
	// hash searches may use additional EVs with 0, 1, 2, ... suffixes.
	i = 0
	for _, s := range ss {
		if s == "" {
			if i != 0 || len(ss) > 1 && ss[1] != "" || len(ss) > 2 {
				return nil, fmt.Errorf("empty hash match string for %s should be first (and only) one", ev)
			}
			// Special case of should match everything.
			zero, _ := toHashAndMask("0", fmt.Sprintf("%s0", ev))
			one, _ := toHashAndMask("1", fmt.Sprintf("%s1", ev))
			hd.matches = append(hd.matches, zero, one)
			break
		}
		var name string
		if i == 0 {
			name = ev
		} else {
			name = fmt.Sprintf("%s%d", ev, i-1)
		}
		mask, err := toHashAndMask(s, name)
		if err != nil {
			return nil, err
		}
		hd.matches = append(hd.matches, mask)
		i++
	}
	return hd, nil
}

// TODO: Delete when we switch to bisect-only.
func (d *HashDebug) excluded(hash uint64) bool {
	for _, m := range d.excludes {
		if (m.hash^hash)&m.mask == 0 {
			return true
		}
	}
	return false
}

// TODO: Delete when we switch to bisect-only.
func hashString(hash uint64) string {
	hstr := ""
	if hash == 0 {
		hstr = "0"
	} else {
		for ; hash != 0; hash = hash >> 1 {
			hstr = string('0'+byte(hash&1)) + hstr
		}
	}
	if len(hstr) > 24 {
		hstr = hstr[len(hstr)-24:]
	}
	return hstr
}

// TODO: Delete when we switch to bisect-only.
func (d *HashDebug) match(hash uint64) *hashAndMask {
	for i, m := range d.matches {
		if (m.hash^hash)&m.mask == 0 {
			return &d.matches[i]
		}
	}
	return nil
}

// MatchPkgFunc returns true if either the variable used to create d is
// unset, or if its value is y, or if it is a suffix of the base-two
// representation of the hash of pkg and fn.  If the variable is not nil,
// then a true result is accompanied by stylized output to d.logfile, which
// is used for automated bug search.
func (d *HashDebug) MatchPkgFunc(pkg, fn string, note func() string) bool {
	if d == nil {
		return true
	}
	// Written this way to make inlining likely.
	return d.matchPkgFunc(pkg, fn, note)
}

func (d *HashDebug) matchPkgFunc(pkg, fn string, note func() string) bool {
	hash := bisect.Hash(pkg, fn)
	return d.matchAndLog(hash, func() string { return pkg + "." + fn }, note)
}

// MatchPos is similar to MatchPkgFunc, but for hash computation
// it uses the source position including all inlining information instead of
// package name and path.
// Note that the default answer for no environment variable (d == nil)
// is "yes", do the thing.
func (d *HashDebug) MatchPos(pos src.XPos, desc func() string) bool {
	return d.MatchPosWithLink(Ctxt, pos, desc)
}

// MatchPosWithLink is like MatchPos, but uses ctxt instead of the legacy
// process-wide linker context.
func (d *HashDebug) MatchPosWithLink(ctxt *obj.Link, pos src.XPos, desc func() string) bool {
	if d == nil {
		return true
	}
	// Written this way to make inlining likely.
	return d.matchPos(ctxt, pos, desc)
}

func (d *HashDebug) matchPos(ctxt *obj.Link, pos src.XPos, note func() string) bool {
	return d.matchPosWithInfo(ctxt, pos, nil, note)
}

func (d *HashDebug) matchPosWithInfo(ctxt *obj.Link, pos src.XPos, info any, note func() string) bool {
	hash := d.hashPos(ctxt, pos)
	if info != nil {
		hash = bisect.Hash(hash, info)
	}
	return d.matchAndLog(hash,
		func() string {
			r := d.fmtPos(ctxt, pos)
			if info != nil {
				r += fmt.Sprintf(" (%v)", info)
			}
			return r
		},
		note)
}

// MatchPosWithInfo is similar to MatchPos, but with additional information
// that is included for hash computation, so it can distinguish multiple
// matches on the same source location.
// Note that the default answer for no environment variable (d == nil)
// is "yes", do the thing.
func (d *HashDebug) MatchPosWithInfo(pos src.XPos, info any, desc func() string) bool {
	return d.MatchPosWithInfoAndLink(Ctxt, pos, info, desc)
}

// MatchPosWithInfoAndLink is like MatchPosWithInfo, but uses ctxt instead of
// the legacy process-wide linker context.
func (d *HashDebug) MatchPosWithInfoAndLink(ctxt *obj.Link, pos src.XPos, info any, desc func() string) bool {
	if d == nil {
		return true
	}
	// Written this way to make inlining likely.
	return d.matchPosWithInfo(ctxt, pos, info, desc)
}

// matchAndLog is the core matcher. It reports whether the hash matches the pattern.
// If a report needs to be printed, match prints that report to the log file.
// The text func must be non-nil and should return a user-readable
// representation of what was hashed. The note func may be nil; if non-nil,
// it should return additional information to display to the user when this
// change is selected.
func (d *HashDebug) matchAndLog(hash uint64, text, note func() string) bool {
	if d.bisect != nil {
		enabled := d.bisect.ShouldEnable(hash)
		if d.bisect.ShouldPrint(hash) {
			disabled := ""
			if !enabled {
				disabled = " [DISABLED]"
			}
			var t string
			if !d.bisect.MarkerOnly() {
				t = text()
				if note != nil {
					if n := note(); n != "" {
						t += ": " + n + disabled
						disabled = ""
					}
				}
			}
			d.log(d.name, hash, strings.TrimSpace(t+disabled))
		}
		return enabled
	}

	// TODO: Delete rest of function body when we switch to bisect-only.
	if d.excluded(hash) {
		return false
	}
	if m := d.match(hash); m != nil {
		d.log(m.name, hash, text())
		return true
	}
	return false
}

// short returns the form of file name to use for d.
// The default is the full path, but fileSuffixOnly selects
// just the final path element.
func (d *HashDebug) short(name string) string {
	if d.fileSuffixOnly {
		return filepath.Base(name)
	}
	return name
}

// hashPos returns a hash of the position pos, including its entire inline stack.
// If d.inlineSuffixOnly is true, hashPos only considers the innermost (leaf) position on the inline stack.
func (d *HashDebug) hashPos(ctxt *obj.Link, pos src.XPos) uint64 {
	if d.inlineSuffixOnly {
		p := ctxt.InnermostPos(pos)
		return bisect.Hash(d.short(p.Filename()), p.Line(), p.Col())
	}
	h := bisect.Hash()
	ctxt.AllPos(pos, func(p src.Pos) {
		h = bisect.Hash(h, d.short(p.Filename()), p.Line(), p.Col())
	})
	return h
}

// fmtPos returns a textual formatting of the position pos, including its entire inline stack.
// If d.inlineSuffixOnly is true, fmtPos only considers the innermost (leaf) position on the inline stack.
func (d *HashDebug) fmtPos(ctxt *obj.Link, pos src.XPos) string {
	format := func(p src.Pos) string {
		return fmt.Sprintf("%s:%d:%d", d.short(p.Filename()), p.Line(), p.Col())
	}
	if d.inlineSuffixOnly {
		return format(ctxt.InnermostPos(pos))
	}
	var stk []string
	ctxt.AllPos(pos, func(p src.Pos) {
		stk = append(stk, format(p))
	})
	return strings.Join(stk, "; ")
}

// log prints a match with the given hash and textual formatting.
// TODO: Delete varname parameter when we switch to bisect-only.
func (d *HashDebug) log(varname string, hash uint64, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	file := d.logfile
	if file == nil {
		if tmpfile := os.Getenv("GSHS_LOGFILE"); tmpfile != "" {
			var err error
			file, err = os.OpenFile(tmpfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			if err != nil {
				Fatalf("could not open hash-testing logfile %s", tmpfile)
				return
			}
		}
		if file == nil {
			file = os.Stdout
		}
		d.logfile = file
	}

	// Bisect output.
	fmt.Fprintf(file, "%s %s\n", text, bisect.Marker(hash))

	// Gossahash output.
	// TODO: Delete rest of function when we switch to bisect-only.
	fmt.Fprintf(file, "%s triggered %s %s\n", varname, text, hashString(hash))
}
