// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"cmd/publicinternal/obj"
	"cmd/publicinternal/objabi"
	"strconv"
)

type Pkg struct {
	Context *Context // type and layout context that owns this package

	Path    string // string literal used in import statement, e.g. "publicinternal/runtime/sys"
	Name    string // package name, e.g. "sys"
	Prefix  string // escaped path for use in symbol table
	Syms    map[string]*Sym
	Pathsym *obj.LSym

	Direct bool // imported directly
	Local  bool // package being compiled in this compiler context

	SlicedataGen int // next generated mutable string or embed data symbol suffix
}

// NewPkgInstanceFor returns a fresh package object owned by context.
func NewPkgInstanceFor(context *Context, path, name string) *Pkg {
	if context == nil {
		panic("package instance requires a compilation context")
	}
	p := newPkg(path, name)
	p.Context = context
	return p
}

func newPkg(path, name string) *Pkg {
	p := new(Pkg)
	p.Path = path
	p.Name = name
	if path == "go.shape" {
		// Don't escape "go.shape", since it's not needed (it's a builtin
		// package), and we don't want escape codes showing up in shape type
		// names, which also appear in names of function/method
		// instantiations.
		p.Prefix = path
	} else {
		p.Prefix = objabi.PathToPrefix(path)
	}
	p.Syms = make(map[string]*Sym)
	return p
}

func (pkg *Pkg) Lookup(name string) *Sym {
	s, _ := pkg.LookupOK(name)
	return s
}

// LookupOK looks up name in pkg and reports whether it previously existed.
func (pkg *Pkg) LookupOK(name string) (s *Sym, existed bool) {
	if pkg == nil {
		return &Sym{Name: name}, false
	}
	if s := pkg.Syms[name]; s != nil {
		return s, true
	}

	s = &Sym{
		Name: name,
		Pkg:  pkg,
	}
	pkg.Syms[name] = s
	return s, false
}

func (pkg *Pkg) LookupBytes(name []byte) *Sym {
	if pkg == nil {
		return &Sym{Name: string(name)}
	}
	if s := pkg.Syms[string(name)]; s != nil {
		return s
	}
	return pkg.Lookup(string(name))
}

// LookupNum looks up the symbol starting with prefix and ending with
// the decimal n. If prefix is too long, LookupNum panics.
func (pkg *Pkg) LookupNum(prefix string, n int) *Sym {
	var buf [20]byte // plenty long enough for all current users
	copy(buf[:], prefix)
	b := strconv.AppendInt(buf[:len(prefix)], int64(n), 10)
	return pkg.LookupBytes(b)
}

// Selector looks up a selector identifier.
func (pkg *Pkg) Selector(localPkg *Pkg, name string) *Sym {
	if IsExported(name) {
		pkg = localPkg
	}
	return pkg.Lookup(name)
}

func InternString(b []byte) string {
	return string(b)
}
