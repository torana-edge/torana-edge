package proxy

import "encoding/json"

// rawJSONSpan returns the VERBATIM bytes of the value at path in doc — the
// exact characters the provider sent, with no decode/re-encode round-trip.
// Path elements are string (object key) or int (array index).
//
// Strings come back as the full quoted bytes INCLUDING the quotes, with the
// provider's escaping intact; numbers are untouched (an integer above 2^53
// survives, which a float64 decode cannot). Keys and strings may contain
// escaped quotes and backslashes. Returns ok=false when the path does not
// exist or doc is not parseable JSON.
//
// The response pipeline needs this because the decoded map round-trip is
// lossy: json.Marshal sorts object keys and rounds large integers through
// float64. The only way to ship a body that is byte-identical to what the
// provider sent for everything the pipeline did not touch is to locate those
// regions in the original bytes and splice them back.
func rawJSONSpan(doc []byte, path ...any) ([]byte, bool) {
	start, end, ok := rawJSONSpanAt(doc, path...)
	if !ok {
		return nil, false
	}
	return doc[start:end], true
}

// rawJSONSpanAt is rawJSONSpan plus the byte offsets into doc, so a caller can
// splice a replacement value into the document in place.
func rawJSONSpanAt(doc []byte, path ...any) (int, int, bool) {
	i := 0
	skipWS(doc, &i)
	if i >= len(doc) {
		return 0, 0, false
	}
	return spanAt(doc, i, path)
}

// spanAt assumes doc[i] is the start of a JSON value and walks path down to
// that value, returning the value's [start, end) byte range.
func spanAt(doc []byte, i int, path []any) (int, int, bool) {
	if len(path) == 0 {
		return i, skipValue(doc, i), true
	}
	n := len(doc)
	switch el := path[0].(type) {
	case string:
		// doc[i] must be an object; scan its keys for el.
		if i >= n || doc[i] != '{' {
			return 0, 0, false
		}
		j := i + 1
		for {
			skipWS(doc, &j)
			if j >= n || doc[j] == '}' { // empty object or key exhausted
				return 0, 0, false
			}
			if doc[j] != '"' {
				return 0, 0, false
			}
			ke := scanStringEnd(doc, j) // index after the key's closing quote
			if ke < 0 {
				return 0, 0, false
			}
			key, err := unescapeJSONString(doc[j:ke])
			if err != nil {
				return 0, 0, false
			}
			j = ke
			skipWS(doc, &j)
			if j >= n || doc[j] != ':' {
				return 0, 0, false
			}
			j++
			skipWS(doc, &j)
			if j >= n {
				return 0, 0, false
			}
			if key == el {
				return spanAt(doc, j, path[1:])
			}
			j = skipValue(doc, j)
			skipWS(doc, &j)
			if j >= n {
				return 0, 0, false
			}
			if doc[j] != ',' {
				return 0, 0, false
			}
			j++
		}
	case int:
		// doc[i] must be an array; walk to element el.
		if i >= n || doc[i] != '[' {
			return 0, 0, false
		}
		j := i + 1
		idx := 0
		for {
			skipWS(doc, &j)
			if j >= n || doc[j] == ']' { // empty array or index exhausted
				return 0, 0, false
			}
			if idx == el {
				return spanAt(doc, j, path[1:])
			}
			j = skipValue(doc, j)
			skipWS(doc, &j)
			if j >= n {
				return 0, 0, false
			}
			if doc[j] != ',' {
				return 0, 0, false
			}
			j++
			idx++
		}
	}
	return 0, 0, false
}

// skipValue returns the index just past the JSON value starting at doc[i].
// Strings are skipped whole (so braces/brackets inside them cannot confuse the
// depth count); numbers and literals run to the next structural delimiter.
func skipValue(doc []byte, i int) int {
	n := len(doc)
	if i >= n {
		return n
	}
	switch doc[i] {
	case '{':
		return skipContainer(doc, i, '}', n)
	case '[':
		return skipContainer(doc, i, ']', n)
	case '"':
		e := scanStringEnd(doc, i)
		if e < 0 {
			return n
		}
		return e
	default:
		j := i
		for j < n && !isDelim(doc[j]) {
			j++
		}
		return j
	}
}

func skipContainer(doc []byte, i int, close byte, n int) int {
	depth := 1
	j := i + 1
	for j < n {
		switch doc[j] {
		case '"':
			e := scanStringEnd(doc, j)
			if e < 0 {
				return n
			}
			j = e
			continue
		case doc[i]: // opening brace/bracket of the same kind
			depth++
		case close:
			depth--
			if depth == 0 {
				return j + 1
			}
		}
		j++
	}
	return n
}

// scanStringEnd returns the index just past the closing quote of the string
// starting at doc[i] (which must be '"'). A backslash escapes the next byte,
// so escaped quotes and backslashes inside the string do not terminate it.
// Returns -1 when the string is unterminated.
func scanStringEnd(doc []byte, i int) int {
	n := len(doc)
	j := i + 1
	for j < n {
		switch doc[j] {
		case '\\':
			j += 2
		case '"':
			return j + 1
		default:
			j++
		}
	}
	return -1
}

// unescapeJSONString decodes a JSON string literal (including its quotes) to
// its content, so a key like "a\"b" compares equal to the decoded key a"b.
func unescapeJSONString(lit []byte) (string, error) {
	var s string
	err := json.Unmarshal(lit, &s)
	return s, err
}

func skipWS(doc []byte, i *int) {
	for *i < len(doc) {
		switch doc[*i] {
		case ' ', '\t', '\n', '\r':
			*i++
		default:
			return
		}
	}
}

func isDelim(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}
