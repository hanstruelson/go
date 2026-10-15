// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"bytes"
	"io"
	"publicinternal/goexperiment"

	"cmd/publicinternal/bio"
)

func WriteExports(ctx *Context, out *bio.Writer) {
	var data bytes.Buffer

	if goexperiment.GoListExportNewFormat {
		data.WriteByte('p')
	} else {
		data.WriteByte('u')
	}
	writeUnifiedExport(ctx, &data)

	// The linker also looks for the $$ marker - use char after $$ to distinguish format.
	out.WriteString("\n$$B\n") // indicate binary export format
	io.Copy(out, &data)
	out.WriteString("\n$$\n")

	if ctx.Target.Debug.Export != 0 {
		ctx.Target.Diagnostics.Printf("BenchmarkExportSize:%s 1 %d bytes\n", ctx.Target.Ctxt.Pkgpath, data.Len())
	}
}
