package proxy

import (
	"log"
	"mime"
	"strings"
	"sync"
)

// The response pipeline decodes exactly two shapes: an SSE stream and a JSON
// body. Which content types count as those was decided by
// strings.Contains(contentType, "application/json"), which missed the entire
// `+json` suffix family that RFC 6839 defines as the structured-syntax
// convention for JSON — application/vnd.api+json, application/problem+json,
// and any vendor type ending in +json.
//
// A provider emitting one of those bypassed EVERY response hook, all metering
// and all cost accounting, and said nothing about it. A PII redaction plugin
// simply did not run; the operator's spend report simply lost the request.

// mediaTypeOf returns the lower-cased media type with parameters stripped.
// A content type this cannot parse falls back to the text before the first
// ";" rather than being discarded, because a malformed header from an upstream
// should not decide whether a plugin runs.
func mediaTypeOf(contentType string) string {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		return strings.ToLower(strings.TrimSpace(mt))
	}
	base, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// isJSONMediaType reports whether a body is JSON the pipeline can decode.
func isJSONMediaType(contentType string) bool {
	mt := mediaTypeOf(contentType)
	return mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json")
}

// isEventStreamMediaType reports whether a body is an SSE stream.
func isEventStreamMediaType(contentType string) bool {
	return mediaTypeOf(contentType) == "text/event-stream"
}

// unpipelinedContentTypes remembers which content types have already been
// reported, so an upstream that emits one on every request logs once rather
// than once per call.
var unpipelinedContentTypes sync.Map

// warnUnpipelinedContentType reports, once per content type, that a response
// passed through ungoverned.
func (s *Server) warnUnpipelinedContentType(contentType string) {
	mt := mediaTypeOf(contentType)
	if mt == "" {
		mt = "(absent)"
	}
	if _, seen := unpipelinedContentTypes.LoadOrStore(mt, struct{}{}); seen {
		return
	}
	log.Printf("[proxy] responses with Content-Type %q are passed through unchanged: "+
		"no response plugins run, and no usage or cost is recorded for them. "+
		"If this provider returns JSON, it should say so with a JSON media type.", mt)
}
