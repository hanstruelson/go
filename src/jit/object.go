// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"cmd/publicinternal/archive"
	"cmd/publicinternal/goobj"
)

// GoObject is one compiler object embedded in an archive. Reader refers to
// the archive's original byte slice and does not copy its symbol/data blocks.
type GoObject struct {
	Name   string
	Reader *goobj.Reader
}

// ObjectSymbol is a read-only snapshot of one defined compiler symbol. Data
// aliases the original object archive bytes. Relocation targets keep the
// goobj package/symbol indexes; the linker resolves those against this object
// and the selected package archive closure.
type ObjectSymbol struct {
	Name        string
	Ref         goobj.SymRef
	Gotype      goobj.SymRef
	HasGotype   bool
	ABI         uint16
	Type        uint8
	Flags       uint8
	Flags2      uint8
	Size        uint32
	Align       uint32
	Data        []byte
	Relocations []ObjectRelocation
	Function    ObjectFunctionMetadata
}

// ObjectFunctionMetadata is the compiler's runtime stack metadata for one
// text symbol. The byte slices alias the in-memory compiler object archive.
type ObjectFunctionMetadata struct {
	ArgsSize     uint32
	LocalsSize   uint32
	StartLine    int32
	FuncID       uint8
	FuncFlag     uint8
	InlineTree   []goobj.InlTreeNode
	PCSP         []byte
	PCSPRef      goobj.SymRef
	PCFile       []byte
	PCFileRef    goobj.SymRef
	PCLine       []byte
	PCLineRef    goobj.SymRef
	Files        []string
	PCData       [][]byte
	PCDataRefs   []goobj.SymRef
	FuncData     [][]byte
	FuncDataRefs []goobj.SymRef
}

// ObjectRelocation retains the source compiler relocation encoding until the
// architecture linker translates it into an ImageRelocation.
type ObjectRelocation struct {
	Offset int32
	Size   uint8
	Type   uint16
	Addend int64
	Target goobj.SymRef
}

// ObjectReference is the decoded identity of a compiler-object relocation
// target. PackagePath is empty for non-package and built-in references.
type ObjectReference struct {
	Ref         goobj.SymRef
	PackagePath string
	Name        string
	ABI         uint16
	Local       bool
}

// Symbols returns the defined symbols in object order. It performs structural
// checks before exposing object bytes to the linker.
func (o GoObject) Symbols() ([]ObjectSymbol, error) {
	if o.Reader == nil {
		return nil, fmt.Errorf("jit: Go object %q has no reader", o.Name)
	}
	defined := o.Reader.NSym() + o.Reader.NHashed64def() + o.Reader.NHasheddef() + o.Reader.NNonpkgdef()
	if defined > maxImageEntries {
		return nil, fmt.Errorf("jit: Go object %q has too many symbols", o.Name)
	}
	symbols := make([]ObjectSymbol, defined)
	for i := range symbols {
		index := uint32(i)
		var sym *goobj.Sym
		ref := goobj.SymRef{}
		switch {
		case i < o.Reader.NSym():
			sym = o.Reader.Sym(index)
			ref = goobj.SymRef{PkgIdx: goobj.PkgIdxSelf, SymIdx: index}
		case i < o.Reader.NSym()+o.Reader.NHashed64def():
			local := uint32(i - o.Reader.NSym())
			sym = o.Reader.Hashed64Def(local)
			ref = goobj.SymRef{PkgIdx: goobj.PkgIdxHashed64, SymIdx: local}
		case i < o.Reader.NSym()+o.Reader.NHashed64def()+o.Reader.NHasheddef():
			local := uint32(i - o.Reader.NSym() - o.Reader.NHashed64def())
			sym = o.Reader.HashedDef(local)
			ref = goobj.SymRef{PkgIdx: goobj.PkgIdxHashed, SymIdx: local}
		default:
			local := uint32(i - o.Reader.NSym() - o.Reader.NHashed64def() - o.Reader.NHasheddef())
			sym = o.Reader.NonpkgDef(local)
			ref = goobj.SymRef{PkgIdx: goobj.PkgIdxNone, SymIdx: local}
		}
		if sym == nil {
			return nil, fmt.Errorf("jit: Go object %q has invalid symbol %d", o.Name, i)
		}
		out := ObjectSymbol{
			Name: sym.Name(o.Reader), Ref: ref, ABI: sym.ABI(), Type: sym.Type(),
			Flags: sym.Flag(), Flags2: sym.Flag2(), Size: sym.Siz(),
			Align: sym.Align(), Data: o.Reader.Data(index),
		}
		for auxIndex := 0; auxIndex < o.Reader.NAux(index); auxIndex++ {
			aux := o.Reader.Aux(index, auxIndex)
			ref := aux.Sym()
			switch aux.Type() {
			case goobj.AuxGotype:
				if out.HasGotype {
					return nil, fmt.Errorf("jit: Go object %q symbol %q has duplicate Go type metadata", o.Name, out.Name)
				}
				if ref.PkgIdx != goobj.PkgIdxInvalid {
					out.Gotype = ref
					out.HasGotype = true
				}
			case goobj.AuxFuncInfo:
				data, err := o.dataForReference(ref)
				if err != nil {
					return nil, fmt.Errorf("jit: read func info for %s: %w", out.Name, err)
				}
				if len(data) < 20 {
					return nil, fmt.Errorf("jit: malformed func info for %s", out.Name)
				}
				fi := new(goobj.FuncInfo)
				out.Function.ArgsSize = fi.ReadArgs(data)
				out.Function.LocalsSize = fi.ReadLocals(data)
				out.Function.FuncID = uint8(fi.ReadFuncID(data))
				out.Function.FuncFlag = uint8(fi.ReadFuncFlag(data))
				out.Function.StartLine = fi.ReadStartLine(data)
				lengths := fi.ReadFuncInfoLengths(data)
				filesEnd := uint64(lengths.FileOff) + uint64(lengths.NumFile)*4
				if lengths.NumFile > maxImageEntries || filesEnd+4 > uint64(len(data)) {
					return nil, fmt.Errorf("jit: malformed file list in func info for %s", out.Name)
				}
				inlineCount := binary.LittleEndian.Uint32(data[filesEnd:])
				inlineEnd := filesEnd + 4 + uint64(inlineCount)*24
				if inlineCount > maxImageEntries || inlineEnd > uint64(len(data)) {
					return nil, fmt.Errorf("jit: malformed inline tree in func info for %s", out.Name)
				}
				// PCFile stores raw CUFileIndex values, which are indexes into the
				// object's complete file table. Keep that index space intact: a
				// compact list of only FuncInfo's referenced files makes valid
				// source indexes point at the wrong path during runtime traceback.
				out.Function.Files = make([]string, o.Reader.NFile())
				for i := range out.Function.Files {
					out.Function.Files[i] = o.Reader.File(i)
				}
				for i := uint32(0); i < lengths.NumFile; i++ {
					fileIndex := fi.ReadFile(data, lengths.FileOff, i)
					if int(fileIndex) >= o.Reader.NFile() {
						return nil, fmt.Errorf("jit: func info for %s references missing file %d", out.Name, fileIndex)
					}
				}
				out.Function.InlineTree = make([]goobj.InlTreeNode, inlineCount)
				for i := range out.Function.InlineTree {
					out.Function.InlineTree[i] = fi.ReadInlTree(data, uint32(filesEnd+4), uint32(i))
				}
			case goobj.AuxPcsp:
				data, err := o.dataForReference(ref)
				if err != nil {
					return nil, fmt.Errorf("jit: read pcsp for %s: %w", out.Name, err)
				}
				out.Function.PCSP = data
				out.Function.PCSPRef = ref
			case goobj.AuxPcfile:
				data, err := o.dataForReference(ref)
				if err != nil {
					return nil, fmt.Errorf("jit: read pcfile for %s: %w", out.Name, err)
				}
				out.Function.PCFile = data
				out.Function.PCFileRef = ref
			case goobj.AuxPcline:
				data, err := o.dataForReference(ref)
				if err != nil {
					return nil, fmt.Errorf("jit: read pcline for %s: %w", out.Name, err)
				}
				out.Function.PCLine = data
				out.Function.PCLineRef = ref
			case goobj.AuxPcdata:
				data, err := o.dataForReference(ref)
				if err != nil {
					return nil, fmt.Errorf("jit: read pcdata for %s: %w", out.Name, err)
				}
				out.Function.PCData = append(out.Function.PCData, data)
				out.Function.PCDataRefs = append(out.Function.PCDataRefs, ref)
			case goobj.AuxFuncdata:
				if ref.PkgIdx == goobj.PkgIdxInvalid {
					out.Function.FuncData = append(out.Function.FuncData, nil)
					out.Function.FuncDataRefs = append(out.Function.FuncDataRefs, ref)
					continue
				}
				data, err := o.dataForReference(ref)
				if err != nil {
					return nil, fmt.Errorf("jit: read funcdata for %s: %w", out.Name, err)
				}
				out.Function.FuncData = append(out.Function.FuncData, data)
				out.Function.FuncDataRefs = append(out.Function.FuncDataRefs, ref)
			}
		}
		relocCount := o.Reader.NReloc(index)
		if relocCount > maxImageEntries {
			return nil, fmt.Errorf("jit: Go object %q symbol %q has too many relocations", o.Name, out.Name)
		}
		out.Relocations = make([]ObjectRelocation, relocCount)
		for j := range out.Relocations {
			r := o.Reader.Reloc(index, j)
			out.Relocations[j] = ObjectRelocation{
				Offset: r.Off(), Size: r.Siz(), Type: r.Type(), Addend: r.Add(), Target: r.Sym(),
			}
		}
		symbols[i] = out
	}
	return symbols, nil
}

func (o GoObject) dataForReference(ref goobj.SymRef) ([]byte, error) {
	if ref.PkgIdx == goobj.PkgIdxInvalid {
		return nil, nil
	}
	index := uint64(ref.SymIdx)
	switch ref.PkgIdx {
	case goobj.PkgIdxSelf:
		if index >= uint64(o.Reader.NSym()) {
			return nil, fmt.Errorf("self symbol index %d is out of range", index)
		}
	case goobj.PkgIdxHashed64:
		if index >= uint64(o.Reader.NHashed64def()) {
			return nil, fmt.Errorf("hashed64 symbol index %d is out of range", index)
		}
		index += uint64(o.Reader.NSym())
	case goobj.PkgIdxHashed:
		if index >= uint64(o.Reader.NHasheddef()) {
			return nil, fmt.Errorf("hashed symbol index %d is out of range", index)
		}
		index += uint64(o.Reader.NSym() + o.Reader.NHashed64def())
	case goobj.PkgIdxNone:
		if index >= uint64(o.Reader.NNonpkgdef()) {
			return nil, fmt.Errorf("non-package definition index %d is out of range", index)
		}
		index += uint64(o.Reader.NSym() + o.Reader.NHashed64def() + o.Reader.NHasheddef())
	default:
		return nil, fmt.Errorf("reference %+v is not a local definition", ref)
	}
	return o.Reader.Data(uint32(index)), nil
}

// ResolveReference decodes a relocation target without consulting the linker
// or reading an object from disk.
func (o GoObject) ResolveReference(ref goobj.SymRef) (ObjectReference, error) {
	if o.Reader == nil {
		return ObjectReference{}, fmt.Errorf("jit: Go object %q has no reader", o.Name)
	}
	result := ObjectReference{Ref: ref}
	switch ref.PkgIdx {
	case goobj.PkgIdxInvalid:
		if ref.SymIdx != 0 {
			return ObjectReference{}, fmt.Errorf("jit: object %q has invalid null symbol reference %+v", o.Name, ref)
		}
		return result, nil
	case goobj.PkgIdxSelf:
		if int(ref.SymIdx) >= o.Reader.NSym() {
			return ObjectReference{}, fmt.Errorf("jit: object %q self symbol index %d is out of range", o.Name, ref.SymIdx)
		}
		sym := o.Reader.Sym(ref.SymIdx)
		result.Name, result.ABI, result.Local = sym.Name(o.Reader), sym.ABI(), true
		return result, nil
	case goobj.PkgIdxHashed64:
		if int(ref.SymIdx) >= o.Reader.NHashed64def() {
			return ObjectReference{}, fmt.Errorf("jit: object %q short hash symbol index %d is out of range", o.Name, ref.SymIdx)
		}
		result.Name = fmt.Sprintf("hash64:%016x", o.Reader.Hash64(ref.SymIdx))
		result.Local = true
		return result, nil
	case goobj.PkgIdxHashed:
		if int(ref.SymIdx) >= o.Reader.NHasheddef() {
			return ObjectReference{}, fmt.Errorf("jit: object %q hash symbol index %d is out of range", o.Name, ref.SymIdx)
		}
		result.Name = fmt.Sprintf("hash:%x", o.Reader.Hash(ref.SymIdx)[:])
		result.Local = true
		return result, nil
	case goobj.PkgIdxNone:
		if int(ref.SymIdx) < o.Reader.NNonpkgdef() {
			sym := o.Reader.NonpkgDef(ref.SymIdx)
			result.Name, result.ABI, result.Local = sym.Name(o.Reader), sym.ABI(), true
			return result, nil
		}
		local := int(ref.SymIdx) - o.Reader.NNonpkgdef()
		if local < 0 || local >= o.Reader.NNonpkgref() {
			return ObjectReference{}, fmt.Errorf("jit: object %q non-package symbol index %d is out of range", o.Name, ref.SymIdx)
		}
		sym := o.Reader.NonpkgRef(uint32(local))
		result.Name, result.ABI = sym.Name(o.Reader), sym.ABI()
		return result, nil
	case goobj.PkgIdxBuiltin:
		if int(ref.SymIdx) >= goobj.NBuiltin() {
			return ObjectReference{}, fmt.Errorf("jit: object %q built-in symbol index %d is out of range", o.Name, ref.SymIdx)
		}
		name, abi := goobj.BuiltinName(int(ref.SymIdx))
		result.Name, result.ABI = name, uint16(abi)
		return result, nil
	default:
		if int(ref.PkgIdx) >= o.Reader.NPkg() {
			return ObjectReference{}, fmt.Errorf("jit: object %q package index %d is out of range", o.Name, ref.PkgIdx)
		}
		result.PackagePath = o.Reader.Pkg(int(ref.PkgIdx))
		for i := 0; i < o.Reader.NRefName(); i++ {
			name := o.Reader.RefName(i)
			if name.Sym() == ref {
				result.Name, result.ABI = name.Name(o.Reader), name.ABI(o.Reader)
				return result, nil
			}
		}
		return ObjectReference{}, fmt.Errorf("jit: object %q has no name for referenced symbol %+v in package %q", o.Name, ref, result.PackagePath)
	}
}

// ParseObjectArchive reads compiler output directly from memory. This is the
// input boundary required by the JIT linker: it never creates an archive file
// or a temporary extraction directory.
func ParseObjectArchive(data []byte) ([]GoObject, error) {
	parsed, err := archive.ParseBytes(data, false)
	if err != nil {
		return nil, fmt.Errorf("jit: parse compiler object archive: %w", err)
	}
	objects := make([]GoObject, 0, len(parsed.Entries))
	for _, entry := range parsed.Entries {
		if entry.Type != archive.EntryGoObj || entry.Obj == nil {
			continue
		}
		objectData, err := parsed.ObjectBytes(entry)
		if err != nil {
			return nil, fmt.Errorf("jit: read Go object %q: %w", entry.Name, err)
		}
		reader := goobj.NewReaderFromBytes(objectData, true)
		if reader == nil {
			return nil, fmt.Errorf("jit: malformed Go object %q", entry.Name)
		}
		objects = append(objects, GoObject{Name: entry.Name, Reader: reader})
	}
	if len(objects) == 0 {
		if bytes.HasPrefix(data, []byte("!<arch>\n")) {
			return nil, fmt.Errorf("jit: compiler archive contains no Go object files")
		}
		return nil, fmt.Errorf("jit: compiler output is not a Go object archive")
	}
	return objects, nil
}
