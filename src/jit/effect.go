// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"

	"cmd/publicinternal/goobj"
)

// effectSummary records direct effects and syntactic call targets found in
// runtime source. The linked host manifest resolves direct call candidates to
// per-symbol transitive summaries before the generated module can be reclaimed.
type effectSummary struct {
	usesUnsafe             bool
	startsGoroutine        bool
	callsForeign           bool
	exposesCodePC          bool
	hasIndirectCall        bool
	unknownSyntax          bool
	interfaceCallsCertified bool
	callSites              []effectCallSite
}

// effectCallSite preserves the source-level identity available before
// type-checking. PackagePath and Name identify imported and package functions;
// indirect sites retain only their spelling until compiler/linker metadata can
// resolve their possible targets.
type effectCallSite struct {
	packagePath     string
	name            string
	indirect        bool
	methodCandidate bool
}

// HostEffect is the linker's conservative summary of effects reachable from a
// selected host function. A zero value means the linked call graph contains no
// effect known to require a permanent module pin. HostEffectRetainsCallbacks
// records a Go-heap retention path; owner-carrying function values keep the
// generated module alive while those callbacks remain reachable.
type HostEffect uint8

const (
	HostEffectUnknown HostEffect = 1 << iota
	HostEffectUnsafe
	HostEffectForeign
	HostEffectCodePointer
	HostEffectRetainsCallbacks
	HostEffectInterfaceCall
)

func (e HostEffect) requiresPin() bool {
	return e&(HostEffectUnknown|HostEffectUnsafe|HostEffectForeign|HostEffectCodePointer|HostEffectInterfaceCall) != 0
}

func (e HostEffect) pinReason() string {
	switch {
	case e&HostEffectUnknown != 0:
		return "host call reaches an indirect or unclassified call"
	case e&HostEffectUnsafe != 0:
		return "host call reaches unsafe code"
	case e&HostEffectForeign != 0:
		return "host call reaches foreign code"
	case e&HostEffectCodePointer != 0:
		return "host call may expose a code address"
	case e&HostEffectInterfaceCall != 0:
		return "host call reaches an unresolved interface dispatch"
	default:
		return "host call has an unclassified effect"
	}
}

func (s effectSummary) requiresPin() bool {
	return s.usesUnsafe || s.callsForeign || s.exposesCodePC || s.hasIndirectCall || s.unknownSyntax
}

func compilerEffectsClassifyInterfaceCalls(effects uint8) bool {
	return effects&uint8(goobj.JITEffectInterfaceCall) != 0 && effects&uint8(goobj.JITEffectUnknown) == 0
}

func (s effectSummary) pinReason() string {
	switch {
	case s.usesUnsafe:
		return "snippet uses unsafe"
	case s.callsForeign:
		return "snippet calls foreign code"
	case s.exposesCodePC:
		return "snippet may expose a code address"
	case s.hasIndirectCall:
		return "snippet makes an indirect call with an unknown target"
	case s.unknownSyntax:
		return "snippet effects could not be classified"
	default:
		return "snippet has an unclassified pin-required effect"
	}
}

func (s effectSummary) hostPinReason(metadata *Metadata) string {
	for _, site := range s.callSites {
		if site.indirect {
			if !site.methodCandidate || !s.interfaceCallsCertified {
				return "snippet calls an indirect target that has no linker effect summary"
			}
			if metadata == nil {
				return "interface method " + site.name + " has no linker effect summary"
			}
			methodEffects, found := metadata.LookupLinkerMethodEffects(site.name)
			if !found {
				return "interface method " + site.name + " has no linked implementation summary"
			}
			for _, effects := range methodEffects {
				if effects.requiresPin() {
					return "interface method " + site.name + " reaches " + effects.pinReason()
				}
			}
			continue
		}
		if site.packagePath == "" {
			continue
		}
		if metadata == nil {
			return "host call " + site.packagePath + "." + site.name + " has no linker effect summary"
		}
		var symbol LinkedSymbol
		var found bool
		for _, abi := range [...]uint16{1, 0} { // ABIInternal, then ABI0.
			if symbol, found = metadata.LookupLinkerSymbol(site.packagePath, site.name, abi); found {
				break
			}
		}
		if !found || symbol.Kind != FunctionLink {
			return "host call " + site.packagePath + "." + site.name + " has no linker effect summary"
		}
		if symbol.Effects.requiresPin() {
			return symbol.Effects.pinReason()
		}
	}
	return ""
}

func summarizeSnippetEffects(source string, ctx PackageContext, currentPackagePath string, packages []PackageExport, declarations []Declaration, captures []Capture) effectSummary {
	var effects effectSummary
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "jit-effect.go", "package jit_effect\nfunc _jit_effect() {\n"+source+"\n}", parser.AllErrors)
	if err != nil {
		effects.unknownSyntax = true
		return effects
	}
	packageNames := make(map[string]string, len(packages))
	for _, pkg := range packages {
		packageNames[pkg.Path] = pkg.Name
	}
	imports := make(map[string]string, len(ctx.Imports))
	for _, imp := range ctx.Imports {
		alias := imp.Alias
		if alias == "" {
			alias = packageNames[imp.Path]
			if alias == "" {
				alias = path.Base(imp.Path)
			}
		}
		if alias != "_" && alias != "." {
			imports[alias] = imp.Path
		}
	}
	shadowedImports := make(map[string]bool)
	for _, capture := range captures {
		shadowedImports[capture.name] = true
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && ident.Obj != nil {
			shadowedImports[ident.Name] = true
		}
		return true
	})
	for alias := range shadowedImports {
		delete(imports, alias)
	}
	packageFunctions := make(map[string]bool)
	packageTypes := make(map[string]bool)
	for _, declaration := range declarations {
		for _, name := range declaration.Names {
			switch declaration.Kind {
			case "func":
				packageFunctions[name] = true
			case "type":
				packageTypes[name] = true
			}
		}
	}
	for _, capture := range captures {
		if capture.kind == captureTypeAlias {
			packageTypes[capture.name] = true
		}
	}
	builtins := map[string]bool{
		"append": true, "cap": true, "clear": true, "close": true,
		"complex": true, "copy": true, "delete": true, "imag": true,
		"len": true, "make": true, "max": true, "min": true, "new": true,
		"panic": true, "print": true, "println": true, "real": true,
		"recover": true,
	}
	callSet := make(map[effectCallSite]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.GoStmt:
			effects.startsGoroutine = true
		case *ast.CallExpr:
			name, importedPath, direct, methodCandidate := snippetCallIdentity(node.Fun, imports)
			if importedPath == "" && !methodCandidate {
				switch {
				case builtins[name]:
					return true
				case packageFunctions[name]:
					importedPath = currentPackagePath
				case packageTypes[name]:
					return true // type conversion, not a call edge
				default:
					// A bare identifier may name a local or captured function value.
					// Syntax alone cannot distinguish it from every package symbol.
					direct = false
					methodCandidate = false
				}
			}
			if importedPath == "unsafe" {
				effects.usesUnsafe = true
			}
			if importedPath == "C" {
				effects.callsForeign = true
			}
			if importedPath == "runtime" && (name == "FuncForPC" || name == "ResolveJITFunction") {
				effects.exposesCodePC = true
			}
			if strings.HasSuffix(importedPath, "/abi") && strings.HasPrefix(name, "FuncPCABI") {
				effects.exposesCodePC = true
			}
			if (importedPath == "reflect" || importedPath == "") && (name == "Pointer" || name == "UnsafePointer") {
				effects.exposesCodePC = true
			}
			if !direct && !methodCandidate {
				effects.hasIndirectCall = true
			}
			callSet[effectCallSite{packagePath: importedPath, name: name, indirect: !direct, methodCandidate: methodCandidate}] = true
		}
		return true
	})
	for site := range callSet {
		effects.callSites = append(effects.callSites, site)
	}
	sort.Slice(effects.callSites, func(i, j int) bool {
		a, b := effects.callSites[i], effects.callSites[j]
		if a.packagePath != b.packagePath {
			return a.packagePath < b.packagePath
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return !a.indirect && b.indirect
	})
	return effects
}

func snippetCallIdentity(fun ast.Expr, imports map[string]string) (name, packagePath string, direct, methodCandidate bool) {
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name, "", true, false
	case *ast.SelectorExpr:
		if base, ok := fun.X.(*ast.Ident); ok {
			if packagePath := imports[base.Name]; packagePath != "" {
				return fun.Sel.Name, packagePath, true, false
			}
		}
		return fun.Sel.Name, "", false, true
	case *ast.IndexExpr:
		return snippetCallIdentity(fun.X, imports)
	case *ast.IndexListExpr:
		return snippetCallIdentity(fun.X, imports)
	case *ast.ParenExpr:
		return snippetCallIdentity(fun.X, imports)
	default:
		return "<indirect>", "", false, false
	}
}
