// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"go/constant"
	"strconv"
	"strings"
	"sync"

	"cmd/compile/internal/base"
)

// BuiltinPkg is a fake package that declares the universe block.
var BuiltinPkg *Pkg

// LocalPkg is the package being compiled.
var LocalPkg *Pkg

// UnsafePkg is package unsafe.
var UnsafePkg *Pkg

// BlankSym is the blank (_) symbol.
var BlankSym *Sym

// OrigSym returns the original symbol written by the user.
func OrigSym(s *Sym) *Sym {
	if s == nil {
		return nil
	}

	if len(s.Name) > 1 && s.Name[0] == '~' {
		switch s.Name[1] {
		case 'r': // originally an unnamed result
			return nil
		case 'b': // originally the blank identifier _
			// TODO(mdempsky): Does s.Pkg matter here?
			return BlankSym
		}
		return s
	}

	if strings.HasPrefix(s.Name, ".anon") {
		// originally an unnamed or _ name (see subr.go: NewFuncParams)
		return nil
	}

	return s
}

// numImport tracks how often a package with a given name is imported.
// It is used to provide a better error message (by using the package
// path to disambiguate) if a package that appears multiple times with
// the same name appears in an error message.
var NumImport = make(map[string]int)

// fmtMode represents the kind of printing being done.
// The default is regular Go syntax (fmtGo).
// fmtDebug is like fmtGo but for debugging dumps and prints the type kind too.
// fmtTypeID and fmtTypeIDName are for generating various unique representations
// of types used in hashes, the linker, and function/method instantiations.
type fmtMode int

const (
	fmtGo fmtMode = iota
	fmtDebug
	fmtTypeID
	fmtTypeIDName
)

// Sym

// Format implements formatting for a Sym.
// The valid formats are:
//
//	%v	Go syntax: Name for symbols in the local package, PkgName.Name for imported symbols.
//	%+v	Debug syntax: always include PkgName. prefix even for local names.
//	%S	Short syntax: Name only, no matter what.
//
func (s *Sym) Format(f fmt.State, verb rune) {
	mode := fmtGo
	switch verb {
	case 'v', 'S':
		if verb == 'v' && f.Flag('+') {
			mode = fmtDebug
		}
		fmt.Fprint(f, sconv(s, verb, mode))

	default:
		fmt.Fprintf(f, "%%!%c(*types.Sym=%p)", verb, s)
	}
}

func (s *Sym) String() string {
	return sconv(s, 0, fmtGo)
}

// See #16897 for details about performance implications
// before changing the implementation of sconv.
func sconv(s *Sym, verb rune, mode fmtMode) string {
	if verb == 'L' {
		panic("linksymfmt")
	}

	if s == nil {
		return "<S>"
	}

	q := pkgqual(s.Pkg, verb, mode)
	if q == "" {
		return s.Name
	}

	buf := fmtBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer fmtBufferPool.Put(buf)

	buf.WriteString(q)
	buf.WriteByte('.')
	buf.WriteString(s.Name)
	return InternString(buf.Bytes())
}

func sconv2(b *bytes.Buffer, s *Sym, verb rune, mode fmtMode) {
	if verb == 'L' {
		panic("linksymfmt")
	}
	if s == nil {
		b.WriteString("<S>")
		return
	}

	symfmt(b, s, verb, mode)
}

func symfmt(b *bytes.Buffer, s *Sym, verb rune, mode fmtMode) {
	if q := pkgqual(s.Pkg, verb, mode); q != "" {
		b.WriteString(q)
		b.WriteByte('.')
	}
	b.WriteString(s.Name)
}

// pkgqual returns the qualifier that should be used for printing
// symbols from the given package in the given mode.
// If it returns the empty string, no qualification is needed.
func pkgqual(pkg *Pkg, verb rune, mode fmtMode) string {
	if verb != 'S' {
		switch mode {
		case fmtGo: // This is for the user
			if pkg == BuiltinPkg || pkg == LocalPkg {
				return ""
			}

			// If the name was used by multiple packages, display the full path,
			if pkg.Name != "" && NumImport[pkg.Name] > 1 {
				return strconv.Quote(pkg.Path)
			}
			return pkg.Name

		case fmtDebug:
			return pkg.Name

		case fmtTypeIDName:
			// dcommontype, typehash
			return pkg.Name

		case fmtTypeID:
			// (methodsym), typesym, weaksym
			return pkg.Prefix
		}
	}

	return ""
}

// Type

var BasicTypeNames = []string{
	TINT:        "int",
	TUINT:       "uint",
	TINT8:       "int8",
	TUINT8:      "uint8",
	TINT16:      "int16",
	TUINT16:     "uint16",
	TINT32:      "int32",
	TUINT32:     "uint32",
	TINT64:      "int64",
	TUINT64:     "uint64",
	TUINTPTR:    "uintptr",
	TFLOAT32:    "float32",
	TFLOAT64:    "float64",
	TCOMPLEX64:  "complex64",
	TCOMPLEX128: "complex128",
	TBOOL:       "bool",
	TANY:        "any",
	TSTRING:     "string",
	TNIL:        "nil",
	TIDEAL:      "untyped number",
	TBLANK:      "blank",
}

var fmtBufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// Format implements formatting for a Type.
// The valid formats are:
//
//	%v	Go syntax
//	%+v	Debug syntax: Go syntax with a KIND- prefix for all but builtins.
//	%L	Go syntax for underlying type if t is named
//	%S	short Go syntax: drop leading "func" in function type
//	%-S	special case for method receiver symbol
//
func (t *Type) Format(s fmt.State, verb rune) {
	mode := fmtGo
	switch verb {
	case 'v', 'S', 'L':
		if verb == 'v' && s.Flag('+') { // %+v is debug format
			mode = fmtDebug
		}
		if verb == 'S' && s.Flag('-') { // %-S is special case for receiver - short typeid format
			mode = fmtTypeID
		}
		fmt.Fprint(s, tconv(t, verb, mode))
	default:
		fmt.Fprintf(s, "%%!%c(*Type=%p)", verb, t)
	}
}

// String returns the Go syntax for the type t.
func (t *Type) String() string {
	return tconv(t, 0, fmtGo)
}

// LinkString returns an unexpanded string description of t, suitable
// for use in link symbols. "Unexpanded" here means that the
// description uses `"".` to qualify identifiers from the current
// package, and "expansion" refers to the renaming step performed by
// the linker to replace these qualifiers with proper `path/to/pkg.`
// qualifiers.
//
// After expansion, the description corresponds to type identity. That
// is, for any pair of types t1 and t2, Identical(t1, t2) and
// expand(t1.LinkString()) == expand(t2.LinkString()) report the same
// value.
//
// Within a single compilation unit, LinkString always returns the
// same unexpanded description for identical types. Thus it's safe to
// use as a map key to implement a type-identity-keyed map. However,
// make sure all LinkString calls used for this purpose happen within
// the same compile process; the string keys are not stable across
// multiple processes.
func (t *Type) LinkString() string {
	return tconv(t, 0, fmtTypeID)
}

// NameString generates a user-readable, mostly unique string
// description of t. NameString always returns the same description
// for identical types, even across compilation units.
//
// NameString qualifies identifiers by package name, so it has
// collisions when different packages share the same names and
// identifiers. It also does not distinguish function-scope defined
// types from package-scoped defined types or from each other.
func (t *Type) NameString() string {
	return tconv(t, 0, fmtTypeIDName)
}

func tconv(t *Type, verb rune, mode fmtMode) string {
	buf := fmtBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer fmtBufferPool.Put(buf)

	tconv2(buf, t, verb, mode, nil)
	return InternString(buf.Bytes())
}

// tconv2 writes a string representation of t to b.
// flag and mode control exactly what is printed.
// Any types x that are already in the visited map get printed as @%d where %d=visited[x].
// See #16897 before changing the implementation of tconv.
func tconv2(b *bytes.Buffer, t *Type, verb rune, mode fmtMode, visited map[*Type]int) {
	if off, ok := visited[t]; ok {
		// We've seen this type before, so we're trying to print it recursively.
		// Print a reference to it instead.
		fmt.Fprintf(b, "@%d", off)
		return
	}
	if t == nil {
		b.WriteString("<T>")
		return
	}
	if t.Kind() == TSSA {
		b.WriteString(t.extra.(string))
		return
	}
	if t.Kind() == TTUPLE {
		b.WriteString(t.FieldType(0).String())
		b.WriteByte(',')
		b.WriteString(t.FieldType(1).String())
		return
	}

	if t.Kind() == TRESULTS {
		tys := t.extra.(*Results).Types
		for i, et := range tys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(et.String())
		}
		return
	}

	if t == ByteType || t == RuneType {
		// in %-T mode collapse rune and byte with their originals.
		switch mode {
		case fmtTypeIDName, fmtTypeID:
			t = Types[t.Kind()]
		default:
			sconv2(b, t.Sym(), 'S', mode)
			return
		}
	}
	if t == ErrorType {
		b.WriteString("error")
		return
	}

	// Unless the 'L' flag was specified, if the type has a name, just print that name.
	if verb != 'L' && t.Sym() != nil && t != Types[t.Kind()] {
		// Default to 'v' if verb is invalid.
		if verb != 'S' {
			verb = 'v'
		}

		// In unified IR, function-scope defined types will have a ·N
		// suffix embedded directly in their Name. Trim this off for
		// non-fmtTypeID modes.
		sym := t.Sym()
		if mode != fmtTypeID {
			i := len(sym.Name)
			for i > 0 && sym.Name[i-1] >= '0' && sym.Name[i-1] <= '9' {
				i--
			}
			const dot = "·"
			if i >= len(dot) && sym.Name[i-len(dot):i] == dot {
				sym = &Sym{Pkg: sym.Pkg, Name: sym.Name[:i-len(dot)]}
			}
		}
		sconv2(b, sym, verb, mode)

		// TODO(mdempsky): Investigate including Vargen in fmtTypeIDName
		// output too. It seems like it should, but that mode is currently
		// used in string representation used by reflection, which is
		// user-visible and doesn't expect this.
		if mode == fmtTypeID && t.vargen != 0 {
			fmt.Fprintf(b, "·%d", t.vargen)
		}
		return
	}

	if int(t.Kind()) < len(BasicTypeNames) && BasicTypeNames[t.Kind()] != "" {
		var name string
		switch t {
		case UntypedBool:
			name = "untyped bool"
		case UntypedString:
			name = "untyped string"
		case UntypedInt:
			name = "untyped int"
		case UntypedRune:
			name = "untyped rune"
		case UntypedFloat:
			name = "untyped float"
		case UntypedComplex:
			name = "untyped complex"
		default:
			name = BasicTypeNames[t.Kind()]
		}
		b.WriteString(name)
		return
	}

	if mode == fmtDebug {
		b.WriteString(t.Kind().String())
		b.WriteByte('-')
		tconv2(b, t, 'v', fmtGo, visited)
		return
	}

	// At this point, we might call tconv2 recursively. Add the current type to the visited list so we don't
	// try to print it recursively.
	// We record the offset in the result buffer where the type's text starts. This offset serves as a reference
	// point for any later references to the same type.
	// Note that we remove the type from the visited map as soon as the recursive call is done.
	// This prevents encoding types like map[*int]*int as map[*int]@4. (That encoding would work,
	// but I'd like to use the @ notation only when strictly necessary.)
	if visited == nil {
		visited = map[*Type]int{}
	}
	visited[t] = b.Len()
	defer delete(visited, t)

	switch t.Kind() {
	case TPTR:
		b.WriteByte('*')
		switch mode {
		case fmtTypeID, fmtTypeIDName:
			if verb == 'S' {
				tconv2(b, t.Elem(), 'S', mode, visited)
				return
			}
		}
		tconv2(b, t.Elem(), 'v', mode, visited)

	case TARRAY:
		b.WriteByte('[')
		b.WriteString(strconv.FormatInt(t.NumElem(), 10))
		b.WriteByte(']')
		tconv2(b, t.Elem(), 0, mode, visited)

	case TSLICE:
		b.WriteString("[]")
		tconv2(b, t.Elem(), 0, mode, visited)

	case TCHAN:
		switch t.ChanDir() {
		case Crecv:
			b.WriteString("<-chan ")
			tconv2(b, t.Elem(), 0, mode, visited)
		case Csend:
			b.WriteString("chan<- ")
			tconv2(b, t.Elem(), 0, mode, visited)
		default:
			b.WriteString("chan ")
			if t.Elem() != nil && t.Elem().IsChan() && t.Elem().Sym() == nil && t.Elem().ChanDir() == Crecv {
				b.WriteByte('(')
				tconv2(b, t.Elem(), 0, mode, visited)
				b.WriteByte(')')
			} else {
				tconv2(b, t.Elem(), 0, mode, visited)
			}
		}

	case TMAP:
		b.WriteString("map[")
		tconv2(b, t.Key(), 0, mode, visited)
		b.WriteByte(']')
		tconv2(b, t.Elem(), 0, mode, visited)

	case TINTER:
		if t.IsEmptyInterface() {
			b.WriteString("interface {}")
			break
		}
		b.WriteString("interface {")
		for i, f := range t.AllMethods().Slice() {
			if i != 0 {
				b.WriteByte(';')
			}
			b.WriteByte(' ')
			switch {
			case f.Sym == nil:
				// Check first that a symbol is defined for this type.
				// Wrong interface definitions may have types lacking a symbol.
				break
			case IsExported(f.Sym.Name):
				sconv2(b, f.Sym, 'S', mode)
			default:
				if mode != fmtTypeIDName {
					mode = fmtTypeID
				}
				sconv2(b, f.Sym, 'v', mode)
			}
			tconv2(b, f.Type, 'S', mode, visited)
		}
		if t.AllMethods().Len() != 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('}')

	case TFUNC:
		if verb == 'S' {
			// no leading func
		} else {
			if t.Recv() != nil {
				b.WriteString("method")
				tconv2(b, t.Recvs(), 0, mode, visited)
				b.WriteByte(' ')
			}
			b.WriteString("func")
		}
		if t.NumTParams() > 0 {
			tconv2(b, t.TParams(), 0, mode, visited)
		}
		tconv2(b, t.Params(), 0, mode, visited)

		switch t.NumResults() {
		case 0:
			// nothing to do

		case 1:
			b.WriteByte(' ')
			tconv2(b, t.Results().Field(0).Type, 0, mode, visited) // struct->field->field's type

		default:
			b.WriteByte(' ')
			tconv2(b, t.Results(), 0, mode, visited)
		}

	case TSTRUCT:
		if m := t.StructType().Map; m != nil {
			mt := m.MapType()
			// Format the bucket struct for map[x]y as map.bucket[x]y.
			// This avoids a recursive print that generates very long names.
			switch t {
			case mt.Bucket:
				b.WriteString("map.bucket[")
			case mt.Hmap:
				b.WriteString("map.hdr[")
			case mt.Hiter:
				b.WriteString("map.iter[")
			default:
				base.Fatalf("unknown internal map type")
			}
			tconv2(b, m.Key(), 0, mode, visited)
			b.WriteByte(']')
			tconv2(b, m.Elem(), 0, mode, visited)
			break
		}

		if funarg := t.StructType().Funarg; funarg != FunargNone {
			open, close := '(', ')'
			if funarg == FunargTparams {
				open, close = '[', ']'
			}
			b.WriteByte(byte(open))
			fieldVerb := 'v'
			switch mode {
			case fmtTypeID, fmtTypeIDName, fmtGo:
				// no argument names on function signature, and no "noescape"/"nosplit" tags
				fieldVerb = 'S'
			}
			for i, f := range t.Fields().Slice() {
				if i != 0 {
					b.WriteString(", ")
				}
				fldconv(b, f, fieldVerb, mode, visited, funarg)
			}
			b.WriteByte(byte(close))
		} else {
			b.WriteString("struct {")
			for i, f := range t.Fields().Slice() {
				if i != 0 {
					b.WriteByte(';')
				}
				b.WriteByte(' ')
				fldconv(b, f, 'L', mode, visited, funarg)
			}
			if t.NumFields() != 0 {
				b.WriteByte(' ')
			}
			b.WriteByte('}')
		}

	case TFORW:
		b.WriteString("undefined")
		if t.Sym() != nil {
			b.WriteByte(' ')
			sconv2(b, t.Sym(), 'v', mode)
		}

	case TUNSAFEPTR:
		b.WriteString("unsafe.Pointer")

	case TTYPEPARAM:
		if t.Sym() != nil {
			sconv2(b, t.Sym(), 'v', mode)
		} else {
			b.WriteString("tp")
			// Print out the pointer value for now to disambiguate type params
			b.WriteString(fmt.Sprintf("%p", t))
		}

	case TUNION:
		for i := 0; i < t.NumTerms(); i++ {
			if i > 0 {
				b.WriteString("|")
			}
			elem, tilde := t.Term(i)
			if tilde {
				b.WriteString("~")
			}
			tconv2(b, elem, 0, mode, visited)
		}

	case Txxx:
		b.WriteString("Txxx")

	default:
		// Don't know how to handle - fall back to detailed prints
		b.WriteString(t.Kind().String())
		b.WriteString(" <")
		sconv2(b, t.Sym(), 'v', mode)
		b.WriteString(">")

	}
}

func fldconv(b *bytes.Buffer, f *Field, verb rune, mode fmtMode, visited map[*Type]int, funarg Funarg) {
	if f == nil {
		b.WriteString("<T>")
		return
	}

	var name string
	if verb != 'S' {
		s := f.Sym

		// Take the name from the original.
		if mode == fmtGo {
			s = OrigSym(s)
		}

		if s != nil && f.Embedded == 0 {
			if funarg != FunargNone {
				name = fmt.Sprint(f.Nname)
			} else if verb == 'L' {
				name = s.Name
				if name == ".F" {
					name = "F" // Hack for toolstash -cmp.
				}
				if !IsExported(name) && mode != fmtTypeIDName {
					name = sconv(s, 0, mode) // qualify non-exported names (used on structs, not on funarg)
				}
			} else {
				name = sconv(s, 0, mode)
			}
		}
	}

	if name != "" {
		b.WriteString(name)
		b.WriteString(" ")
	}

	if f.IsDDD() {
		var et *Type
		if f.Type != nil {
			et = f.Type.Elem()
		}
		b.WriteString("...")
		tconv2(b, et, 0, mode, visited)
	} else {
		tconv2(b, f.Type, 0, mode, visited)
	}

	if verb != 'S' && funarg == FunargNone && f.Note != "" {
		b.WriteString(" ")
		b.WriteString(strconv.Quote(f.Note))
	}
}

// Val

func FmtConst(v constant.Value, sharp bool) string {
	if !sharp && v.Kind() == constant.Complex {
		real, imag := constant.Real(v), constant.Imag(v)

		var re string
		sre := constant.Sign(real)
		if sre != 0 {
			re = real.String()
		}

		var im string
		sim := constant.Sign(imag)
		if sim != 0 {
			im = imag.String()
		}

		switch {
		case sre == 0 && sim == 0:
			return "0"
		case sre == 0:
			return im + "i"
		case sim == 0:
			return re
		case sim < 0:
			return fmt.Sprintf("(%s%si)", re, im)
		default:
			return fmt.Sprintf("(%s+%si)", re, im)
		}
	}

	return v.String()
}

// TypeHash computes a hash value for type t to use in type switch statements.
func TypeHash(t *Type) uint32 {
	p := t.NameString()

	// Using MD5 is overkill, but reduces accidental collisions.
	h := md5.Sum([]byte(p))
	return binary.LittleEndian.Uint32(h[:4])
}
