// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bio implements common I/O abstractions used within the Go toolchain.
package bio

import (
	"bufio"
	"errors"
	"io"
	"log"
	"os"
)

// Reader implements a seekable buffered io.Reader.
type Reader struct {
	r    io.ReadSeeker
	f    *os.File
	name string
	*bufio.Reader
}

// Writer implements a seekable buffered io.Writer.
type Writer struct {
	w   io.WriteSeeker
	f   *os.File
	buf *Buffer
	*bufio.Writer
}

// Buffer is an in-memory seekable byte buffer.
type Buffer struct {
	data []byte
	off  int64
}

func (b *Buffer) Write(p []byte) (int, error) {
	if b.off < 0 {
		return 0, errors.New("negative buffer offset")
	}
	end := b.off + int64(len(p))
	if end < b.off || end > int64(int(^uint(0)>>1)) {
		return 0, errors.New("buffer size overflow")
	}
	if end > int64(len(b.data)) {
		b.data = append(b.data, make([]byte, int(end)-len(b.data))...)
	}
	n := copy(b.data[int(b.off):int(end)], p)
	b.off = end
	return n, nil
}

func (b *Buffer) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = b.off + offset
	case io.SeekEnd:
		next = int64(len(b.data)) + offset
	default:
		return b.off, errors.New("invalid seek origin")
	}
	if next < 0 {
		return b.off, errors.New("negative buffer offset")
	}
	b.off = next
	return next, nil
}

// Bytes returns the written bytes. Callers should not use the result until
// writing has finished.
func (b *Buffer) Bytes() []byte { return b.data }

// Create creates the file named name and returns a Writer
// for that file.
func Create(name string) (*Writer, error) {
	f, err := os.Create(name)
	if err != nil {
		return nil, err
	}
	return &Writer{w: f, f: f, Writer: bufio.NewWriter(f)}, nil
}

// NewBufferWriter returns a Writer backed only by memory.
func NewBufferWriter() *Writer {
	b := new(Buffer)
	return &Writer{w: b, buf: b, Writer: bufio.NewWriter(b)}
}

// Bytes flushes and returns the in-memory output. It returns an error for a
// file-backed Writer.
func (w *Writer) Bytes() ([]byte, error) {
	if w.buf == nil {
		return nil, errors.New("writer is not backed by memory")
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// Open returns a Reader for the file named name.
func Open(name string) (*Reader, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return NewReader(f), nil
}

// NewReader returns a Reader from an open file.
func NewReader(f *os.File) *Reader {
	return &Reader{r: f, f: f, name: f.Name(), Reader: bufio.NewReader(f)}
}

// NewReaderFrom returns a seekable reader over r. name is used only in
// diagnostics; r may be memory-backed.
func NewReaderFrom(r io.ReadSeeker, name string) *Reader {
	reader := &Reader{r: r, name: name, Reader: bufio.NewReader(r)}
	reader.f, _ = r.(*os.File)
	return reader
}

func (r *Reader) MustSeek(offset int64, whence int) int64 {
	if whence == 1 {
		offset -= int64(r.Buffered())
	}
	off, err := r.r.Seek(offset, whence)
	if err != nil {
		log.Fatalf("seeking in output: %v", err)
	}
	r.Reset(r.r)
	return off
}

func (w *Writer) MustSeek(offset int64, whence int) int64 {
	if err := w.Flush(); err != nil {
		log.Fatalf("writing output: %v", err)
	}
	off, err := w.w.Seek(offset, whence)
	if err != nil {
		log.Fatalf("seeking in output: %v", err)
	}
	return off
}

func (r *Reader) Offset() int64 {
	off, err := r.r.Seek(0, 1)
	if err != nil {
		log.Fatalf("seeking in output [0, 1]: %v", err)
	}
	off -= int64(r.Buffered())
	return off
}

func (w *Writer) Offset() int64 {
	if err := w.Flush(); err != nil {
		log.Fatalf("writing output: %v", err)
	}
	off, err := w.w.Seek(0, 1)
	if err != nil {
		log.Fatalf("seeking in output [0, 1]: %v", err)
	}
	return off
}

func (r *Reader) Close() error {
	if closer, ok := r.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (w *Writer) Close() error {
	err := w.Flush()
	var err1 error
	if closer, ok := w.w.(io.Closer); ok {
		err1 = closer.Close()
	}
	if err == nil {
		err = err1
	}
	return err
}

func (r *Reader) File() *os.File {
	return r.f
}

// Name returns the diagnostic name supplied when the reader was created.
func (r *Reader) Name() string { return r.name }

func (w *Writer) File() *os.File {
	return w.f
}

// Slice reads the next length bytes of r into a slice.
//
// This slice may be backed by mmap'ed memory. Currently, this memory
// will never be unmapped. The second result reports whether the
// backing memory is read-only.
func (r *Reader) Slice(length uint64) ([]byte, bool, error) {
	if length == 0 {
		return []byte{}, false, nil
	}

	data, ok := r.sliceOS(length)
	if ok {
		return data, true, nil
	}

	data = make([]byte, length)
	_, err := io.ReadFull(r, data)
	if err != nil {
		return nil, false, err
	}
	return data, false, nil
}

// SliceRO returns a slice containing the next length bytes of r
// backed by a read-only mmap'd data. If the mmap cannot be
// established (limit exceeded, region too small, etc) a nil slice
// will be returned. If mmap succeeds, it will never be unmapped.
func (r *Reader) SliceRO(length uint64) []byte {
	data, ok := r.sliceOS(length)
	if ok {
		return data
	}
	return nil
}
