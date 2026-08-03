package engine

// Authoritative JSON wrappers for the eight request byte fields
// (revision-4 design, reassessment-approved three-wrapper model).
//
// The ABI inventory needs exactly three durable types:
//
//	OptionalJSONObject   — content_parts-free object fields that may be
//	                       absent (message/tool cache-control,
//	                       provider_extensions_json, torana_meta_json)
//	RequiredJSONObject   — tool-call arguments_json and tool-def
//	                       parameters_json: never absent, canonical `{}`
//	OptionalJSONArray    — content_parts_json, safety_settings_json
//
// Contract (shared by all three):
//   - the bytes are IMMUTABLE by construction: constructors and accessors
//     copy, and views are read-only by contract and copies in fact;
//   - every byte sequence is validated with the strict shared JSON-text
//     rules (valid UTF-8, paired surrogates, unique decoded member names,
//     exactly one top-level value — pb/v2/jsontext) before it can become
//     or remain the authority; mutations re-validate and leave the
//     receiver unchanged on error;
//   - SHAPE IS TYPE-LEVEL: an OptionalJSONObject can only ever hold an
//     object, an OptionalJSONArray only an array, so Replace cannot change
//     the top-level shape of a field behind its wrapper — including while
//     absent;
//   - PRESENCE IS DURABLE: the zero value of OptionalJSONObject /
//     OptionalJSONArray is ABSENT (with the wrapper's object/array
//     identity intact); the zero value of RequiredJSONObject is the
//     canonical `{}` — the illegal absent-required state is
//     unrepresentable inside the engine;
//   - object mutations splice STRUCTURAL spans (key/value spans, separator
//     spans, closing-brace offset), so untouched members keep their exact
//     lexemes, key order, and whitespace (1e999, large integers,
//     non-alphabetical order, and whitespace-only empty objects survive;
//     set-then-delete round trips are byte-exact);
//   - member keys must be valid UTF-8: an invalid key is refused, never
//     silently rewritten to U+FFFD;
//   - DecodeObject/DecodeArray return exact-lexeme copies.
//
// PB conversion constructs every field through these validating
// constructors only — there is no unvalidated route into an authority.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	pbjsontext "github.com/torana-edge/torana-plugin-sdk/pb/v2/jsontext"
)

// jsonBytes is the shared unexported validated byte/span authority. nil
// bytes mean ABSENT (only legal for the optional wrappers). The top-level
// shape is a property of the wrapper type, not of this value.
type jsonBytes struct {
	bytes []byte
}

func (b jsonBytes) absent() bool { return b.bytes == nil }
func (b jsonBytes) copyBytes() []byte {
	if b.bytes == nil {
		return nil
	}
	out := make([]byte, len(b.bytes))
	copy(out, b.bytes)
	return out
}
func (b jsonBytes) String() string {
	if b.bytes == nil {
		return ""
	}
	return string(b.bytes)
}

// validateMemberKey is the ONE UTF-8 key rule for set/delete on every
// wrapper: an invalid key is refused, never silently rewritten to U+FFFD.
// It runs BEFORE any absent/zero no-op, so the rule is state-independent.
func validateMemberKey(key string) error {
	if !utf8.ValidString(key) {
		return fmt.Errorf("raw JSON member key is not valid UTF-8")
	}
	return nil
}

// validateObject validates raw as a strict JSON object. Empty bytes are an
// error unless allowAbsent (optional wrappers treat them as absent).
func validateObject(raw []byte, allowAbsent bool) ([]byte, error) {
	if len(raw) == 0 {
		if allowAbsent {
			return nil, nil
		}
		return nil, fmt.Errorf("raw JSON: empty bytes are not a JSON object")
	}
	if err := pbjsontext.Validate(raw); err != nil {
		return nil, fmt.Errorf("raw JSON: %w", err)
	}
	v, err := decodeUseNumber(raw)
	if err != nil {
		return nil, fmt.Errorf("raw JSON: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("raw JSON must be a JSON object")
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

func validateArray(raw []byte, allowAbsent bool) ([]byte, error) {
	if len(raw) == 0 {
		if allowAbsent {
			return nil, nil
		}
		return nil, fmt.Errorf("raw JSON: empty bytes are not a JSON array")
	}
	if err := pbjsontext.Validate(raw); err != nil {
		return nil, fmt.Errorf("raw JSON: %w", err)
	}
	v, err := decodeUseNumber(raw)
	if err != nil {
		return nil, fmt.Errorf("raw JSON: %w", err)
	}
	if _, ok := v.([]any); !ok {
		return nil, fmt.Errorf("raw JSON must be a JSON array")
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

func decodeUseNumber(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// OptionalJSONObject
// ---------------------------------------------------------------------------

// OptionalJSONObject is an object authority that may be absent. The zero
// value is ABSENT while retaining object identity: SetMember materializes
// it, DeleteMember is a no-op, and Replace(nil) returns to absence.
type OptionalJSONObject struct {
	v jsonBytes
}

// ParseOptionalJSONObject validates raw as an object authority. Nil or
// empty bytes mean ABSENT.
func ParseOptionalJSONObject(raw []byte) (OptionalJSONObject, error) {
	b, err := validateObject(raw, true)
	if err != nil {
		return OptionalJSONObject{}, err
	}
	return OptionalJSONObject{v: jsonBytes{bytes: b}}, nil
}

// IsAbsent reports whether the object is absent (distinct from the
// canonical empty shape `{}`).
func (o OptionalJSONObject) IsAbsent() bool { return o.v.absent() }

// Bytes returns a defensive copy of the authoritative bytes (nil when
// absent).
func (o OptionalJSONObject) Bytes() []byte { return o.v.copyBytes() }

// String returns the authoritative bytes as a string.
func (o OptionalJSONObject) String() string { return o.v.String() }

// DecodeObject returns a strict ordered view (exact lexemes, copies).
func (o OptionalJSONObject) DecodeObject() (map[string]json.RawMessage, []string, error) {
	if o.v.absent() {
		return nil, nil, fmt.Errorf("raw JSON: absent object has no members")
	}
	return decodeObjectView(o.v.bytes)
}

// Replace validates newBytes as an object and returns a new authority;
// nil/empty returns to ABSENCE. An optional object can never become an
// array. The receiver is unchanged on error.
func (o OptionalJSONObject) Replace(newBytes []byte) (OptionalJSONObject, error) {
	b, err := validateObject(newBytes, true)
	if err != nil {
		return o, err
	}
	return OptionalJSONObject{v: jsonBytes{bytes: b}}, nil
}

// SetMember returns a new authority with member key set to the pre-encoded
// value. On an absent authority the object materializes deterministically
// as `{"k":v}`. See setMember for the splicing rules.
func (o OptionalJSONObject) SetMember(key string, value json.RawMessage) (OptionalJSONObject, error) {
	if err := validateMemberKey(key); err != nil {
		return o, err
	}
	base := o.v.bytes
	if base == nil {
		base = []byte(`{}`)
	}
	out, err := setMember(base, key, value)
	if err != nil {
		return o, err
	}
	return OptionalJSONObject{v: jsonBytes{bytes: out}}, nil
}

// DeleteMember returns a new authority without member key. On an absent
// authority this is a NO-OP (absence has no members to delete).
func (o OptionalJSONObject) DeleteMember(key string) (OptionalJSONObject, error) {
	if err := validateMemberKey(key); err != nil {
		return o, err
	}
	if o.v.absent() {
		return o, nil
	}
	out, err := deleteMember(o.v.bytes, key)
	if err != nil {
		return o, err
	}
	return OptionalJSONObject{v: jsonBytes{bytes: out}}, nil
}

// ---------------------------------------------------------------------------
// RequiredJSONObject
// ---------------------------------------------------------------------------

// RequiredJSONObject is an object authority that is NEVER absent. The zero
// value is the canonical `{}` — the illegal absent-required state is
// unrepresentable inside the engine. Replace(nil) is refused; deleting the
// last member yields the canonical `{}`.
type RequiredJSONObject struct {
	v jsonBytes
}

// ParseRequiredJSONObject validates raw as an object authority. Nil or
// empty bytes are REFUSED: this is the strict asserted-wire constructor.
// Host/provider normalization constructs the zero/canonical-empty wrapper
// instead when nil maps are normalized to `{}`.
func ParseRequiredJSONObject(raw []byte) (RequiredJSONObject, error) {
	b, err := validateObject(raw, false)
	if err != nil {
		return RequiredJSONObject{}, err
	}
	return RequiredJSONObject{v: jsonBytes{bytes: b}}, nil
}

// Bytes returns a defensive copy of the authoritative bytes; the zero
// value yields the canonical `{}`.
func (r RequiredJSONObject) Bytes() []byte {
	if r.v.absent() {
		return []byte(`{}`)
	}
	return r.v.copyBytes()
}

// String returns the authoritative bytes as a string (canonical `{}` for
// the zero value).
func (r RequiredJSONObject) String() string {
	if r.v.absent() {
		return `{}`
	}
	return r.v.String()
}

// DecodeObject returns a strict ordered view; the zero value yields an
// empty object.
func (r RequiredJSONObject) DecodeObject() (map[string]json.RawMessage, []string, error) {
	if r.v.absent() {
		return map[string]json.RawMessage{}, nil, nil
	}
	return decodeObjectView(r.v.bytes)
}

// Replace validates newBytes as an object and returns a new authority.
// nil/empty is REFUSED (requiredness is durable); the top-level shape can
// never change.
func (r RequiredJSONObject) Replace(newBytes []byte) (RequiredJSONObject, error) {
	b, err := validateObject(newBytes, false)
	if err != nil {
		return r, err
	}
	return RequiredJSONObject{v: jsonBytes{bytes: b}}, nil
}

// SetMember returns a new authority with member key set. The zero value
// materializes from the canonical `{}`.
func (r RequiredJSONObject) SetMember(key string, value json.RawMessage) (RequiredJSONObject, error) {
	if err := validateMemberKey(key); err != nil {
		return r, err
	}
	base := r.v.bytes
	if base == nil {
		base = []byte(`{}`)
	}
	out, err := setMember(base, key, value)
	if err != nil {
		return r, err
	}
	return RequiredJSONObject{v: jsonBytes{bytes: out}}, nil
}

// DeleteMember returns a new authority without member key. Deleting the
// last member yields the canonical `{}`; deleting from the zero/empty
// value is a no-op yielding canonical `{}`.
func (r RequiredJSONObject) DeleteMember(key string) (RequiredJSONObject, error) {
	if err := validateMemberKey(key); err != nil {
		return r, err
	}
	if r.v.absent() {
		return r, nil
	}
	out, err := deleteMember(r.v.bytes, key)
	if err != nil {
		return r, err
	}
	// Binding ruling: deleting the last member of a REQUIRED object yields
	// the canonical `{}` — an empty result returns the zero representation
	// (nil bytes materialize `{}`). The optional wrapper keeps its exact
	// preserved spelling because a present empty object is still present.
	if _, order, _, _, err := parseObjectSpans(out); err == nil && len(order) == 0 {
		return RequiredJSONObject{}, nil
	}
	return RequiredJSONObject{v: jsonBytes{bytes: out}}, nil
}

// ---------------------------------------------------------------------------
// OptionalJSONArray
// ---------------------------------------------------------------------------

// OptionalJSONArray is an array authority that may be absent. The zero
// value is ABSENT. Replacement is array-only; there is no member-mutation
// API (arrays are whole-field replaced).
type OptionalJSONArray struct {
	v jsonBytes
}

// ParseOptionalJSONArray validates raw as an array authority. Nil or empty
// bytes mean ABSENT.
func ParseOptionalJSONArray(raw []byte) (OptionalJSONArray, error) {
	b, err := validateArray(raw, true)
	if err != nil {
		return OptionalJSONArray{}, err
	}
	return OptionalJSONArray{v: jsonBytes{bytes: b}}, nil
}

// IsAbsent reports whether the array is absent (distinct from `[]`).
func (a OptionalJSONArray) IsAbsent() bool { return a.v.absent() }

// Bytes returns a defensive copy of the authoritative bytes (nil when
// absent).
func (a OptionalJSONArray) Bytes() []byte { return a.v.copyBytes() }

// String returns the authoritative bytes as a string.
func (a OptionalJSONArray) String() string { return a.v.String() }

// DecodeArray returns a strict view of the array's exact-lexeme elements
// (copies).
func (a OptionalJSONArray) DecodeArray() ([]json.RawMessage, error) {
	if a.v.absent() {
		return nil, fmt.Errorf("raw JSON: absent array has no elements")
	}
	return decodeArrayView(a.v.bytes)
}

// Replace validates newBytes as an array and returns a new authority;
// nil/empty returns to ABSENCE. An optional array can never become an
// object.
func (a OptionalJSONArray) Replace(newBytes []byte) (OptionalJSONArray, error) {
	b, err := validateArray(newBytes, true)
	if err != nil {
		return a, err
	}
	return OptionalJSONArray{v: jsonBytes{bytes: b}}, nil
}

// ---------------------------------------------------------------------------
// Shared views
// ---------------------------------------------------------------------------

func decodeObjectView(raw []byte) (map[string]json.RawMessage, []string, error) {
	vals, order, _, _, err := parseObjectSpans(raw)
	if err != nil {
		return nil, nil, err
	}
	return vals, order, nil
}

func decodeArrayView(raw []byte) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("raw JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("raw JSON must be an array")
	}
	var out []json.RawMessage
	for dec.More() {
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return nil, fmt.Errorf("raw JSON: %w", err)
		}
		out = append(out, append(json.RawMessage(nil), el...))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Shared span editor (object members)
// ---------------------------------------------------------------------------

// memberSpan is one object member's exact structural spans.
type memberSpan struct {
	keyStart int
	valStart int
	valEnd   int
	sepEnd   int
}

// parseObjectSpans strictly parses an object into ordered member values
// (exact lexemes), the wire order, the member spans, and the offset of the
// structural closing brace. The object must already be jsontext-valid.
func parseObjectSpans(raw []byte) (map[string]json.RawMessage, []string, map[string]memberSpan, int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("raw JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, nil, 0, fmt.Errorf("raw JSON must be an object")
	}
	vals := map[string]json.RawMessage{}
	spans := map[string]memberSpan{}
	var order []string
	for dec.More() {
		afterPrev := int(dec.InputOffset())
		p := afterPrev
		for p < len(raw) && isJSONSpace(raw[p]) {
			p++
		}
		if p < len(raw) && raw[p] == ',' {
			p++
			for p < len(raw) && isJSONSpace(raw[p]) {
				p++
			}
		}
		keyStart := p
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("raw JSON: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, nil, 0, fmt.Errorf("raw JSON: object member name is not a string")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, nil, 0, fmt.Errorf("raw JSON: %w", err)
		}
		valEnd := int(dec.InputOffset())
		valStart := valEnd - len(v)
		sepEnd := valEnd
		q := valEnd
		for q < len(raw) && isJSONSpace(raw[q]) {
			q++
		}
		if q < len(raw) && raw[q] == ',' {
			sepEnd = q + 1
			for sepEnd < len(raw) && isJSONSpace(raw[sepEnd]) {
				sepEnd++
			}
		}
		vals[key] = append(json.RawMessage(nil), v...)
		spans[key] = memberSpan{keyStart: keyStart, valStart: valStart, valEnd: valEnd, sepEnd: sepEnd}
		order = append(order, key)
	}
	closeBrace := len(raw) - 1
	if len(order) > 0 {
		last := spans[order[len(order)-1]]
		cb := last.sepEnd
		for cb < len(raw) && isJSONSpace(raw[cb]) {
			cb++
		}
		if cb < len(raw) && raw[cb] == '}' {
			closeBrace = cb
		}
	} else {
		// Empty object: skip leading whitespace to the opening brace, then
		// whitespace to the structural closing brace.
		cb := 0
		for cb < len(raw) && isJSONSpace(raw[cb]) {
			cb++
		}
		cb++ // the '{'
		for cb < len(raw) && isJSONSpace(raw[cb]) {
			cb++
		}
		if cb < len(raw) && raw[cb] == '}' {
			closeBrace = cb
		}
	}
	return vals, order, spans, closeBrace, nil
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// setMember splices member key into the object raw bytes: an existing
// member's VALUE SPAN is replaced in place; a new member is inserted before
// the structural closing brace, preserving inner and trailing whitespace.
// The key must be valid UTF-8; the value must be a strict single JSON
// value. The result is re-validated as an object.
func setMember(raw []byte, key string, value json.RawMessage) ([]byte, error) {
	if err := validateMemberKey(key); err != nil {
		return nil, err
	}
	if err := pbjsontext.Validate(value); err != nil {
		return nil, fmt.Errorf("raw JSON member %q: %w", key, err)
	}
	_, _, spans, closeBrace, err := parseObjectSpans(raw)
	if err != nil {
		return nil, err
	}
	keyBytes, err := json.Marshal(key)
	if err != nil {
		return nil, fmt.Errorf("raw JSON member key: %w", err)
	}
	valueBytes := append(json.RawMessage(nil), value...)

	var out []byte
	if sp, ok := spans[key]; ok {
		out = append(out, raw[:sp.valStart]...)
		out = append(out, valueBytes...)
		out = append(out, raw[sp.valEnd:]...)
	} else {
		// Insert before the structural closing brace — including the
		// empty-object case, so prefix, inner, and trailing whitespace are
		// retained and the only-member deletion restores the original
		// spans byte-exactly.
		base := bytes.TrimRight(raw[:closeBrace], " \t\r\n")
		innerWS := raw[len(base):closeBrace]
		out = append(out, base...)
		if base[len(base)-1] != '{' {
			out = append(out, ',')
		}
		out = append(out, keyBytes...)
		out = append(out, ':')
		out = append(out, valueBytes...)
		out = append(out, innerWS...)
		out = append(out, '}')
		out = append(out, raw[closeBrace+1:]...)
	}
	if _, err := validateObject(out, false); err != nil {
		return nil, fmt.Errorf("raw JSON SetMember produced invalid bytes: %w", err)
	}
	return out, nil
}

// deleteMember removes member key plus exactly one structural separator
// from the object raw bytes. A missing key is a no-op. The result is
// re-validated as an object.
func deleteMember(raw []byte, key string) ([]byte, error) {
	if err := validateMemberKey(key); err != nil {
		return nil, err
	}
	_, order, spans, _, err := parseObjectSpans(raw)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, k := range order {
		if k == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		out := make([]byte, len(raw))
		copy(out, raw)
		return out, nil
	}
	sp := spans[key]
	var prefix, suffix []byte
	if idx > 0 {
		prev := spans[order[idx-1]]
		prefix = raw[:prev.valEnd]
		suffix = raw[sp.valEnd:]
	} else if len(order) > 1 {
		next := spans[order[1]]
		prefix = raw[:sp.keyStart]
		suffix = raw[next.keyStart:]
	} else {
		prefix = raw[:sp.keyStart]
		suffix = raw[sp.valEnd:]
	}
	joined := append(append([]byte{}, prefix...), suffix...)
	if _, err := validateObject(joined, false); err != nil {
		return nil, fmt.Errorf("raw JSON DeleteMember produced invalid bytes: %w", err)
	}
	return joined, nil
}

// ParseRequiredObjectOrEmpty is the host/provider NORMALIZATION constructor
// for required object fields: nil or empty bytes mean the canonical empty
// object (the zero wrapper), while any present bytes must be a strict JSON
// object. This is the single documented normalization point for tool
// arguments and tool schemas on the accepted side (providers that omit the
// member or send an empty arguments string normalize to `{}` here).
func ParseRequiredObjectOrEmpty(raw []byte) (RequiredJSONObject, error) {
	if len(raw) == 0 {
		return RequiredJSONObject{}, nil
	}
	return ParseRequiredJSONObject(raw)
}
