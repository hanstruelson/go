// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package syntax_test

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	compilerSyntax "cmd/compile/publicinternal/syntax"
)

func TestPublicParserAndFormatterImports(t *testing.T) {
	const source = "// greeting\npackage sample\nfunc Greeting() string { return \"hi\" }\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := new(types.Config).Check("sample", fset, []*ast.File{file}, nil); err != nil {
		t.Fatal(err)
	}
	var formatted bytes.Buffer
	if err := format.Node(&formatted, fset, file); err != nil {
		t.Fatal(err)
	}
	if got := formatted.String(); !strings.Contains(got, "// greeting") || !strings.Contains(got, "func Greeting() string") {
		t.Fatalf("formatted source is missing parsed syntax or comments:\n%s", got)
	}
}

func TestPublicCompilerSyntaxParserAndPrinterImports(t *testing.T) {
	const source = "package sample\nfunc Greeting() string { return \"hi\" }\n"

	file, err := compilerSyntax.Parse(compilerSyntax.NewFileBase("sample.go"), strings.NewReader(source), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var printed bytes.Buffer
	if _, err := compilerSyntax.Fprint(&printed, file, 0); err != nil {
		t.Fatal(err)
	}
	if got := printed.String(); !strings.Contains(got, "package sample") || !strings.Contains(got, "Greeting") {
		t.Fatalf("printed compiler syntax is incomplete:\n%s", got)
	}
}
