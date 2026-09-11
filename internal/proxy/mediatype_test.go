package proxy

import (
	"fmt"
	"testing"
)

// Which content types reach the response pipeline decides whether a plugin
// runs at all. The predicate was strings.Contains(ct, "application/json"),
// which missed the whole `+json` suffix family RFC 6839 defines — so a
// provider answering with application/vnd.api+json had every response hook
// skipped, and its usage and cost never recorded, silently.
func TestIsJSONMediaType(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"Application/JSON", true},
		{"  application/json  ", true},
		{"text/json", true},

		// The suffix family the old check could not see.
		{"application/vnd.api+json", true},
		{"application/problem+json", true},
		{"application/vnd.anthropic.v1+json", true},
		{"application/ld+json; charset=utf-8", true},

		// Not JSON.
		{"text/event-stream", false},
		{"application/octet-stream", false},
		{"text/plain", false},
		{"application/vnd.amazon.eventstream", false},
		{"", false},

		// The old substring check matched these; a media type must not be
		// recognised because another token happens to contain its name.
		{"text/plain; note=application/json", false},
		{"multipart/form-data; boundary=application/json", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := isJSONMediaType(tc.in); got != tc.want {
				t.Errorf("isJSONMediaType(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsEventStreamMediaType(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"Text/Event-Stream", true},
		{"application/json", false},
		{"", false},
		{"text/plain; x=text/event-stream", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := isEventStreamMediaType(tc.in); got != tc.want {
				t.Errorf("isEventStreamMediaType(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// A header the parser rejects must still be classified on its leading token,
// rather than silently becoming "not JSON" and skipping the pipeline.
func TestMalformedContentTypeStillClassifies(t *testing.T) {
	for _, in := range []string{
		"application/json;",
		"application/json; charset",
		"application/vnd.api+json; =bad",
	} {
		if !isJSONMediaType(in) {
			t.Errorf("isJSONMediaType(%q) = false; a malformed parameter list must not "+
				"decide that a JSON body is not JSON", in)
		}
	}
}

// The de-duplication set is keyed by a string the UPSTREAM chooses, so its
// size must be the proxy's decision and not the provider's.
//
// Without a cap, an upstream answering application/x-1, application/x-2, …
// grows the process for its lifetime — purely to suppress a diagnostic. That
// is a memory-exhaustion vector introduced by a log line.
func TestUnrecognisedContentTypeSetIsBounded(t *testing.T) {
	resetReportedContentTypesForTest()
	t.Cleanup(resetReportedContentTypesForTest)

	srv := &Server{}
	for i := range 10_000 {
		srv.warnUnpipelinedContentType(fmt.Sprintf("application/x-%d", i))
	}

	reportedContentTypes.mu.Lock()
	n := len(reportedContentTypes.seen)
	saturated := reportedContentTypes.saturated
	reportedContentTypes.mu.Unlock()

	if n > maxReportedContentTypes {
		t.Errorf("retained %d distinct media types after 10000 distinct responses; the bound "+
			"is %d and it is the proxy's to choose, not the upstream's", n, maxReportedContentTypes)
	}
	if !saturated {
		t.Error("the set never reported itself saturated, so nothing tells an operator that " +
			"further types are going unreported")
	}
}

// Saturation must not change what the pipeline DOES: classification and
// pass-through are unaffected by how many diagnostics have been emitted.
func TestSaturationDoesNotChangeClassification(t *testing.T) {
	resetReportedContentTypesForTest()
	t.Cleanup(resetReportedContentTypesForTest)

	srv := &Server{}
	for i := range 10_000 {
		srv.warnUnpipelinedContentType(fmt.Sprintf("application/x-%d", i))
	}

	if !isJSONMediaType("application/vnd.api+json") {
		t.Error("a +json type stopped being classified as JSON after saturation")
	}
	if !isEventStreamMediaType("text/event-stream") {
		t.Error("SSE stopped being classified after saturation")
	}
	if isJSONMediaType("application/octet-stream") {
		t.Error("a non-JSON type started being classified as JSON after saturation")
	}
}

// Repeating one type logs once and retains one entry, which is the behaviour
// the cap must not cost us.
func TestRepeatedContentTypeIsRememberedOnce(t *testing.T) {
	resetReportedContentTypesForTest()
	t.Cleanup(resetReportedContentTypesForTest)

	srv := &Server{}
	for range 100 {
		srv.warnUnpipelinedContentType("application/octet-stream; charset=binary")
	}
	reportedContentTypes.mu.Lock()
	n := len(reportedContentTypes.seen)
	reportedContentTypes.mu.Unlock()
	if n != 1 {
		t.Errorf("retained %d entries for one repeated media type, want 1", n)
	}
}
