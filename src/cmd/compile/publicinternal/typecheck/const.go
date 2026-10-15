// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"fmt"
	"go/constant"
	"go/token"
	"math"
	"math/big"
	"unicode"

	"cmd/compile/publicinternal/ir"
	"cmd/compile/publicinternal/types"
	"cmd/publicinternal/src"
)

func roundFloat(v constant.Value, sz int64) constant.Value {
	return roundFloatFor(nil, nil, v, sz)
}

func roundFloatFor(target *ir.Package, typ *types.Type, v constant.Value, sz int64) constant.Value {
	switch sz {
	case 4:
		f, _ := constant.Float32Val(v)
		return makeFloat64For(target, typ, float64(f))
	case 8:
		f, _ := constant.Float64Val(v)
		return makeFloat64For(target, typ, f)
	}
	panic(fmt.Sprintf("unexpected size: %v", sz))
}

// truncate float literal fv to 32-bit or 64-bit precision
// according to type; return truncated value.
func truncfltlit(v constant.Value, t *types.Type) constant.Value {
	return truncfltlitFor(nil, v, t)
}

func truncfltlitFor(target *ir.Package, v constant.Value, t *types.Type) constant.Value {
	if t.IsUntyped() {
		return v
	}

	return roundFloatFor(target, t, v, t.Size())
}

// truncate Real and Imag parts of Mpcplx to 32-bit or 64-bit
// precision, according to type; return truncated value. In case of
// overflow, calls Errorf but does not truncate the input value.
func trunccmplxlit(v constant.Value, t *types.Type) constant.Value {
	return trunccmplxlitFor(nil, v, t)
}

func trunccmplxlitFor(target *ir.Package, v constant.Value, t *types.Type) constant.Value {
	if t.IsUntyped() {
		return v
	}

	fsz := t.Size() / 2
	return makeComplex(roundFloatFor(target, t, constant.Real(v), fsz), roundFloatFor(target, t, constant.Imag(v), fsz))
}

// TODO(mdempsky): Replace these with better APIs.
func convlit(n ir.Node, t *types.Type) ir.Node { return convlit1(nil, n, t, false, nil) }

// DefaultLit uses the legacy target-free basic type table. New compiler code
// should use DefaultLitFor so default types come from the active target.
func DefaultLit(n ir.Node, t *types.Type) ir.Node { return convlit1(nil, n, t, false, nil) }

// DefaultLitFor converts an untyped expression using target's basic types.
func DefaultLitFor(target *ir.Package, n ir.Node, t *types.Type) ir.Node {
	return convlit1(target, n, t, false, nil)
}

// convlit1 converts an untyped expression n to type t. If n already
// has a type, convlit1 has no effect.
//
// For explicit conversions, t must be non-nil, and integer-to-string
// conversions are allowed.
//
// For implicit conversions (e.g., assignments), t may be nil; if so,
// n is converted to its default type.
//
// If there's an error converting n to t, context is used in the error
// message.
func convlit1(target *ir.Package, n ir.Node, t *types.Type, explicit bool, context func() string) ir.Node {
	if explicit && t == nil {
		fatalf(target, "explicit conversion missing type")
	}
	if t != nil && t.IsUntyped() {
		fatalf(target, "bad conversion to untyped: %v", t)
	}

	if n == nil || n.Type() == nil {
		// Allow sloppy callers.
		return n
	}
	if !n.Type().IsUntyped() {
		// Already typed; nothing to do.
		return n
	}

	// Nil is technically not a constant, so handle it specially.
	if n.Type().Kind() == types.TNIL {
		if n.Op() != ir.ONIL {
			fatalf(target, "unexpected op: %v (%v)", n, n.Op())
		}
		n = ir.Copy(n)
		if t == nil {
			fatalf(target, "use of untyped nil")
		}

		if !t.HasNil() {
			// Leave for caller to handle.
			return n
		}

		n.SetType(t)
		return n
	}

	if t == nil || !ir.OKForConst[t.Kind()] {
		t = defaultType(target, n.Type())
	}

	switch n.Op() {
	default:
		fatalf(target, "unexpected untyped expression: %v", n)

	case ir.OLITERAL:
		v := ConvertValFor(target, n.Val(), t, explicit)
		if v.Kind() == constant.Unknown {
			n = ir.NewConstExpr(n.Val(), n)
			break
		}
		n = ir.NewConstExpr(v, n)
		n.SetType(t)
		return n

	case ir.OPLUS, ir.ONEG, ir.OBITNOT, ir.ONOT, ir.OREAL, ir.OIMAG:
		ot := operandType(n.Op(), t)
		if ot == nil {
			n = convlit1(target, n, nil, false, nil)
			break
		}

		n := n.(*ir.UnaryExpr)
		n.X = convlit1(target, n.X, ot, false, nil)
		if n.X.Type() == nil {
			n.SetType(nil)
			return n
		}
		n.SetType(t)
		return n

	case ir.OADD, ir.OSUB, ir.OMUL, ir.ODIV, ir.OMOD, ir.OOR, ir.OXOR, ir.OAND, ir.OANDNOT, ir.OOROR, ir.OANDAND, ir.OCOMPLEX:
		ot := operandType(n.Op(), t)
		if ot == nil {
			n = convlit1(target, n, nil, false, nil)
			break
		}

		var l, r ir.Node
		switch n := n.(type) {
		case *ir.BinaryExpr:
			n.X = convlit1(target, n.X, ot, false, nil)
			n.Y = convlit1(target, n.Y, ot, false, nil)
			l, r = n.X, n.Y
		case *ir.LogicalExpr:
			n.X = convlit1(target, n.X, ot, false, nil)
			n.Y = convlit1(target, n.Y, ot, false, nil)
			l, r = n.X, n.Y
		}

		if l.Type() == nil || r.Type() == nil {
			n.SetType(nil)
			return n
		}
		if !types.Identical(l.Type(), r.Type()) {
			errorf(target, "invalid operation: %v (mismatched types %v and %v)", n, l.Type(), r.Type())
			n.SetType(nil)
			return n
		}

		n.SetType(t)
		return n

	case ir.OEQ, ir.ONE, ir.OLT, ir.OLE, ir.OGT, ir.OGE:
		n := n.(*ir.BinaryExpr)
		if !t.IsBoolean() {
			break
		}
		n.SetType(t)
		return n

	case ir.OLSH, ir.ORSH:
		n := n.(*ir.BinaryExpr)
		n.X = convlit1(target, n.X, t, explicit, nil)
		n.SetType(n.X.Type())
		if n.Type() != nil && !n.Type().IsInteger() {
			errorf(target, "invalid operation: %v (shift of type %v)", n, n.Type())
			n.SetType(nil)
		}
		return n
	}

	if explicit {
		fatalf(target, "cannot convert %L to type %v", n, t)
	} else if context != nil {
		fatalf(target, "cannot use %L as type %v in %s", n, t, context())
	} else {
		fatalf(target, "cannot use %L as type %v", n, t)
	}

	n.SetType(nil)
	return n
}

func operandType(op ir.Op, t *types.Type) *types.Type {
	switch op {
	case ir.OCOMPLEX:
		if t.IsComplex() {
			return types.FloatForComplex(t)
		}
	case ir.OREAL, ir.OIMAG:
		if t.IsFloat() {
			return types.ComplexForFloat(t)
		}
	default:
		if okfor[op][t.Kind()] {
			return t
		}
	}
	return nil
}

// ConvertVal converts v into a representation appropriate for t. If
// no such representation exists, it returns constant.MakeUnknown()
// instead.
//
// If explicit is true, then conversions from integer to string are
// also allowed.
func ConvertVal(v constant.Value, t *types.Type, explicit bool) constant.Value {
	return ConvertValFor(nil, v, t, explicit)
}

// ConvertValFor converts v into a representation appropriate for t and
// reports conversion diagnostics through target.
func ConvertValFor(target *ir.Package, v constant.Value, t *types.Type, explicit bool) constant.Value {
	switch ct := v.Kind(); ct {
	case constant.Bool:
		if t.IsBoolean() {
			return v
		}

	case constant.String:
		if t.IsString() {
			return v
		}

	case constant.Int:
		if explicit && t.IsString() {
			return tostr(v)
		}
		fallthrough
	case constant.Float, constant.Complex:
		switch {
		case t.IsInteger():
			v = tointFor(target, t, v)
			return v
		case t.IsFloat():
			v = toflt(v)
			v = truncfltlitFor(target, v, t)
			return v
		case t.IsComplex():
			v = tocplx(v)
			v = trunccmplxlitFor(target, v, t)
			return v
		}
	}

	return constant.MakeUnknown()
}

func tocplx(v constant.Value) constant.Value {
	return constant.ToComplex(v)
}

func toflt(v constant.Value) constant.Value {
	if v.Kind() == constant.Complex {
		v = constant.Real(v)
	}

	return constant.ToFloat(v)
}

func toint(v constant.Value) constant.Value {
	return tointFor(nil, nil, v)
}

func tointFor(target *ir.Package, typ *types.Type, v constant.Value) constant.Value {
	if v.Kind() == constant.Complex {
		v = constant.Real(v)
	}

	if v := constant.ToInt(v); v.Kind() == constant.Int {
		return v
	}

	// The value of v cannot be represented as an integer;
	// so we need to print an error message.
	// Unfortunately some float values cannot be
	// reasonably formatted for inclusion in an error
	// message (example: 1 + 1e-100), so first we try to
	// format the float; if the truncation resulted in
	// something that looks like an integer we omit the
	// value from the error message.
	// (See issue #11371).
	f := ir.BigFloat(v)
	if f.MantExp(nil) > 2*ir.ConstPrec {
		errorfFor(target, typ, "integer too large")
	} else {
		var t big.Float
		t.Parse(fmt.Sprint(v), 0)
		if t.IsInt() {
			errorfFor(target, typ, "constant truncated to integer")
		} else {
			errorfFor(target, typ, "constant %v truncated to integer", v)
		}
	}

	// Prevent follow-on errors.
	return constant.MakeUnknown()
}

func tostr(v constant.Value) constant.Value {
	if v.Kind() == constant.Int {
		r := unicode.ReplacementChar
		if x, ok := constant.Uint64Val(v); ok && x <= unicode.MaxRune {
			r = rune(x)
		}
		v = constant.MakeString(string(r))
	}
	return v
}

func makeFloat64(f float64) constant.Value {
	return makeFloat64For(nil, nil, f)
}

func makeFloat64For(target *ir.Package, typ *types.Type, f float64) constant.Value {
	if math.IsInf(f, 0) {
		fatalfFor(target, typ, src.NoXPos, "infinity is not a valid constant")
	}
	return constant.MakeFloat64(f)
}

func makeComplex(real, imag constant.Value) constant.Value {
	return constant.BinaryOp(constant.ToFloat(real), token.ADD, constant.MakeImag(constant.ToFloat(imag)))
}

// DefaultLit on both nodes simultaneously;
// if they're both ideal going in they better
// get the same type going out.
// force means must assign concrete (non-ideal) type.
// The results of defaultlit2 MUST be assigned back to l and r, e.g.
//
//	n.Left, n.Right = defaultlit2(n.Left, n.Right, force)
func defaultlit2(target *ir.Package, l ir.Node, r ir.Node, force bool) (ir.Node, ir.Node) {
	if l.Type() == nil || r.Type() == nil {
		return l, r
	}

	if !l.Type().IsInterface() && !r.Type().IsInterface() {
		// Can't mix bool with non-bool, string with non-string.
		if l.Type().IsBoolean() != r.Type().IsBoolean() {
			return l, r
		}
		if l.Type().IsString() != r.Type().IsString() {
			return l, r
		}
	}

	if !l.Type().IsUntyped() {
		r = convlit1(target, r, l.Type(), false, nil)
		return l, r
	}

	if !r.Type().IsUntyped() {
		l = convlit1(target, l, r.Type(), false, nil)
		return l, r
	}

	if !force {
		return l, r
	}

	// Can't mix nil with anything untyped.
	if ir.IsNil(l) || ir.IsNil(r) {
		return l, r
	}
	t := defaultType(target, mixUntyped(l.Type(), r.Type()))
	l = convlit1(target, l, t, false, nil)
	r = convlit1(target, r, t, false, nil)
	return l, r
}

func mixUntyped(t1, t2 *types.Type) *types.Type {
	if t1 == t2 {
		return t1
	}

	rank := func(t *types.Type) int {
		switch t {
		case types.UntypedInt:
			return 0
		case types.UntypedRune:
			return 1
		case types.UntypedFloat:
			return 2
		case types.UntypedComplex:
			return 3
		}
		fatalfType(t, src.NoXPos, "bad type %v", t)
		panic("unreachable")
	}

	if rank(t2) > rank(t1) {
		return t2
	}
	return t1
}

func defaultType(target *ir.Package, t *types.Type) *types.Type {
	if !t.IsUntyped() || t.Kind() == types.TNIL {
		return t
	}

	context := t.Context()
	if target != nil {
		context = target.TypeContext
	}
	basic := func(kind types.Kind) *types.Type {
		if target != nil {
			return target.Type(kind)
		}
		return context.Types[kind]
	}

	switch t {
	case types.UntypedBool:
		return basic(types.TBOOL)
	case types.UntypedString:
		return basic(types.TSTRING)
	case types.UntypedInt:
		return basic(types.TINT)
	case types.UntypedRune:
		return context.RuneType
	case types.UntypedFloat:
		return basic(types.TFLOAT64)
	case types.UntypedComplex:
		return basic(types.TCOMPLEX128)
	}

	fatalfType(t, t.Pos(), "bad type %v", t)
	return nil
}

func basicType(target *ir.Package, kind types.Kind) *types.Type {
	if target != nil {
		return target.Type(kind)
	}
	return types.Types[kind]
}

// IndexConst returns the index value of constant Node n.
func IndexConst(n ir.Node) int64 {
	return IndexConstFor(nil, n)
}

// IndexConstFor returns the index value of constant Node n using target's int type.
func IndexConstFor(target *ir.Package, n ir.Node) int64 {
	var typ *types.Type
	if target != nil {
		typ = target.Type(types.TINT)
	} else if n.Type() != nil {
		typ = n.Type().Context().Types[types.TINT]
	} else {
		typ = types.Types[types.TINT]
	}
	return ir.IntVal(typ, toint(n.Val()))
}

// callOrChan reports whether n is a call or channel operation.
func callOrChan(n ir.Node) bool {
	switch n.Op() {
	case ir.OAPPEND,
		ir.OCALL,
		ir.OCALLFUNC,
		ir.OCALLINTER,
		ir.OCALLMETH,
		ir.OCAP,
		ir.OCLEAR,
		ir.OCLOSE,
		ir.OCOMPLEX,
		ir.OCOPY,
		ir.ODELETE,
		ir.OIMAG,
		ir.OLEN,
		ir.OMAKE,
		ir.OMAX,
		ir.OMIN,
		ir.ONEW,
		ir.OPANIC,
		ir.OPRINT,
		ir.OPRINTLN,
		ir.OREAL,
		ir.ORECOVER,
		ir.ORECV,
		ir.OUNSAFEADD,
		ir.OUNSAFESLICE,
		ir.OUNSAFESLICEDATA,
		ir.OUNSAFESTRING,
		ir.OUNSAFESTRINGDATA:
		return true
	}
	return false
}
