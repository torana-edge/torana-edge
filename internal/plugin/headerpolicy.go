package plugin

import (
	"encoding/json"
	"net/http"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// requestHeadersKey is the host-owned ToranaMeta key carrying the allowlisted
// request headers to a plugin's run_before_request hook. The field is
// injected PER PLUGIN (never pipeline-wide) and restored away before the next
// plugin sees the request.
const requestHeadersKey = "_request_headers"

// httpOperationalHeaders are always visible to a run_on_http_request handler.
// They are deliberately NOT exposed through chat metadata: the chat surface
// keeps its credential/identity allowlist only.
var httpOperationalHeaders = []string{"Accept", "Content-Type", "User-Agent"}

// credentialHeaders is the credential/identity header set, IDENTICAL on both
// surfaces: grant-gated on HTTP (only when the exact executing plugin holds
// the approved env.request_headers grant) and the chat projection allowlist
// (string-valued _request_headers). ONE list, so the two surfaces cannot
// drift. X-Torana-Agent, Cookie, Proxy-Authorization and arbitrary custom
// headers are never forwarded on either surface.
var credentialHeaders = []string{
	"Authorization", "X-Api-Key", "X-Torana-User", "X-Torana-Team", "X-Torana-Tenant",
}

// snapshotHeaders copies the raw header map and every value slice at dispatch
// admission. Both dispatch methods treat their raw-header argument as
// untrusted caller input and snapshot it at entry, so a caller that mutates
// its map or slices during dispatch cannot affect what plugins observe. Nil
// means no headers.
func snapshotHeaders(raw map[string][]string) map[string][]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string][]string, len(raw))
	for k, v := range raw {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// resolveHeader finds the HTTP field-name match for one canonical policy
// name in the raw snapshot. The raw map is UNTRUSTED: only a key whose
// net/textproto canonicalization EXACTLY equals the policy name counts.
// Unicode confusables (long-s folding to s, the Kelvin sign folding to K)
// and invalid ASCII names are left unchanged by http.CanonicalHeaderKey and
// therefore never match — Unicode case folding is deliberately NOT used.
// Differently-cased keys for the same canonical header may coexist in the
// raw map; map iteration order must never decide which one wins. Returns:
//
//	(values, false, false) — no matching key (absent);
//	(values, true, false)  — exactly one matching key;
//	(nil, false, true)     — MORE than one matching key: ambiguous, the
//	                         header is omitted fail-closed.
func resolveHeader(raw map[string][]string, canonical string) ([]string, bool, bool) {
	var match []string
	found := false
	for k, v := range raw {
		if http.CanonicalHeaderKey(k) != canonical {
			continue
		}
		if found {
			return nil, false, true
		}
		match = v
		found = true
	}
	return match, found, false
}

// filterHTTPHeaders applies the three-class HTTP header policy. The fixed
// policy lists (httpOperationalHeaders, credentialHeaders) are the single
// source of truth: each allowed canonical header is resolved from the
// snapshot by HTTP field-name canonicalization, and only the fixed canonical
// name is emitted. A case collision (two differently-cased keys for one
// allowed header) omits that header fail-closed. Allowed multi-values are
// preserved in order; everything else is never forwarded.
func filterHTTPHeaders(raw map[string][]string, credentialsGranted bool) map[string][]string {
	out := make(map[string][]string)
	for _, canonical := range httpOperationalHeaders {
		values, found, ambiguous := resolveHeader(raw, canonical)
		if !found || ambiguous {
			continue
		}
		out[canonical] = append([]string(nil), values...)
	}
	if credentialsGranted {
		for _, canonical := range credentialHeaders {
			values, found, ambiguous := resolveHeader(raw, canonical)
			if !found || ambiguous {
				continue
			}
			out[canonical] = append([]string(nil), values...)
		}
	}
	return out
}

// projectChatHeaders maps the raw header snapshot onto the chat allowlist
// (the SAME credentialHeaders vocabulary as HTTP) in the existing
// string-valued representation: EXACTLY element zero of the matched value
// slice, omitting the header when the slice is empty or element zero is empty
// — the same semantics as req.Header.Get. A case collision omits the header
// fail-closed.
func projectChatHeaders(raw map[string][]string) map[string]any {
	headers := make(map[string]any)
	for _, name := range credentialHeaders {
		values, found, ambiguous := resolveHeader(raw, name)
		if !found || ambiguous || len(values) == 0 || values[0] == "" {
			continue
		}
		headers[name] = values[0]
	}
	return headers
}

// injectRequestHeaders writes the per-plugin projection into the request's
// host-owned ToranaMetaJson. The caller restores the EXACT pre-injection
// bytes afterwards (restoreRequestHeaders); this function never leaves the
// field in a request that chains or returns.
func injectRequestHeaders(current *pbv1.ChatRequest, headers map[string]any) {
	meta := map[string]any{}
	if len(current.ToranaMetaJson) > 0 {
		if err := json.Unmarshal(current.ToranaMetaJson, &meta); err != nil {
			// Malformed host meta is a protocol defect; leave the request
			// untouched rather than clobbering it.
			return
		}
	}
	meta[requestHeadersKey] = headers
	b, err := json.Marshal(meta)
	if err != nil {
		return
	}
	current.ToranaMetaJson = b
}

// restoreRequestHeaders restores the EXACT pre-injection ToranaMetaJson bytes
// on the request object(s) that can chain or return. Byte restoration — not
// parse/delete/re-marshal — preserves nil vs "{}", byte identity of unrelated
// host metadata, and works because an accepted replacement must carry
// byte-identical injected metadata (the host-owned torana_meta_json
// invariant).
func restoreRequestHeaders(target *pbv1.ChatRequest, saved []byte) {
	if target != nil {
		target.ToranaMetaJson = saved
	}
}
