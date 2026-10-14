// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"sync"

	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// MatchASTDump returns true if the fn matches the value
// of the astdump debug flag.
func MatchASTDump(target *ir.Package, pkgPath string, fn *syntax.FuncDecl) bool {
	if len(target.Debug.AstDump) == 0 {
		return false
	}
	if fn.Name == nil {
		return false
	}
	return matchForDump(target, fn, pkgPath)
}

// matchForDump is marked noinline to ensure that the exported
// function MatchAstDump IS inlineable and is also small, because
// common case is AstDump is not set.
//
//go:noinline
func matchForDump(target *ir.Package, fn *syntax.FuncDecl, pkgPath string) bool {
	return ir.MatchPkgFn(pkgPath, fn.Name.Value, target.Debug.AstDump)
}

func escapedFileName(pkgPath string, fn *syntax.FuncDecl, suffix string) string {
	return ir.EscapedFileName(pkgPath+"."+fn.Name.Value, suffix)
}

var dumpFileMu sync.Mutex

// DumpNodeHTML dumps the node n to the HTML writer for fn.
func DumpNodeHTML(ctx *Context, pkg *types2.Package, file *syntax.File, info *types2.Info, fn *syntax.FuncDecl, why string, n syntax.Node) {
	ctx.dumpMu.Lock()
	defer ctx.dumpMu.Unlock()
	dumpFileMu.Lock()
	defer dumpFileMu.Unlock()
	if ctx.htmlDumpWriters == nil {
		ctx.htmlDumpWriters = make(map[*syntax.FuncDecl]*HTMLWriter)
	}
	w, ok := ctx.htmlDumpWriters[fn]
	if !ok {
		name := escapedFileName(pkg.Path(), fn, ".syntax.html")
		w = NewHTMLWriter(pkg, file, info, name, fn, "")
		ctx.htmlDumpWriters[fn] = w
		ctx.htmlDumpFuncs = append(ctx.htmlDumpFuncs, fn)
	}
	w.WritePhase(why, why)
}

// CloseHTMLWriters closes the syntax HTML writers for this target.
func CloseHTMLWriters(target *ir.Package) {
	if target == nil {
		return
	}
	ctx, _ := target.FrontendContext.(*Context)
	if ctx == nil {
		return
	}
	ctx.dumpMu.Lock()
	defer ctx.dumpMu.Unlock()
	dumpFileMu.Lock()
	defer dumpFileMu.Unlock()
	for _, fn := range ctx.htmlDumpFuncs {
		if w, ok := ctx.htmlDumpWriters[fn]; ok {
			w.Close("Writing html syntax output for %s to %s\n", w.pkgFuncName(), w.Path())
			delete(ctx.htmlDumpWriters, fn)
		}
	}
	ctx.htmlDumpFuncs = nil
	target.FrontendContext = nil
}
