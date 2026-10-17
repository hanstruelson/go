// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ModuleImage is the versioned, pointer-free contract between the JIT linker
// and the in-memory loader. Every address in the image is a section-relative
// offset until the loader validates and maps it.
//
// This format intentionally contains no native ELF image. The loader maps the
// sections directly and applies the typed relocations below.
type ModuleImage struct {
	GOOS        string
	GOARCH      string
	GoVersion   string
	ABI         string
	BuildID     string
	PackagePath string
	Sections    []ImageSection
	Symbols     []ImageSymbol
	Functions   []ImageFunctionMetadata
	Imports     []ImageImport
	Relocations []ImageRelocation
	Entry       uint32 // index into Symbols; must name a function in Text
}

// mappedImage owns the process mappings associated with one validated module
// image. Target-specific loaders populate it after relocation and protection.
type mappedImage struct {
	sections      [][]byte
	mapping       []byte
	entry         uintptr
	runtimeModule uintptr
}

type ImageSectionKind uint8

const (
	ImageText ImageSectionKind = iota + 1
	ImageReadOnly
	ImageData
	ImageBSS
	ImagePCLN
	ImageFuncData
	ImageTypes
	ImageITabs
	ImageGCData
	ImageGCBSS
	ImageModuleData
	ImageInitTasks
)

// ImageSection contains bytes for one independently protected mapping. BSS
// sections have no Data and use MemSize to describe their zero-filled extent.
type ImageSection struct {
	Kind    ImageSectionKind
	Align   uint32
	MemSize uint64
	Data    []byte
	Read    bool
	Write   bool
	Execute bool
}

type ImageSymbolKind uint8

const (
	ImageFunction ImageSymbolKind = iota + 1
	ImageVariable
	ImageType
	ImageITable
)

// ImageSymbol identifies a local definition. Offset is relative to Section.
// ABIHash is a stable compiler/type identity, not a process address.
type ImageSymbol struct {
	PackagePath string
	Name        string
	Kind        ImageSymbolKind
	Section     uint32
	Offset      uint64
	Size        uint64
	ABIHash     [16]byte
}

// ImageFunctionMetadata contains the compiler's PC tables and stack maps for
// one generated function. Symbol indexes into ModuleImage.Symbols.
type ImageFunctionMetadata struct {
	Symbol    uint32
	ArgsSize  uint32
	StartLine int32
	FuncID    uint8
	FuncFlag  uint8
	PCSP      []byte
	PCData    [][]byte
	FuncData  [][]byte
}

// ImageImport identifies a host symbol that must be resolved before mapping.
// The loader matches all fields; a package/name match alone is insufficient.
type ImageImport struct {
	PackagePath string
	Name        string
	Kind        ImageSymbolKind
	ABIHash     [16]byte
}

type ImageRelocationKind uint16

const (
	RelocAMD64Abs64 ImageRelocationKind = iota + 1
	RelocAMD64PC32
	RelocAMD64PLT32
	RelocAMD64GOTPCREL
	RelocAMD64GOTPCRELX
	RelocAMD64Addroff32
)

// ImageRelocation patches Offset in Section. TargetImport selects Imports;
// otherwise Target selects Symbols. Relocations are validated against their
// architecture-specific write width before any image is mapped.
type ImageRelocation struct {
	Section      uint32
	Offset       uint64
	Kind         ImageRelocationKind
	Target       uint32
	TargetImport bool
	Addend       int64
}

const (
	moduleImageVersion = 2
	maxModuleImageSize = 256 << 20
	maxImageSections   = 64
	maxImageEntries    = 1 << 20
	maxImageString     = 1 << 20
)

var moduleImageMagic = [8]byte{'G', 'O', 'J', 'I', 'T', 'I', 'M', 0}

// MarshalModuleImage validates and encodes an image in a deterministic,
// checksummed format. It stores no absolute pointers or native file paths.
func MarshalModuleImage(m ModuleImage) ([]byte, error) {
	if err := validateModuleImage(m); err != nil {
		return nil, err
	}
	var payload bytes.Buffer
	w := imageWriter{w: &payload}
	w.str(m.GOOS)
	w.str(m.GOARCH)
	w.str(m.GoVersion)
	w.str(m.ABI)
	w.str(m.BuildID)
	w.str(m.PackagePath)
	w.u32(m.Entry)
	w.count(len(m.Sections))
	for _, s := range m.Sections {
		w.u8(uint8(s.Kind))
		w.u32(s.Align)
		w.u64(s.MemSize)
		var flags uint8
		if s.Read {
			flags |= 1
		}
		if s.Write {
			flags |= 2
		}
		if s.Execute {
			flags |= 4
		}
		w.u8(flags)
		w.bytes(s.Data)
	}
	w.count(len(m.Symbols))
	for _, s := range m.Symbols {
		w.str(s.PackagePath)
		w.str(s.Name)
		w.u8(uint8(s.Kind))
		w.u32(s.Section)
		w.u64(s.Offset)
		w.u64(s.Size)
		w.raw(s.ABIHash[:])
	}
	w.count(len(m.Functions))
	for _, f := range m.Functions {
		w.u32(f.Symbol)
		w.u32(f.ArgsSize)
		w.u32(uint32(f.StartLine))
		w.u8(f.FuncID)
		w.u8(f.FuncFlag)
		w.bytes(f.PCSP)
		w.count(len(f.PCData))
		for _, data := range f.PCData {
			w.bytes(data)
		}
		w.count(len(f.FuncData))
		for _, data := range f.FuncData {
			w.bytes(data)
		}
	}
	w.count(len(m.Imports))
	for _, s := range m.Imports {
		w.str(s.PackagePath)
		w.str(s.Name)
		w.u8(uint8(s.Kind))
		w.raw(s.ABIHash[:])
	}
	w.count(len(m.Relocations))
	for _, r := range m.Relocations {
		w.u32(r.Section)
		w.u64(r.Offset)
		w.u16(uint16(r.Kind))
		w.u32(r.Target)
		if r.TargetImport {
			w.u8(1)
		} else {
			w.u8(0)
		}
		w.u64(uint64(r.Addend))
	}
	if w.err != nil {
		return nil, w.err
	}
	if payload.Len() > maxModuleImageSize {
		return nil, fmt.Errorf("jit: module payload too large: %d bytes", payload.Len())
	}
	digest := sha256.Sum256(payload.Bytes())
	const headerSize = 8 + 2 + 8 + sha256.Size
	out := make([]byte, headerSize, headerSize+payload.Len())
	copy(out, moduleImageMagic[:])
	binary.LittleEndian.PutUint16(out[8:], moduleImageVersion)
	binary.LittleEndian.PutUint64(out[10:], uint64(payload.Len()))
	copy(out[18:], digest[:])
	return append(out, payload.Bytes()...), nil
}

// UnmarshalModuleImage validates and decodes a module image. It rejects
// malformed metadata before the caller allocates executable memory.
func UnmarshalModuleImage(data []byte) (ModuleImage, error) {
	const headerSize = 8 + 2 + 8 + sha256.Size
	if len(data) < headerSize || !bytes.Equal(data[:8], moduleImageMagic[:]) {
		return ModuleImage{}, errors.New("jit: invalid module image header")
	}
	version := binary.LittleEndian.Uint16(data[8:])
	if version != moduleImageVersion {
		return ModuleImage{}, fmt.Errorf("jit: unsupported module image version %d", version)
	}
	want := binary.LittleEndian.Uint64(data[10:])
	if want > maxModuleImageSize || want != uint64(len(data)-headerSize) {
		return ModuleImage{}, errors.New("jit: invalid module image payload length")
	}
	got := sha256.Sum256(data[headerSize:])
	if !bytes.Equal(got[:], data[18:headerSize]) {
		return ModuleImage{}, errors.New("jit: module image checksum mismatch")
	}
	r := imageReader{r: bytes.NewReader(data[headerSize:])}
	m := ModuleImage{
		GOOS:        r.str(),
		GOARCH:      r.str(),
		GoVersion:   r.str(),
		ABI:         r.str(),
		BuildID:     r.str(),
		PackagePath: r.str(),
		Entry:       r.u32(),
	}
	for n := r.count(maxImageSections); n > 0; n-- {
		kind := ImageSectionKind(r.u8())
		align := r.u32()
		memSize := r.u64()
		flags := r.u8()
		data := r.bytes()
		m.Sections = append(m.Sections, ImageSection{
			Kind: kind, Align: align, MemSize: memSize, Data: data,
			Read: flags&1 != 0, Write: flags&2 != 0, Execute: flags&4 != 0,
		})
	}
	for n := r.count(maxImageEntries); n > 0; n-- {
		s := ImageSymbol{
			PackagePath: r.str(), Name: r.str(), Kind: ImageSymbolKind(r.u8()),
			Section: r.u32(), Offset: r.u64(), Size: r.u64(),
		}
		r.fixed(s.ABIHash[:])
		m.Symbols = append(m.Symbols, s)
	}
	for n := r.count(maxImageEntries); n > 0; n-- {
		f := ImageFunctionMetadata{
			Symbol: r.u32(), ArgsSize: r.u32(), StartLine: int32(r.u32()),
			FuncID: r.u8(), FuncFlag: r.u8(), PCSP: r.bytes(),
		}
		for count := r.count(maxImageEntries); count > 0; count-- {
			f.PCData = append(f.PCData, r.bytes())
		}
		for count := r.count(maxImageEntries); count > 0; count-- {
			f.FuncData = append(f.FuncData, r.bytes())
		}
		m.Functions = append(m.Functions, f)
	}
	for n := r.count(maxImageEntries); n > 0; n-- {
		s := ImageImport{PackagePath: r.str(), Name: r.str(), Kind: ImageSymbolKind(r.u8())}
		r.fixed(s.ABIHash[:])
		m.Imports = append(m.Imports, s)
	}
	for n := r.count(maxImageEntries); n > 0; n-- {
		reloc := ImageRelocation{
			Section: r.u32(), Offset: r.u64(), Kind: ImageRelocationKind(r.u16()),
			Target: r.u32(), TargetImport: r.u8() != 0, Addend: int64(r.u64()),
		}
		m.Relocations = append(m.Relocations, reloc)
	}
	if r.err != nil {
		return ModuleImage{}, r.err
	}
	if r.r.Len() != 0 {
		return ModuleImage{}, errors.New("jit: trailing bytes in module image")
	}
	if err := validateModuleImage(m); err != nil {
		return ModuleImage{}, err
	}
	return m, nil
}

func validateModuleImage(m ModuleImage) error {
	for field, value := range map[string]string{
		"GOOS": m.GOOS, "GOARCH": m.GOARCH, "GoVersion": m.GoVersion,
		"ABI": m.ABI, "BuildID": m.BuildID, "PackagePath": m.PackagePath,
	} {
		if value == "" || len(value) > maxImageString {
			return fmt.Errorf("jit: invalid module image %s", field)
		}
	}
	if len(m.Sections) == 0 || len(m.Sections) > maxImageSections || len(m.Symbols) == 0 {
		return errors.New("jit: invalid module image section or symbol count")
	}
	if len(m.Symbols) > maxImageEntries || len(m.Functions) > maxImageEntries || len(m.Imports) > maxImageEntries || len(m.Relocations) > maxImageEntries {
		return errors.New("jit: too many module image entries")
	}
	if int(m.Entry) >= len(m.Symbols) || m.Symbols[m.Entry].Kind != ImageFunction {
		return errors.New("jit: invalid module image entry symbol")
	}
	entrySection := m.Symbols[m.Entry].Section
	if int(entrySection) >= len(m.Sections) {
		return errors.New("jit: module image entry has invalid section")
	}
	if m.Sections[entrySection].Kind != ImageText || !m.Sections[entrySection].Read || !m.Sections[entrySection].Execute {
		return errors.New("jit: module image entry is not in readable executable text")
	}
	var totalMemSize uint64
	for i, s := range m.Sections {
		if s.Kind < ImageText || s.Kind > ImageInitTasks || !isPowerOfTwo(s.Align) || s.Align > 1<<16 {
			return fmt.Errorf("jit: invalid module image section %d", i)
		}
		if s.MemSize < uint64(len(s.Data)) || s.MemSize > maxModuleImageSize || s.MemSize == 0 {
			return fmt.Errorf("jit: invalid module image section size %d", i)
		}
		if totalMemSize > maxModuleImageSize-s.MemSize {
			return errors.New("jit: total module image mapping exceeds size limit")
		}
		totalMemSize += s.MemSize
		if s.Execute && (s.Write || s.Kind != ImageText) {
			return fmt.Errorf("jit: invalid executable section permissions at %d", i)
		}
		if s.Kind == ImageBSS && len(s.Data) != 0 {
			return fmt.Errorf("jit: BSS section %d contains file data", i)
		}
	}
	for i, s := range m.Symbols {
		if s.PackagePath == "" || s.Name == "" || len(s.PackagePath) > maxImageString || len(s.Name) > maxImageString || s.Kind < ImageFunction || s.Kind > ImageITable {
			return fmt.Errorf("jit: invalid module image symbol %d", i)
		}
		if int(s.Section) >= len(m.Sections) || s.Offset > m.Sections[s.Section].MemSize || s.Size > m.Sections[s.Section].MemSize-s.Offset {
			var sectionSize uint64
			if int(s.Section) < len(m.Sections) {
				sectionSize = m.Sections[s.Section].MemSize
			}
			return fmt.Errorf("jit: symbol %d (%s) is outside section %d: offset=%d size=%d section-size=%d", i, s.Name, s.Section, s.Offset, s.Size, sectionSize)
		}
		if i == int(m.Entry) && m.Sections[s.Section].Kind != ImageText {
			return errors.New("jit: module image entry is not in executable text")
		}
	}
	seenFunctions := make(map[uint32]bool, len(m.Functions))
	for _, f := range m.Functions {
		if int(f.Symbol) >= len(m.Symbols) || m.Symbols[f.Symbol].Kind != ImageFunction || seenFunctions[f.Symbol] {
			return errors.New("jit: invalid or duplicate function metadata symbol")
		}
		seenFunctions[f.Symbol] = true
		if len(f.PCSP) == 0 || len(f.PCData) < 2 || len(f.FuncData) < 2 || len(f.FuncData) > 255 {
			return errors.New("jit: incomplete function stack metadata")
		}
		if len(f.PCSP) > maxModuleImageSize {
			return errors.New("jit: pcsp table exceeds image limit")
		}
		for _, data := range f.PCData {
			if len(data) > maxModuleImageSize {
				return errors.New("jit: pcdata table exceeds image limit")
			}
		}
		for _, data := range f.FuncData {
			if len(data) > maxModuleImageSize {
				return errors.New("jit: funcdata table exceeds image limit")
			}
		}
	}
	for i, s := range m.Imports {
		if s.PackagePath == "" || s.Name == "" || len(s.PackagePath) > maxImageString || len(s.Name) > maxImageString || s.Kind < ImageFunction || s.Kind > ImageITable {
			return fmt.Errorf("jit: invalid module image import %d", i)
		}
	}
	for i, r := range m.Relocations {
		if int(r.Section) >= len(m.Sections) {
			return fmt.Errorf("jit: relocation %d has invalid section", i)
		}
		width, ok := imageRelocationWidth(r.Kind)
		if !ok || r.Offset > m.Sections[r.Section].MemSize || uint64(width) > m.Sections[r.Section].MemSize-r.Offset {
			return fmt.Errorf("jit: invalid relocation %d", i)
		}
		limit := len(m.Symbols)
		if r.TargetImport {
			limit = len(m.Imports)
		}
		if int(r.Target) >= limit {
			return fmt.Errorf("jit: relocation %d has invalid target", i)
		}
	}
	return nil
}

func imageRelocationWidth(kind ImageRelocationKind) (uint64, bool) {
	switch kind {
	case RelocAMD64Abs64:
		return 8, true
	case RelocAMD64PC32, RelocAMD64PLT32, RelocAMD64GOTPCREL, RelocAMD64GOTPCRELX, RelocAMD64Addroff32:
		return 4, true
	default:
		return 0, false
	}
}

func isPowerOfTwo(v uint32) bool { return v != 0 && v&(v-1) == 0 }

type imageWriter struct {
	w   io.Writer
	err error
}

func (w *imageWriter) raw(b []byte) {
	if w.err == nil {
		_, w.err = w.w.Write(b)
	}
}
func (w *imageWriter) u8(v uint8) { w.raw([]byte{v}) }
func (w *imageWriter) u16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	w.raw(b[:])
}
func (w *imageWriter) u32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.raw(b[:])
}
func (w *imageWriter) u64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.raw(b[:])
}
func (w *imageWriter) count(n int)  { w.u32(uint32(n)) }
func (w *imageWriter) str(s string) { w.bytes([]byte(s)) }
func (w *imageWriter) bytes(b []byte) {
	if len(b) > maxModuleImageSize {
		w.err = errors.New("jit: module image field too large")
		return
	}
	w.u64(uint64(len(b)))
	w.raw(b)
}

type imageReader struct {
	r   *bytes.Reader
	err error
}

func (r *imageReader) read(dst []byte) {
	if r.err == nil {
		_, r.err = io.ReadFull(r.r, dst)
	}
}
func (r *imageReader) fixed(dst []byte) { r.read(dst) }
func (r *imageReader) u8() uint8 {
	var b [1]byte
	r.read(b[:])
	return b[0]
}
func (r *imageReader) u16() uint16 {
	var b [2]byte
	r.read(b[:])
	return binary.LittleEndian.Uint16(b[:])
}
func (r *imageReader) u32() uint32 {
	var b [4]byte
	r.read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}
func (r *imageReader) u64() uint64 {
	var b [8]byte
	r.read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}
func (r *imageReader) count(max int) int {
	n := r.u32()
	if r.err == nil && uint64(n) > uint64(max) {
		r.err = errors.New("jit: module image collection exceeds limit")
		return 0
	}
	return int(n)
}
func (r *imageReader) str() string {
	b := r.bytes()
	if len(b) > maxImageString && r.err == nil {
		r.err = errors.New("jit: module image string exceeds limit")
	}
	return string(b)
}
func (r *imageReader) bytes() []byte {
	n := r.u64()
	if r.err != nil {
		return nil
	}
	if n > maxModuleImageSize || n > uint64(r.r.Len()) {
		r.err = errors.New("jit: invalid module image field length")
		return nil
	}
	b := make([]byte, int(n))
	r.read(b)
	return b
}
