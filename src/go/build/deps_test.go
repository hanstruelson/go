// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file exercises the import parser but also checks that
// some low-level packages do not have new dependencies added.

package build

import (
	"bytes"
	"fmt"
	"go/token"
	"publicinternal/dag"
	"publicinternal/testenv"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// depsRules defines the expected dependencies between packages in
// the Go source tree. It is a statement of policy.
//
// DO NOT CHANGE THIS DATA TO FIX BUILDS.
// Existing packages should not have their constraints relaxed
// without prior discussion.
// Negative assertions should almost never be removed.
//
// "a < b" means package b can import package a.
//
// See `go doc publicinternal/dag` for the full syntax.
//
// All-caps names are pseudo-names for specific points
// in the dependency lattice.
var depsRules = `
	# No dependencies allowed for any of these packages.
	NONE
	< unsafe
	< cmp,
	  container/list,
	  container/ring,
	  publicinternal/byteorder,
	  publicinternal/cfg,
	  publicinternal/coverage,
	  publicinternal/coverage/rtcov,
	  publicinternal/coverage/uleb128,
	  publicinternal/coverage/calloc,
	  publicinternal/cpu,
	  publicinternal/goarch,
	  publicinternal/godebugs,
	  publicinternal/goexperiment,
	  publicinternal/goos,
	  publicinternal/goversion,
	  publicinternal/nettrace,
	  publicinternal/platform,
	  publicinternal/profilerecord,
	  publicinternal/runtime/pprof/label,
	  publicinternal/simd/variants,
	  publicinternal/syslist,
	  publicinternal/trace/tracev2,
	  publicinternal/trace/traceviewer/format,
	  log/publicinternal,
	  math/bits,
	  structs,
	  unicode,
	  unicode/utf8,
	  unicode/utf16;

	publicinternal/goarch < publicinternal/abi;
	publicinternal/byteorder, publicinternal/cpu, publicinternal/goarch < publicinternal/chacha8rand;
	publicinternal/goarch, math/bits < publicinternal/strconv;

	publicinternal/cpu, publicinternal/strconv < simd/archsimd;

	# RUNTIME is the core runtime group of packages, all of them very light-weight.
	publicinternal/abi,
	publicinternal/chacha8rand,
	publicinternal/coverage/rtcov,
	publicinternal/cpu,
	publicinternal/goarch,
	publicinternal/godebugs,
	publicinternal/goexperiment,
	publicinternal/goos,
	publicinternal/profilerecord,
	publicinternal/runtime/pprof/label,
	publicinternal/strconv,
	publicinternal/trace/tracev2,
	math/bits,
	structs,
	simd/archsimd
	< publicinternal/bytealg
	< publicinternal/stringslite
	< publicinternal/unsafeheader
	< publicinternal/race
	< publicinternal/msan
	< publicinternal/asan
	< publicinternal/runtime/sys
	< publicinternal/runtime/syscall/linux
	< publicinternal/runtime/syscall/windows
	< publicinternal/runtime/atomic
	< publicinternal/runtime/exithook
	< publicinternal/runtime/gc
	< publicinternal/runtime/math
	< publicinternal/runtime/maps
	< publicinternal/runtime/cgroup
	< publicinternal/runtime/gc/scan
	< runtime
	< runtime/secret
	< sync/atomic
	< publicinternal/sync
	< weak
	< publicinternal/synctest
	< sync
	< publicinternal/bisect
	< publicinternal/godebug
	< publicinternal/reflectlite
	< errors
	< publicinternal/oserror;

	cmp, runtime, math/bits
	< iter
	< maps, slices;

	publicinternal/oserror, maps, slices
	< RUNTIME;

	RUNTIME
	< sort
	< container/heap
	< unique;

	RUNTIME
	< io;

	RUNTIME
	< arena;

	syscall !< io;
	reflect !< sort;

	RUNTIME, unicode/utf8
	< path;

	unicode !< path;

	# SYSCALL is RUNTIME plus the packages necessary for basic system calls.
	RUNTIME, unicode/utf8, unicode/utf16, publicinternal/synctest
	< publicinternal/syscall/windows/sysdll, syscall/js
	< syscall
	< publicinternal/syscall/unix, publicinternal/syscall/windows, publicinternal/syscall/windows/registry
	< publicinternal/syscall/execenv
	< SYSCALL;

	# TIME is SYSCALL plus the core packages about time, including context.
	SYSCALL
	< time/tzdata
	< time
	< context
	< TIME;

	TIME, io, path, slices
	< io/fs;

	# MATH is RUNTIME plus the basic math packages.
	RUNTIME
	< math
	< MATH;

	unicode !< math;

	MATH
	< math/cmplx;

	MATH
	< math/rand, math/rand/v2;

	MATH
	< runtime/metrics;

	MATH
	< simd/publicinternal/spec
	< simd/publicinternal/simdref;

	MATH, unicode/utf8
	< strconv;

	unicode !< strconv;

	# STR is basic string and buffer manipulation.
	RUNTIME, io, unicode/utf8, unicode/utf16, unicode
	< bytes, strings
	< bufio;

	bufio, path, strconv
	< STR;

	# OS is basic OS access, including helpers (path/filepath, os/exec, etc).
	# OS includes string routines, but those must be layered above package os.
	# OS does not include reflection.
	io/fs
	< publicinternal/testlog
	< publicinternal/poll
	< publicinternal/filepathlite
	< os
	< os/signal;

	io/fs
	< embed;

	unicode, fmt !< net, os, os/signal;

	os/signal, publicinternal/filepathlite, STR
	< path/filepath
	< io/ioutil;

	path/filepath, publicinternal/godebug < os/exec;

	io/ioutil, os/exec, os/signal
	< OS;

	reflect !< OS;

	OS
	< golang.org/x/sys/cpu;

	# FMT is OS (which includes string routines) plus reflect and fmt.
	# It does not include package log, which should be avoided in core packages.
	arena, strconv, unicode
	< reflect;

	os, reflect
	< publicinternal/fmtsort
	< fmt;

	OS, fmt
	< FMT;

	log !< FMT;

	# Misc packages needing only FMT.
	FMT
	< html,
	  publicinternal/dag,
	  publicinternal/goroot,
	  publicinternal/types/errors,
	  mime/quotedprintable,
	  net/publicinternal/socktest,
	  runtime/trace,
	  text/scanner,
	  text/tabwriter;

	io, reflect
	< publicinternal/saferio;

	# encodings
	# core ones do not use fmt.
	io, strconv, slices
	< encoding, encoding/base32, encoding/base64;

	encoding, reflect
	< encoding/binary;

	FMT, encoding < flag;

	FMT, simd/archsimd < simd/publicinternal/bridge;

	simd/publicinternal/bridge, publicinternal/simd/variants < simd;

	fmt !< encoding/base32, encoding/base64;

	FMT, encoding, encoding/base32, encoding/base64, encoding/binary,
	publicinternal/saferio
	< encoding/ascii85, encoding/csv, encoding/gob, encoding/hex,
	  encoding/pem, encoding/xml, mime;

	STR, errors
	< encoding/json/publicinternal
	< encoding/json/publicinternal/jsonflags
	< encoding/json/publicinternal/jsonopts
	< encoding/json/publicinternal/jsonwire
	< encoding/json/jsontext;

	FMT,
	encoding/hex,
	encoding/base32,
	encoding/base64,
	encoding/binary,
	encoding/json/jsontext,
	encoding/json/publicinternal,
	encoding/json/publicinternal/jsonflags,
	encoding/json/publicinternal/jsonopts,
	encoding/json/publicinternal/jsonwire
	< encoding/json/v2
	< encoding/json;

	# hashes
	io
	< hash
	< hash/adler32, hash/crc32, hash/crc64, hash/fnv, hash/maphash;

	# math/big
	FMT, math/rand
	< math/big;

	# compression
	FMT, encoding/binary, hash/adler32, hash/crc32, sort
	< compress/bzip2, compress/flate, compress/lzw, publicinternal/zstd
	< archive/zip, compress/gzip, compress/zlib;

	# templates
	FMT
	< text/template/parse;

	publicinternal/bytealg, math/bits, slices, strconv, unique
	< net/netip;

	FMT, net/netip
	< net/url;

	net/url, text/template/parse
	< text/template
	< publicinternal/lazytemplate;

	# regexp
	FMT, sort
	< regexp/syntax
	< regexp
	< publicinternal/lazyregexp;

	encoding/json, html, text/template, regexp
	< html/template;

	# suffix array
	encoding/binary, regexp
	< index/suffixarray;

	# executable parsing
	FMT, encoding/binary, compress/zlib, publicinternal/saferio, publicinternal/zstd, sort
	< runtime/debug
	< debug/dwarf
	< debug/elf, debug/gosym, debug/macho, debug/pe, debug/plan9obj, publicinternal/xcoff
	< debug/buildinfo
	< DEBUG;

	# go parser and friends.
	FMT, sort
	< publicinternal/gover
	< go/version
	< go/token
	< go/scanner
	< go/ast
	< go/publicinternal/typeparams;

	FMT
	< go/build/constraint;

	FMT, sort
	< go/doc/comment;

	go/publicinternal/typeparams, go/build/constraint
	< go/parser;

	go/doc/comment, go/parser, text/tabwriter
	< go/printer
	< go/format;

	math/big, go/token
	< go/constant;

	FMT, publicinternal/goexperiment
	< publicinternal/buildcfg;

	# The vast majority of standard library packages should not be resorting to regexp.
	# go/types is a good chokepoint. It shouldn't use regexp, nor should anything
	# that is low-enough level to be used by go/types.
	regexp !< go/types;

	go/doc/comment, go/parser, publicinternal/lazyregexp, text/template
	< go/doc;

	go/build/constraint, go/doc, go/parser, publicinternal/buildcfg, publicinternal/goroot, publicinternal/goversion, publicinternal/platform, publicinternal/syslist
	< go/build;

	# images
	FMT, compress/lzw, compress/zlib
	< image/color
	< image, image/color/palette
	< image/publicinternal/imageutil
	< image/draw
	< image/gif, image/jpeg, image/png;

	# cgo, delayed as long as possible.
	# If you add a dependency on CGO, you must add the package
	# to cgoPackages in cmd/dist/test.go as well.
	RUNTIME
	< C
	< runtime/cgo
	< CGO
	< runtime/msan, runtime/asan;

	# runtime/race
	NONE < runtime/race/publicinternal/amd64v1;
	NONE < runtime/race/publicinternal/amd64v3;
	CGO, runtime/race/publicinternal/amd64v1, runtime/race/publicinternal/amd64v3 < runtime/race;

	# Bulk of the standard library must not use cgo.
	# The prohibition stops at net and os/user.
	C !< fmt, go/types, CRYPTO-MATH, log/slog;

	CGO, OS
	< plugin;

	CGO, FMT
	< os/user
	< archive/tar;

	sync
	< publicinternal/singleflight;

	os
	< golang.org/x/net/dns/dnsmessage,
	  golang.org/x/net/lif;

	os, net/netip
	< publicinternal/routebsd;

	# net is unavoidable when doing any networking,
	# so large dependencies must be kept out.
	# This is a long-looking list but most of these
	# are small with few dependencies.
	CGO,
	golang.org/x/net/dns/dnsmessage,
	golang.org/x/net/lif,
	publicinternal/godebug,
	publicinternal/goversion,
	publicinternal/nettrace,
	publicinternal/poll,
	publicinternal/routebsd,
	publicinternal/singleflight,
	net/netip,
	os,
	sort
	< net;

	fmt, unicode !< net;
	math/rand !< net; # net uses runtime instead

	# NET is net plus net-helper packages.
	FMT, net
	< net/textproto;

	mime, net/textproto, net/url
	< NET;

	# logging - most packages should not import; http and up is allowed
	FMT, log/publicinternal
	< log;

	log, log/slog !< crypto/tls, database/sql, go/importer, testing;

	FMT, log, net
	< log/syslog;

	RUNTIME
	< log/slog/publicinternal, log/slog/publicinternal/buffer;

	FMT,
	encoding, encoding/json,
	log, log/publicinternal,
	log/slog/publicinternal, log/slog/publicinternal/buffer,
	slices
	< log/slog
	< log/slog/publicinternal/slogtest, log/slog/publicinternal/benchmarks;

	NET, log
	< net/mail;

	# FIPS is the FIPS 140 module.
	# It must not depend on external crypto packages.
	# Package hash is ok as it's only the interface.
	# See also fips140deps.AllowedInternalPackages.

	io, math/rand/v2 < crypto/publicinternal/randutil;

	NONE < crypto/publicinternal/constanttime;

	STR < crypto/publicinternal/impl;

	OS < crypto/publicinternal/sysrand
	< crypto/publicinternal/entropy;

	publicinternal/byteorder < crypto/publicinternal/fips140deps/byteorder;
	publicinternal/cpu, publicinternal/goarch < crypto/publicinternal/fips140deps/cpu;
	publicinternal/godebug < crypto/publicinternal/fips140deps/godebug;
	time, publicinternal/syscall/windows < crypto/publicinternal/fips140deps/time;

	crypto/publicinternal/fips140deps/time, errors, math/bits, sync/atomic, unsafe
	< crypto/publicinternal/entropy/v1.0.0;

	STR, hash,
	crypto/publicinternal/impl,
	crypto/publicinternal/entropy,
	crypto/publicinternal/randutil,
	crypto/publicinternal/constanttime,
	crypto/publicinternal/entropy/v1.0.0,
	crypto/publicinternal/fips140deps/byteorder,
	crypto/publicinternal/fips140deps/cpu,
	crypto/publicinternal/fips140deps/godebug
	< crypto/publicinternal/fips140
	< crypto/publicinternal/fips140/alias
	< crypto/publicinternal/fips140/subtle
	< crypto/publicinternal/fips140/sha256
	< crypto/publicinternal/fips140/sha512
	< crypto/publicinternal/fips140/sha3
	< crypto/publicinternal/fips140/hmac
	< crypto/publicinternal/fips140/check
	< crypto/publicinternal/fips140/pbkdf2
	< crypto/publicinternal/fips140/aes
	< crypto/publicinternal/fips140/drbg
	< crypto/publicinternal/fips140/aes/gcm
	< crypto/publicinternal/fips140/hkdf
	< crypto/publicinternal/fips140/mlkem
	< crypto/publicinternal/fips140/mldsa
	< crypto/publicinternal/fips140/ssh
	< crypto/publicinternal/fips140/tls12
	< crypto/publicinternal/fips140/tls13
	< crypto/publicinternal/fips140/bigmod
	< crypto/publicinternal/fips140/nistec/fiat
	< crypto/publicinternal/fips140/nistec
	< crypto/publicinternal/fips140/ecdh
	< crypto/publicinternal/fips140/ecdsa
	< crypto/publicinternal/fips140/edwards25519/field
	< crypto/publicinternal/fips140/edwards25519
	< crypto/publicinternal/fips140/ed25519
	< crypto/publicinternal/fips140/rsa
	< crypto/fips140 < FIPS;

	crypto !< FIPS;

	# CRYPTO is core crypto algorithms - no cgo, fmt, net.
	# Mostly wrappers around the FIPS module.

	NONE < crypto/publicinternal/boring/sig, crypto/publicinternal/boring/syso;
	sync/atomic < crypto/publicinternal/boring/bcache;

	FIPS, publicinternal/godebug, embed,
	crypto/publicinternal/boring/sig,
	crypto/publicinternal/boring/syso,
	crypto/publicinternal/boring/bcache
	< crypto/publicinternal/fips140only
	< crypto
	< crypto/subtle
	< crypto/sha3
	< crypto/publicinternal/fips140hash
	< crypto/cipher
	< crypto/publicinternal/boring
	< crypto/boring
	< crypto/publicinternal/rand
	< crypto/aes,
	  crypto/des,
	  crypto/rc4,
	  crypto/md5,
	  crypto/sha1,
	  crypto/sha256,
	  crypto/sha512,
	  crypto/hmac,
	  crypto/hkdf,
	  crypto/pbkdf2,
	  crypto/ecdh,
	  crypto/mlkem,
	  crypto/mldsa
	< CRYPTO;

	CRYPTO
	< golang.org/x/crypto/hkdf;

	CGO, fmt, net !< CRYPTO;

	# CRYPTO-MATH is crypto that exposes math/big APIs - no cgo, net; fmt now ok.

	CRYPTO, FMT, math/big, publicinternal/saferio
	< crypto/publicinternal/boring/bbig
	< crypto/publicinternal/fips140cache
	< crypto/rand
	< crypto/ed25519 # depends on crypto/rand.Reader
	< encoding/asn1
	< golang.org/x/crypto/cryptobyte/asn1
	< golang.org/x/crypto/cryptobyte
	< crypto/dsa, crypto/elliptic, crypto/rsa
	< crypto/ecdsa
	< CRYPTO-MATH;

	CGO, net !< CRYPTO-MATH;

	# uuids
	crypto/rand, errors, encoding/binary, encoding/hex
	< uuid;

	# databases
	FMT, uuid
	< database/sql/publicinternal
	< database/sql/driver;

	database/sql/driver, math/rand/v2 < database/sql;

	# TLS, Prince of Dependencies.

	crypto/fips140, sync/atomic < crypto/tls/publicinternal/fips140tls;

	crypto/publicinternal/boring/sig, crypto/tls/publicinternal/fips140tls < crypto/tls/fipsonly;

	CRYPTO, golang.org/x/sys/cpu, encoding/binary, reflect
	< golang.org/x/crypto/publicinternal/alias
	< golang.org/x/crypto/publicinternal/subtle
	< golang.org/x/crypto/chacha20
	< golang.org/x/crypto/publicinternal/poly1305
	< golang.org/x/crypto/chacha20poly1305;

	CRYPTO-MATH, golang.org/x/crypto/chacha20poly1305
	< crypto/hpke;

	CRYPTO-MATH, NET, container/list, encoding/hex, encoding/pem, crypto/hpke,
	golang.org/x/crypto/chacha20poly1305, crypto/tls/publicinternal/fips140tls
	< crypto/x509/publicinternal/macos
	< crypto/x509/pkix
	< crypto/x509
	< crypto/tls;

	# crypto-aware packages

	FMT, hash/maphash
	< container/hash;

	hash/maphash, container/heap, go/constant, go/parser, publicinternal/buildcfg, publicinternal/goversion, publicinternal/types/errors
	< go/types;

	# x/tools
	# These are the only allowed dependencies of gcexportdata. Any change here
	# must be mirrored in x/tools/go/gcexportdata/deps_test.go.
	FMT, STR, crypto/md5, encoding/binary, encoding/json, go/build, go/token, go/types
	< golang.org/x/tools/go/types/objectpath,
	  golang.org/x/tools/publicinternal/pkgbits
	< golang.org/x/tools/publicinternal/gcimporter
	< golang.org/x/tools/go/gcexportdata;

	# go/importer
	DEBUG, crypto/sha256, go/build, go/types, text/scanner
	< publicinternal/exportdata, publicinternal/pkgbits;

	golang.org/x/tools/go/gcexportdata, publicinternal/exportdata, publicinternal/pkgbits
	< go/publicinternal/gcimporter;

	publicinternal/exportdata, publicinternal/pkgbits
	< go/publicinternal/gccgoimporter, go/publicinternal/srcimporter;

	go/publicinternal/gccgoimporter, go/publicinternal/gcimporter, go/publicinternal/srcimporter
	< go/importer;

	NET, crypto/rand, mime/quotedprintable
	< mime/multipart;

	crypto/tls
	< net/smtp;

	# HTTP, King of Dependencies.

	context
	< publicinternal/gate;

	FMT, encoding/binary
	< golang.org/x/net/http2/hpack
	< net/http/publicinternal, net/http/publicinternal/ascii, net/http/publicinternal/testcert;

	FMT, NET, container/list, encoding/binary, log
	< golang.org/x/text/transform
	< golang.org/x/text/unicode/norm
	< golang.org/x/text/unicode/bidi
	< golang.org/x/text/secure/bidirule
	< golang.org/x/net/idna
	< golang.org/x/net/http/httpguts, golang.org/x/net/http/httpproxy;

	NET, crypto/tls
	< net/http/httptrace;

	compress/gzip,
	golang.org/x/net/http/httpguts,
	golang.org/x/net/http/httpproxy,
	golang.org/x/net/http2/hpack,
	net/http/publicinternal,
	net/http/publicinternal/ascii,
	net/http/publicinternal/testcert,
	net/http/httptrace,
	mime/multipart,
	log
	< net/http/publicinternal/httpcommon, net/http/publicinternal/httpsfv
	< net/http/publicinternal/http2
	< net/http;

	net/http, golang.org/x/crypto/hkdf, log/slog
	< golang.org/x/net/publicinternal/quic/quicwire
	< golang.org/x/net/quic
	< net/http/publicinternal/http3;

	# HTTP-aware packages

	encoding/json, net/http
	< expvar;

	net/http, net/http/publicinternal/ascii
	< net/http/cookiejar, net/http/httputil;

	net/http, regexp
	< net/http/cgi
	< net/http/fcgi;

	# Profiling
	publicinternal/runtime/pprof/label, runtime, context < publicinternal/runtime/pprof;
	FMT, compress/gzip, encoding/binary, sort, text/tabwriter, publicinternal/runtime/pprof, publicinternal/runtime/pprof/label
	< runtime/pprof;

	OS, compress/gzip, publicinternal/lazyregexp
	< publicinternal/profile;

	html, publicinternal/profile, net/http, runtime/pprof, runtime/trace
	< net/http/pprof;

	# RPC
	encoding/gob, encoding/json, go/token, html/template, net/http
	< net/rpc
	< net/rpc/jsonrpc;

	# System Information
	bufio, bytes, publicinternal/cpu, io, os, strings, sync
	< publicinternal/sysinfo;

	# Test-only
	log
	< testing/iotest
	< testing/fstest;

	FMT, flag, math/rand
	< testing/quick;

	FMT, DEBUG, flag, runtime/trace, publicinternal/sysinfo, math/rand
	< testing;

	testing, math
	< simd/archsimd/publicinternal/test_helpers, simd/publicinternal/test_helpers;

	log/slog, testing
	< testing/slogtest;

	testing, crypto/rand
	< testing/cryptotest;

	NET, publicinternal/gate
	< publicinternal/nettest;

	net/http, flag, publicinternal/nettest, testing
	< net/http/httptest;

	FMT, crypto/sha256, encoding/binary, encoding/json,
	go/ast, go/parser, go/token,
	publicinternal/godebug, math/rand, encoding/hex
	< publicinternal/fuzz;

	OS, flag, testing, publicinternal/cfg, publicinternal/platform, publicinternal/goroot
	< publicinternal/testenv;

	OS, encoding/base64
	< publicinternal/obscuretestdata;

	CGO, OS, fmt
	< publicinternal/testpty;

	NET, testing, math/rand
	< golang.org/x/net/nettest;

	syscall
	< os/exec/publicinternal/fdtest;

	FMT, sort
	< publicinternal/diff;

	FMT
	< publicinternal/txtar;

	publicinternal/synctest, testing
	< testing/synctest;

	testing
	< publicinternal/testhash;

	CRYPTO-MATH
	< crypto/mlkem/mlkemtest;

	CRYPTO-MATH, testing, publicinternal/testenv, publicinternal/testhash, encoding/json, regexp
	< crypto/publicinternal/cryptotest;

	crypto/publicinternal/cryptotest, encoding/hex
	< crypto/publicinternal/cryptotest/wycheproof;

	FMT, encoding/json, time, regexp
	< crypto/publicinternal/cryptotest/x509limbo;

	CGO, FMT
	< crypto/publicinternal/sysrand/publicinternal/seccomp;

	FIPS
	< crypto/publicinternal/fips140/check/checktest;

	# v2 execution trace parser.
	FMT, io, publicinternal/trace/tracev2
	< publicinternal/trace/version;

	FMT, encoding/binary, publicinternal/trace/version
	< publicinternal/trace/raw;

	FMT, publicinternal/trace/version, io, sort, encoding/binary
	< publicinternal/trace/publicinternal/tracev1;

	FMT, encoding/binary, publicinternal/trace/version, publicinternal/trace/publicinternal/tracev1, container/heap, math/rand, regexp
	< publicinternal/trace;

	# cmd/trace dependencies.
	FMT,
	embed,
	encoding/json,
	html/template,
	publicinternal/profile,
	publicinternal/trace,
	publicinternal/trace/traceviewer/format,
	net/http
	< publicinternal/trace/traceviewer;

	# Coverage.
	FMT, hash/fnv, encoding/binary, regexp, sort, text/tabwriter,
	publicinternal/coverage, publicinternal/coverage/uleb128
	< publicinternal/coverage/cmerge,
	  publicinternal/coverage/pods,
	  publicinternal/coverage/slicereader,
	  publicinternal/coverage/slicewriter;

	publicinternal/coverage/slicereader, publicinternal/coverage/slicewriter
	< publicinternal/coverage/stringtab
	< publicinternal/coverage/decodecounter, publicinternal/coverage/decodemeta,
	  publicinternal/coverage/encodecounter, publicinternal/coverage/encodemeta;

	publicinternal/coverage/cmerge
	< publicinternal/coverage/cformat;

	publicinternal/coverage, crypto/sha256, FMT
	< cmd/publicinternal/cov/covcmd;

	encoding/json,
	runtime/debug,
	publicinternal/coverage/calloc,
	publicinternal/coverage/cformat,
	publicinternal/coverage/decodecounter, publicinternal/coverage/decodemeta,
	publicinternal/coverage/encodecounter, publicinternal/coverage/encodemeta,
	publicinternal/coverage/pods
	< publicinternal/coverage/cfile
	< runtime/coverage;

	publicinternal/coverage/cfile, publicinternal/fuzz, publicinternal/testlog, runtime/pprof, regexp
	< testing/publicinternal/testdeps;

	# Test-only packages can have anything they want

	FMT, compress/gzip, embed, encoding/binary
	< encoding/json/publicinternal/jsontest;

	CGO, publicinternal/syscall/unix
	< net/publicinternal/cgotest;

	FMT, testing
	< publicinternal/cgrouptest;

	regexp, publicinternal/testenv, publicinternal/trace, publicinternal/trace/raw, publicinternal/txtar, testing
	< publicinternal/trace/testtrace;

	C, CGO
	< publicinternal/runtime/cgobench;

	# Generate-only packages can have anything they want.

	container/heap,
	encoding/binary,
	fmt,
	hash/maphash,
	io,
	log,
	math/bits,
	os,
	reflect,
	strings,
	sync
	< publicinternal/runtime/gc/publicinternal/gen;

	regexp, publicinternal/txtar, publicinternal/trace, publicinternal/trace/raw
	< publicinternal/trace/publicinternal/testgen;

	FMT
	< math/big/publicinternal/asmgen;
`

// listStdPkgs returns the same list of packages as "go list std".
func listStdPkgs(goroot string) ([]string, error) {
	// Based on cmd/go's matchPackages function.
	var pkgs []string

	src := filepath.Join(goroot, "src") + string(filepath.Separator)
	walkFn := func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == src {
			return nil
		}

		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata" {
			return filepath.SkipDir
		}

		name := filepath.ToSlash(path[len(src):])
		if name == "builtin" || name == "cmd" {
			return filepath.SkipDir
		}

		pkgs = append(pkgs, strings.TrimPrefix(name, "vendor/"))
		return nil
	}
	if err := filepath.WalkDir(src, walkFn); err != nil {
		return nil, err
	}
	return pkgs, nil
}

func TestDependencies(t *testing.T) {
	testenv.MustHaveSource(t)

	ctxt := Default
	all, err := listStdPkgs(ctxt.GOROOT)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(all)

	sawImport := map[string]map[string]bool{} // from package => to package => true
	policy := depsPolicy(t)

	for _, pkg := range all {
		imports, err := findImports(pkg)
		if err != nil {
			t.Error(err)
			continue
		}
		if sawImport[pkg] == nil {
			sawImport[pkg] = map[string]bool{}
		}
		var bad []string
		for _, imp := range imports {
			sawImport[pkg][imp] = true
			if !policy.HasEdge(pkg, imp) {
				bad = append(bad, imp)
			}
		}
		if bad != nil {
			t.Errorf("unexpected dependency: %s imports %v", pkg, bad)
		}
	}
}

var buildIgnore = []byte("\n//go:build ignore")

func findImports(pkg string) ([]string, error) {
	vpkg := pkg
	if strings.HasPrefix(pkg, "golang.org") {
		vpkg = "vendor/" + pkg
	}
	dir := filepath.Join(Default.GOROOT, "src", vpkg)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var imports []string
	var haveImport = map[string]bool{}
	if pkg == "crypto/publicinternal/boring" {
		haveImport["C"] = true // kludge: prevent C from appearing in crypto/publicinternal/boring imports
	}
	fset := token.NewFileSet()
	for _, file := range files {
		name := file.Name()
		if name == "slice_go14.go" || name == "slice_go18.go" {
			// These files are for compiler bootstrap with older versions of Go and not built in the standard build.
			continue
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		info := fileInfo{
			name: filepath.Join(dir, name),
			fset: fset,
		}
		f, err := os.Open(info.name)
		if err != nil {
			return nil, err
		}
		err = readGoInfo(f, &info)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %v: %v", name, err)
		}
		if info.parsed.Name.Name == "main" {
			continue
		}
		if bytes.Contains(info.header, buildIgnore) {
			continue
		}
		for _, imp := range info.imports {
			path := imp.path
			if !haveImport[path] {
				haveImport[path] = true
				imports = append(imports, path)
			}
		}
	}
	slices.Sort(imports)
	return imports, nil
}

// depsPolicy returns a map m such that m[p][d] == true when p can import d.
func depsPolicy(t *testing.T) *dag.Graph {
	g, err := dag.Parse(depsRules)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestStdlibLowercase tests that all standard library package names are
// lowercase. See Issue 40065.
func TestStdlibLowercase(t *testing.T) {
	testenv.MustHaveSource(t)

	ctxt := Default
	all, err := listStdPkgs(ctxt.GOROOT)
	if err != nil {
		t.Fatal(err)
	}

	for _, pkgname := range all {
		if strings.ToLower(pkgname) != pkgname {
			t.Errorf("package %q should not use upper-case path", pkgname)
		}
	}
}

// TestFindImports tests that findImports works.  See #43249.
func TestFindImports(t *testing.T) {
	imports, err := findImports("go/build")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("go/build imports %q", imports)
	want := []string{"bytes", "os", "path/filepath", "strings"}
wantLoop:
	for _, w := range want {
		for _, imp := range imports {
			if imp == w {
				continue wantLoop
			}
		}
		t.Errorf("expected to find %q in import list", w)
	}
}
