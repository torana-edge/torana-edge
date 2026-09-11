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

// maxReportedContentTypes bounds how many distinct unrecognised media types
// are remembered for de-duplication.
//
// The set is keyed by a string the UPSTREAM chooses, so without a cap its
// cardinality is the upstream's to decide: a misbehaving or hostile provider
// answering application/x-1, application/x-2, … grows the proxy's memory for
// the life of the process, and does it purely to suppress a diagnostic.
// Stripping parameters stops churn within one base type; it does not bound how
// many base types exist.
//
// Sixteen is enough to name the handful a real deployment could produce while
// making the memory a constant.
const maxReportedContentTypes = 16

// reportedContentTypes tracks which unrecognised media types have already been
// logged, up to maxReportedContentTypes, after which the set is SATURATED and
// stops growing.
var reportedContentTypes = struct {
	mu        sync.Mutex
	seen      map[string]struct{}
	saturated bool
}{seen: make(map[string]struct{}, maxReportedContentTypes)}

// warnUnpipelinedContentType reports, at most once per media type and for at
// most maxReportedContentTypes of them, that a response passed through
// ungoverned. Beyond the cap it says so once and then stays silent, because a
// diagnostic must not become a way to grow the process.
func (s *Server) warnUnpipelinedContentType(contentType string) {
	mt := mediaTypeOf(contentType)
	if mt == "" {
		mt = "(absent)"
	}

	reportedContentTypes.mu.Lock()
	if _, seen := reportedContentTypes.seen[mt]; seen {
		reportedContentTypes.mu.Unlock()
		return
	}
	if reportedContentTypes.saturated {
		reportedContentTypes.mu.Unlock()
		return
	}
	if len(reportedContentTypes.seen) >= maxReportedContentTypes {
		reportedContentTypes.saturated = true
		reportedContentTypes.mu.Unlock()
		log.Printf("[proxy] more than %d distinct unrecognised response Content-Types have "+
			"been seen; further ones will not be reported individually. Responses the "+
			"pipeline cannot decode still pass through unchanged, with no plugins and no "+
			"usage or cost recorded.", maxReportedContentTypes)
		return
	}
	reportedContentTypes.seen[mt] = struct{}{}
	reportedContentTypes.mu.Unlock()

	log.Printf("[proxy] responses with Content-Type %q are passed through unchanged: "+
		"no response plugins run, and no usage or cost is recorded for them. "+
		"If this provider returns JSON, it should say so with a JSON media type.", mt)
}

// resetReportedContentTypesForTest clears the bounded set so a test can observe
// its growth from a known state.
func resetReportedContentTypesForTest() {
	reportedContentTypes.mu.Lock()
	defer reportedContentTypes.mu.Unlock()
	reportedContentTypes.seen = make(map[string]struct{}, maxReportedContentTypes)
	reportedContentTypes.saturated = false
}
