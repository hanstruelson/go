// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package jitmeta defines the serialized build metadata consumed by jit.
package jitmeta

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	SchemaVersion = 6
	maxBundleSize = 256 << 20
	maxEntries    = 1 << 20
)

var magic = [8]byte{'G', 'O', 'J', 'I', 'T', 'M', 'D', 0}

// Bundle separates compiler and package-scope metadata from linker candidates.
type Bundle struct {
	Build BuildMetadata
	Link  LinkMetadata
}

// BuildMetadata records the source and compiler context needed to interpret
// code associated with one package.
type BuildMetadata struct {
	CompilerVersion string
	BuildID         string
	GOOS            string
	GOARCH          string
	ArchFeatures    string
	GoVersion       string
	Race            bool
	MSan            bool
	ASan            bool
	Coverage        bool
	Checkptr        int
	PackagePath     string
	PackageName     string
	Files           []File
	Declarations    []Declaration
	Packages        []PackageExport
}

// LinkMetadata records symbol identities for which the generated Go source
// carries relocation-backed references.
type LinkMetadata struct {
	Symbols []Symbol
}

// File records import names in the file scope where a snippet may be parsed.
type File struct {
	Name    string
	Imports []Import
}

// Import records the source spelling and resolved import path. An empty Alias
// means the package's declared name; "." and "_" retain their Go meanings.
type Import struct {
	Alias      string
	SourcePath string
	Path       string
}

// Declaration is a package-scope declaration used to reconstruct the caller's
// compile-time scope. Source preserves its Go declaration syntax, including
// constant values, inferred variable initializers, and generic function bodies
// needed for runtime instantiation. Non-generic function bodies are omitted;
// their linked host implementations are reused.
type Declaration struct {
	File    string
	Names   []string
	Kind    string
	Source  string
	Generic bool
}

// Symbol identifies a package-level function, variable, or type for which the
// generated linker table carries a relocation-backed reference.
type Symbol struct {
	PackagePath string
	Name        string
	Kind        string
}

// PackageExport contains code-free compiler export data selected by the host
// build. The custom linker must bind imported symbols to the host manifest
// rather than link a second package instance.
type PackageExport struct {
	Path              string
	Name              string
	ObjectHeader      string
	Dependencies      []PackageDependency
	ExportDataVersion uint32
	Fingerprint       [8]byte
	Data              []byte
}

// PackageDependency records a package identity referenced by export data.
// The identity is retained so the runtime importer can verify that the full
// package closure came from the same build-selected snapshot.
type PackageDependency struct {
	Path string
	Name string
}

// Marshal returns a deterministic, compressed encoding of b. The encoding
// includes a schema version, uncompressed length, and digest so readers can
// reject incompatible, truncated, or corrupted metadata before using it.
func Marshal(b Bundle) ([]byte, error) {
	if err := validateBundle(b); err != nil {
		return nil, err
	}
	b = canonicalize(b)
	var payload bytes.Buffer
	w := wireWriter{w: &payload}
	w.str(b.Build.CompilerVersion)
	w.str(b.Build.BuildID)
	w.str(b.Build.GOOS)
	w.str(b.Build.GOARCH)
	w.str(b.Build.ArchFeatures)
	w.str(b.Build.GoVersion)
	w.boolean(b.Build.Race)
	w.boolean(b.Build.MSan)
	w.boolean(b.Build.ASan)
	w.boolean(b.Build.Coverage)
	w.uvarint(uint64(b.Build.Checkptr))
	w.str(b.Build.PackagePath)
	w.str(b.Build.PackageName)
	w.count(len(b.Build.Files))
	for _, f := range b.Build.Files {
		w.str(f.Name)
		w.count(len(f.Imports))
		for _, imp := range f.Imports {
			w.str(imp.Alias)
			w.str(imp.SourcePath)
			w.str(imp.Path)
		}
	}
	w.count(len(b.Build.Declarations))
	for _, d := range b.Build.Declarations {
		w.str(d.File)
		w.count(len(d.Names))
		for _, name := range d.Names {
			w.str(name)
		}
		w.str(d.Kind)
		w.str(d.Source)
		w.boolean(d.Generic)
	}
	w.count(len(b.Link.Symbols))
	for _, s := range b.Link.Symbols {
		w.str(s.PackagePath)
		w.str(s.Name)
		w.str(s.Kind)
	}
	w.count(len(b.Build.Packages))
	for _, p := range b.Build.Packages {
		w.str(p.Path)
		w.str(p.Name)
		w.str(p.ObjectHeader)
		w.count(len(p.Dependencies))
		for _, dependency := range p.Dependencies {
			w.str(dependency.Path)
			w.str(dependency.Name)
		}
		w.uvarint(uint64(p.ExportDataVersion))
		_, _ = payload.Write(p.Fingerprint[:])
		w.bytes(p.Data)
	}
	if w.err != nil {
		return nil, w.err
	}
	if payload.Len() > maxBundleSize {
		return nil, fmt.Errorf("jit metadata payload is too large: %d bytes", payload.Len())
	}
	digest := sha256.Sum256(payload.Bytes())
	var compressed bytes.Buffer
	zw, err := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(payload.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	const headerSize = 8 + 2 + 8 + sha256.Size
	out := make([]byte, headerSize)
	copy(out, magic[:])
	binary.LittleEndian.PutUint16(out[8:], SchemaVersion)
	binary.LittleEndian.PutUint64(out[10:], uint64(payload.Len()))
	copy(out[18:], digest[:])
	out = append(out, compressed.Bytes()...)
	return out, nil
}

// Unmarshal validates and decodes one Bundle. It rejects unsupported schema
// versions and bounds decompression and collection sizes.
func Unmarshal(data []byte) (Bundle, error) {
	const headerSize = 8 + 2 + 8 + sha256.Size
	if len(data) < headerSize || !bytes.Equal(data[:8], magic[:]) {
		return Bundle{}, errors.New("invalid jit metadata header")
	}
	version := binary.LittleEndian.Uint16(data[8:])
	if version != SchemaVersion {
		return Bundle{}, fmt.Errorf("unsupported jit metadata schema %d (want %d)", version, SchemaVersion)
	}
	wantSize := binary.LittleEndian.Uint64(data[10:])
	if wantSize > maxBundleSize {
		return Bundle{}, fmt.Errorf("jit metadata expands to too many bytes: %d", wantSize)
	}
	zr, err := zlib.NewReader(bytes.NewReader(data[headerSize:]))
	if err != nil {
		return Bundle{}, fmt.Errorf("open compressed jit metadata: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(zr, maxBundleSize+1))
	closeErr := zr.Close()
	if readErr != nil {
		return Bundle{}, fmt.Errorf("decompress jit metadata: %w", readErr)
	}
	if closeErr != nil {
		return Bundle{}, fmt.Errorf("close compressed jit metadata: %w", closeErr)
	}
	if uint64(len(payload)) != wantSize {
		return Bundle{}, fmt.Errorf("jit metadata size mismatch: got %d, want %d", len(payload), wantSize)
	}
	digest := sha256.Sum256(payload)
	if !bytes.Equal(digest[:], data[18:headerSize]) {
		return Bundle{}, errors.New("jit metadata checksum mismatch")
	}
	r := wireReader{r: bytes.NewReader(payload)}
	b := Bundle{Build: BuildMetadata{
		CompilerVersion: r.str(),
		BuildID:         r.str(),
		GOOS:            r.str(),
		GOARCH:          r.str(),
		ArchFeatures:    r.str(),
		GoVersion:       r.str(),
		Race:            r.boolean(),
		MSan:            r.boolean(),
		ASan:            r.boolean(),
		Coverage:        r.boolean(),
	}}
	checkptr := r.uvarint()
	if checkptr > 2 {
		r.err = errors.New("invalid checkptr level in jit metadata")
	} else {
		b.Build.Checkptr = int(checkptr)
	}
	b.Build.PackagePath = r.str()
	b.Build.PackageName = r.str()
	for n := r.count(); n > 0; n-- {
		f := File{Name: r.str()}
		for m := r.count(); m > 0; m-- {
			f.Imports = append(f.Imports, Import{Alias: r.str(), SourcePath: r.str(), Path: r.str()})
		}
		b.Build.Files = append(b.Build.Files, f)
	}
	for n := r.count(); n > 0; n-- {
		declaration := Declaration{File: r.str()}
		for m := r.count(); m > 0; m-- {
			declaration.Names = append(declaration.Names, r.str())
		}
		declaration.Kind = r.str()
		declaration.Source = r.str()
		declaration.Generic = r.boolean()
		b.Build.Declarations = append(b.Build.Declarations, declaration)
	}
	for n := r.count(); n > 0; n-- {
		b.Link.Symbols = append(b.Link.Symbols, Symbol{PackagePath: r.str(), Name: r.str(), Kind: r.str()})
	}
	for n := r.count(); n > 0; n-- {
		p := PackageExport{
			Path:         r.str(),
			Name:         r.str(),
			ObjectHeader: r.str(),
		}
		for m := r.count(); m > 0; m-- {
			p.Dependencies = append(p.Dependencies, PackageDependency{Path: r.str(), Name: r.str()})
		}
		p.ExportDataVersion = uint32(r.uvarint())
		if r.err == nil {
			_, r.err = io.ReadFull(r.r, p.Fingerprint[:])
		}
		p.Data = r.bytes()
		b.Build.Packages = append(b.Build.Packages, p)
	}
	if r.err != nil {
		return Bundle{}, r.err
	}
	if r.r.Len() != 0 {
		return Bundle{}, errors.New("trailing bytes in jit metadata payload")
	}
	if err := validateBundle(b); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func validateBundle(b Bundle) error {
	if b.Build.PackagePath == "" || b.Build.PackageName == "" {
		return errors.New("jit metadata is missing its package identity")
	}
	if b.Build.Checkptr < 0 || b.Build.Checkptr > 2 {
		return fmt.Errorf("jit metadata has invalid checkptr level %d", b.Build.Checkptr)
	}
	instrumentedModes := 0
	for _, enabled := range []bool{b.Build.Race, b.Build.MSan, b.Build.ASan} {
		if enabled {
			instrumentedModes++
		}
	}
	if instrumentedModes > 1 {
		return errors.New("jit metadata enables incompatible sanitizer modes")
	}
	if len(b.Build.Files) > maxEntries || len(b.Build.Declarations) > maxEntries ||
		len(b.Link.Symbols) > maxEntries || len(b.Build.Packages) > maxEntries {
		return errors.New("jit metadata has too many entries")
	}
	totalEntries := uint64(len(b.Build.Files) + len(b.Build.Declarations) + len(b.Link.Symbols) + len(b.Build.Packages))
	addEntries := func(n int) bool {
		totalEntries += uint64(n)
		return totalEntries <= maxEntries
	}
	files := make(map[string]bool, len(b.Build.Files))
	for _, f := range b.Build.Files {
		if f.Name == "" || len(f.Imports) > maxEntries {
			return errors.New("jit metadata has an invalid file or import list")
		}
		if !addEntries(len(f.Imports)) {
			return errors.New("jit metadata has too many total entries")
		}
		if files[f.Name] {
			return fmt.Errorf("jit metadata has duplicate file %q", f.Name)
		}
		files[f.Name] = true
		for _, imp := range f.Imports {
			if imp.SourcePath == "" || imp.Path == "" {
				return fmt.Errorf("jit metadata file %q has an empty import path", f.Name)
			}
		}
	}
	directPackages := make(map[string]bool)
	for _, f := range b.Build.Files {
		for _, imp := range f.Imports {
			if imp.Path != "C" && imp.Path != "unsafe" {
				directPackages[imp.Path] = true
			}
		}
	}
	if len(b.Build.Files) == 0 {
		return errors.New("jit metadata has no source files")
	}
	for _, d := range b.Build.Declarations {
		if len(d.Names) == 0 || len(d.Names) > maxEntries || d.Kind == "" || d.Source == "" {
			return errors.New("jit metadata has an incomplete declaration")
		}
		if !addEntries(len(d.Names)) {
			return errors.New("jit metadata has too many total entries")
		}
		if !files[d.File] {
			return fmt.Errorf("jit metadata declaration refers to unknown file %q", d.File)
		}
		for _, name := range d.Names {
			if name == "" {
				return errors.New("jit metadata declaration has an empty name")
			}
		}
	}
	symbols := make(map[string]bool, len(b.Link.Symbols))
	for _, s := range b.Link.Symbols {
		if s.PackagePath == "" || s.Name == "" || s.Kind == "" {
			return errors.New("jit metadata has an incomplete symbol")
		}
		if s.Kind != "function" && s.Kind != "variable" && s.Kind != "type" {
			return fmt.Errorf("jit metadata symbol %s.%s has invalid kind %q", s.PackagePath, s.Name, s.Kind)
		}
		key := s.PackagePath + "\x00" + s.Name + "\x00" + s.Kind
		if symbols[key] {
			return fmt.Errorf("jit metadata has duplicate symbol %s.%s (%s)", s.PackagePath, s.Name, s.Kind)
		}
		symbols[key] = true
	}
	packages := make(map[string]bool, len(b.Build.Packages))
	for changed := true; changed; {
		changed = false
		for _, p := range b.Build.Packages {
			if !directPackages[p.Path] {
				continue
			}
			for _, dependency := range p.Dependencies {
				if !directPackages[dependency.Path] {
					directPackages[dependency.Path] = true
					changed = true
				}
			}
		}
	}
	for _, p := range b.Build.Packages {
		if len(p.Dependencies) > maxEntries || !addEntries(len(p.Dependencies)) {
			return errors.New("jit metadata has too many total entries")
		}
	}
	for _, p := range b.Build.Packages {
		if !directPackages[p.Path] {
			return fmt.Errorf("jit metadata contains package data outside imported dependency closure: %q", p.Path)
		}
	}
	for _, p := range b.Build.Packages {
		if p.Path == "" || p.Name == "" || len(p.Data) == 0 {
			return errors.New("jit metadata has an incomplete package export")
		}
		if packages[p.Path] {
			return fmt.Errorf("jit metadata has duplicate export data for package %q", p.Path)
		}
		packages[p.Path] = true
		dependencies := make(map[string]bool, len(p.Dependencies))
		for _, dependency := range p.Dependencies {
			if dependency.Path == "" || dependency.Name == "" || dependency.Path == p.Path {
				return fmt.Errorf("jit metadata package %q has an invalid dependency", p.Path)
			}
			if dependencies[dependency.Path] {
				return fmt.Errorf("jit metadata package %q has duplicate dependency %q", p.Path, dependency.Path)
			}
			dependencies[dependency.Path] = true
		}
	}
	for _, s := range b.Link.Symbols {
		if s.PackagePath != b.Build.PackagePath && !packages[s.PackagePath] {
			return fmt.Errorf("jit metadata symbol %s.%s refers to package without export data", s.PackagePath, s.Name)
		}
	}
	return nil
}

func canonicalize(b Bundle) Bundle {
	b.Build.Files = append([]File(nil), b.Build.Files...)
	for i := range b.Build.Files {
		b.Build.Files[i].Imports = append([]Import(nil), b.Build.Files[i].Imports...)
		sort.Slice(b.Build.Files[i].Imports, func(a, c int) bool {
			x, y := b.Build.Files[i].Imports[a], b.Build.Files[i].Imports[c]
			if x.Path != y.Path {
				return x.Path < y.Path
			}
			if x.SourcePath != y.SourcePath {
				return x.SourcePath < y.SourcePath
			}
			return x.Alias < y.Alias
		})
	}
	sort.Slice(b.Build.Files, func(i, j int) bool { return b.Build.Files[i].Name < b.Build.Files[j].Name })
	b.Build.Declarations = append([]Declaration(nil), b.Build.Declarations...)
	sort.Slice(b.Build.Declarations, func(i, j int) bool {
		x, y := b.Build.Declarations[i], b.Build.Declarations[j]
		if x.File != y.File {
			return x.File < y.File
		}
		xName, yName := x.Names[0], y.Names[0]
		if xName != yName {
			return xName < yName
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Source < y.Source
	})
	b.Link.Symbols = append([]Symbol(nil), b.Link.Symbols...)
	sort.Slice(b.Link.Symbols, func(i, j int) bool {
		x, y := b.Link.Symbols[i], b.Link.Symbols[j]
		if x.PackagePath != y.PackagePath {
			return x.PackagePath < y.PackagePath
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Kind < y.Kind
	})
	b.Build.Packages = append([]PackageExport(nil), b.Build.Packages...)
	for i := range b.Build.Packages {
		b.Build.Packages[i].Dependencies = append([]PackageDependency(nil), b.Build.Packages[i].Dependencies...)
		b.Build.Packages[i].Data = append([]byte(nil), b.Build.Packages[i].Data...)
		sort.Slice(b.Build.Packages[i].Dependencies, func(a, c int) bool {
			x, y := b.Build.Packages[i].Dependencies[a], b.Build.Packages[i].Dependencies[c]
			if x.Path != y.Path {
				return x.Path < y.Path
			}
			return x.Name < y.Name
		})
	}
	sort.Slice(b.Build.Packages, func(i, j int) bool { return b.Build.Packages[i].Path < b.Build.Packages[j].Path })
	return b
}

type wireWriter struct {
	w   io.Writer
	err error
}

func (w *wireWriter) write(p []byte) {
	if w.err == nil {
		_, w.err = w.w.Write(p)
	}
}

func (w *wireWriter) uvarint(x uint64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], x)
	w.write(buf[:n])
}

func (w *wireWriter) count(n int)  { w.uvarint(uint64(n)) }
func (w *wireWriter) str(s string) { w.bytes([]byte(s)) }
func (w *wireWriter) bytes(p []byte) {
	w.uvarint(uint64(len(p)))
	w.write(p)
}
func (w *wireWriter) boolean(v bool) {
	if v {
		w.write([]byte{1})
	} else {
		w.write([]byte{0})
	}
}

type wireReader struct {
	r           *bytes.Reader
	err         error
	usedEntries uint64
}

func (r *wireReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	x, err := binary.ReadUvarint(r.r)
	r.err = err
	return x
}

func (r *wireReader) count() int {
	x := r.uvarint()
	if r.err == nil {
		if x > maxEntries || r.usedEntries+x > maxEntries {
			r.err = errors.New("jit metadata collection exceeds limit")
		} else {
			r.usedEntries += x
		}
	}
	if r.err != nil {
		return 0
	}
	return int(x)
}

func (r *wireReader) str() string { return string(r.bytes()) }
func (r *wireReader) bytes() []byte {
	n := r.uvarint()
	if r.err != nil {
		return nil
	}
	if n > maxBundleSize || n > uint64(r.r.Len()) {
		r.err = errors.New("invalid string or byte slice length in jit metadata")
		return nil
	}
	b := make([]byte, int(n))
	_, r.err = io.ReadFull(r.r, b)
	return b
}

func (r *wireReader) boolean() bool {
	if r.err != nil {
		return false
	}
	b, err := r.r.ReadByte()
	if err != nil {
		r.err = err
		return false
	}
	if b > 1 {
		r.err = errors.New("invalid boolean in jit metadata")
		return false
	}
	return b == 1
}
