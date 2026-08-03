// Package jsontext validates a JSON document for parser-differential hazards
// BEFORE any format adapter decodes it (the approved known-format
// parse-bypass seam, round 2):
//
//   - valid UTF-8 only (Go's decoder silently replaces invalid bytes);
//   - well-formed string escapes: literal escaped `\\u...` stays literal,
//     paired high+low surrogates are accepted, lone halves are rejected
//     (Go's decoder normalizes lone surrogates to U+FFFD);
//   - unique DECODED member names in every object, recursively, so
//     `"messages"` and `"\u006dessages"` cannot be interpreted as two
//     different fields by different parsers;
//   - exactly one top-level JSON value.
//
// It deliberately does NOT enforce the full JSON grammar: number tokens are
// scanned leniently and unknown members are left to the adapter, so the
// validator can only ever REJECT a document that a strict parser would also
// reject (or that is a listed hazard). It must never accept a document a
// format adapter would reject — a malformed number like `1-2` is consumed as
// one token and the adapter's own decode rejects it, keeping the fail-closed
// 400 path without inventing a new differential.
package jsontext

import (
	"errors"
	"unicode/utf8"
)

const maxDepth = 10000 // matches encoding/json's decoder bound

// ErrTooDeep reports a document exceeding maxDepth.
var ErrTooDeep = errors.New("jsontext: document exceeds maximum nesting depth")

// Validate reports whether data is free of the parser-differential hazards
// above. A nil return means the document is UTF-8 clean, structurally
// walkable, duplicate-free, single-valued, and surrogate-safe; it does NOT
// mean the document is valid JSON by every grammar (see the package comment).
func Validate(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("jsontext: invalid UTF-8")
	}
	v := &validator{data: data}
	v.skipWS()
	if v.pos >= len(v.data) {
		return errors.New("jsontext: empty document")
	}
	if err := v.value(0); err != nil {
		return err
	}
	v.skipWS()
	if v.pos != len(v.data) {
		return errors.New("jsontext: trailing data after the top-level value")
	}
	return nil
}

type validator struct {
	data []byte
	pos  int
	// scopes is the object-member-name stack: one map per OPEN object, so
	// duplicate detection is scoped to the object being parsed and a nested
	// object can reuse its parents' member names. Each map is allocated once
	// per object (linear in document size, O(1) per key).
	scopes []map[string]struct{}
	// buf is a reusable scratch buffer for string decoding; only object keys
	// are retained, so value strings allocate nothing.
	buf []byte
}

func (v *validator) skipWS() {
	for v.pos < len(v.data) {
		switch v.data[v.pos] {
		case ' ', '\t', '\n', '\r':
			v.pos++
		default:
			return
		}
	}
}

func (v *validator) value(depth int) error {
	if depth > maxDepth {
		return ErrTooDeep
	}
	if v.pos >= len(v.data) {
		return errors.New("jsontext: unexpected end of document")
	}
	switch c := v.data[v.pos]; {
	case c == '{':
		return v.object(depth + 1)
	case c == '[':
		return v.array(depth + 1)
	case c == '"':
		_, err := v.decodeString()
		return err
	case c == 't':
		return v.literal("true")
	case c == 'f':
		return v.literal("false")
	case c == 'n':
		return v.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return v.number()
	default:
		return errors.New("jsontext: unexpected character")
	}
}

func (v *validator) literal(want string) error {
	if v.pos+len(want) > len(v.data) || string(v.data[v.pos:v.pos+len(want)]) != want {
		return errors.New("jsontext: malformed literal")
	}
	v.pos += len(want)
	return nil
}

// number consumes a LENIENT JSON number token: it starts with `-` or a digit
// and continues over digits, dots, exponent markers and signs. Anything the
// token swallows that is not a real number is still rejected by the adapter's
// strict decode afterwards — the validator never becomes the accepting side
// of a differential.
func (v *validator) number() error {
	if v.data[v.pos] == '-' {
		v.pos++
	}
	digits := 0
	for v.pos < len(v.data) {
		c := v.data[v.pos]
		if c >= '0' && c <= '9' {
			digits++
			v.pos++
			continue
		}
		if c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			v.pos++
			continue
		}
		break
	}
	if digits == 0 && v.data[v.pos-1] == '-' {
		return errors.New("jsontext: malformed number")
	}
	return nil
}

func (v *validator) array(depth int) error {
	v.pos++ // '['
	v.skipWS()
	if v.pos < len(v.data) && v.data[v.pos] == ']' {
		v.pos++
		return nil
	}
	for {
		if err := v.value(depth); err != nil {
			return err
		}
		v.skipWS()
		if v.pos >= len(v.data) {
			return errors.New("jsontext: unterminated array")
		}
		switch v.data[v.pos] {
		case ',':
			v.pos++
			v.skipWS()
		case ']':
			v.pos++
			return nil
		default:
			return errors.New("jsontext: expected ',' or ']'")
		}
	}
}

func (v *validator) object(depth int) error {
	v.pos++ // '{'
	v.skipWS()
	scope := make(map[string]struct{})
	v.scopes = append(v.scopes, scope)
	if v.pos < len(v.data) && v.data[v.pos] == '}' {
		v.pos++
		v.scopes = v.scopes[:len(v.scopes)-1]
		return nil
	}
	for {
		if v.pos >= len(v.data) || v.data[v.pos] != '"' {
			return errors.New("jsontext: expected an object member name")
		}
		key, err := v.decodeString()
		if err != nil {
			return err
		}
		if _, dup := scope[string(key)]; dup {
			return errors.New("jsontext: duplicate object member name")
		}
		scope[string(key)] = struct{}{}
		v.skipWS()
		if v.pos >= len(v.data) || v.data[v.pos] != ':' {
			return errors.New("jsontext: expected ':' after member name")
		}
		v.pos++
		v.skipWS()
		if err := v.value(depth); err != nil {
			return err
		}
		v.skipWS()
		if v.pos >= len(v.data) {
			return errors.New("jsontext: unterminated object")
		}
		switch v.data[v.pos] {
		case ',':
			v.pos++
			v.skipWS()
		case '}':
			v.pos++
			v.scopes = v.scopes[:len(v.scopes)-1]
			return nil
		default:
			return errors.New("jsontext: expected ',' or '}'")
		}
	}
}

// decodeString consumes a JSON string starting at v.data[v.pos] == '"' and
// returns its DECODED bytes. Escapes are validated: control characters must
// be escaped, `\uXXXX` must hold hex digits, a high surrogate must be
// immediately followed by an escaped low surrogate, and a lone low surrogate
// is rejected. A literal `\\u...` (escaped backslash) is ordinary text.
func (v *validator) decodeString() ([]byte, error) {
	if v.data[v.pos] != '"' {
		return nil, errors.New("jsontext: expected string")
	}
	v.pos++
	v.buf = v.buf[:0]
	for {
		if v.pos >= len(v.data) {
			return nil, errors.New("jsontext: unterminated string")
		}
		c := v.data[v.pos]
		switch {
		case c == '"':
			v.pos++
			return v.buf, nil
		case c == '\\':
			v.pos++
			if v.pos >= len(v.data) {
				return nil, errors.New("jsontext: unterminated escape")
			}
			e := v.data[v.pos]
			v.pos++
			switch e {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				v.buf = append(v.buf, e)
			case 'u':
				r, err := v.hexRune()
				if err != nil {
					return nil, err
				}
				if r >= 0xD800 && r <= 0xDBFF {
					// High surrogate: must pair with an escaped low surrogate.
					if v.pos+1 >= len(v.data) || v.data[v.pos] != '\\' || v.data[v.pos+1] != 'u' {
						return nil, errors.New("jsontext: lone high surrogate")
					}
					v.pos += 2
					lo, err := v.hexRune()
					if err != nil {
						return nil, err
					}
					if lo < 0xDC00 || lo > 0xDFFF {
						return nil, errors.New("jsontext: high surrogate not followed by a low surrogate")
					}
					combined := 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
					v.buf = utf8.AppendRune(v.buf, rune(combined))
				} else if r >= 0xDC00 && r <= 0xDFFF {
					return nil, errors.New("jsontext: lone low surrogate")
				} else {
					v.buf = utf8.AppendRune(v.buf, r)
				}
			default:
				return nil, errors.New("jsontext: invalid escape")
			}
		case c < 0x20:
			return nil, errors.New("jsontext: unescaped control character")
		default:
			v.buf = append(v.buf, c)
			v.pos++
		}
	}
}

// hexRune consumes exactly four hex digits and returns their rune value.
func (v *validator) hexRune() (rune, error) {
	if v.pos+4 > len(v.data) {
		return 0, errors.New("jsontext: truncated \\u escape")
	}
	r := rune(0)
	for i := 0; i < 4; i++ {
		d := hexDigit(v.data[v.pos+i])
		if d < 0 {
			return 0, errors.New("jsontext: invalid \\u escape")
		}
		r = r<<4 | rune(d)
	}
	v.pos += 4
	return r, nil
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return -1
	}
}
