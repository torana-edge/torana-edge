package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/engine"
)

type requestBodyProbe struct {
	reader io.Reader
	reads  int
	closed bool
}

func (p *requestBodyProbe) Read(dst []byte) (int, error) {
	p.reads++
	return p.reader.Read(dst)
}

func (p *requestBodyProbe) Close() error {
	p.closed = true
	return nil
}

func TestMissingRequestStateIsExplicit(t *testing.T) {
	if got := reqStateFrom(context.Background()); got != nil {
		t.Fatalf("missing request state = %#v, want nil", got)
	}
}

func TestStateDependentOperationsFailClosedWithoutRequestState(t *testing.T) {
	decision := (&Server{}).evaluateCompaction(context.Background(), economics.CompactionReport{})
	if decision.Reason != economics.UnavailableRouteUnresolved {
		t.Fatalf("compaction reason = %q, want unavailable route", decision.Reason)
	}
	_, err := runJSONResponseHooks(
		context.Background(), nil, 0, "openai", &engine.ChatRequest{}, []byte(`{}`),
	)
	if err == nil || !strings.Contains(err.Error(), "request state unavailable") {
		t.Fatalf("response hook error = %v", err)
	}
}

func TestEnsureRequestStateAttachesOneDurableObject(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := ensureReqState(req)
	first.Provider = "openai"
	second := ensureReqState(req)
	if first != second || second.Provider != "openai" || reqStateFrom(req.Context()) != first {
		t.Fatal("ensured request state was not durably attached to the request")
	}
}

func TestRequestBodyForRewriteUsesBoundarySnapshotWithoutReadingAgain(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := &requestBodyProbe{reader: strings.NewReader("stale-reader-copy")}
	req.Body = probe
	rs := ensureReqState(req)
	rs.inboundBody = []byte("boundary-owned")
	rs.inboundBodySet = true

	got := requestBodyForRewrite(req)
	if string(got) != "boundary-owned" || probe.reads != 0 || !probe.closed {
		t.Fatalf("body=%q reads=%d closed=%v", got, probe.reads, probe.closed)
	}
	if &got[0] != &rs.inboundBody[0] {
		t.Fatal("rewrite copied the authoritative snapshot")
	}
}

func TestRequestBodyForRewriteDirectEntryFallback(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := &requestBodyProbe{reader: strings.NewReader("direct-entry")}
	req.Body = probe

	got := requestBodyForRewrite(req)
	if string(got) != "direct-entry" || probe.reads == 0 || !probe.closed {
		t.Fatalf("body=%q reads=%d closed=%v", got, probe.reads, probe.closed)
	}
}

func TestReadRequestBodyTreatsContentLengthOnlyAsHint(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	for _, tc := range []struct {
		name string
		hint int64
	}{
		{name: "exact", hint: int64(len(payload))},
		{name: "under-reported", hint: 17},
		{name: "over-reported", hint: int64(len(payload) + 100)},
		{name: "unknown", hint: -1},
		{name: "oversized declaration", hint: maxBodySize + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readRequestBody(bytes.NewReader(payload), tc.hint)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("read body: len=%d err=%v", len(got), err)
			}
		})
	}
}

func TestReadRequestBodyPreservesBoundaryError(t *testing.T) {
	wantErr := errors.New("boundary read failed")
	reader := io.MultiReader(strings.NewReader("prefix"), errorReader{err: wantErr})
	got, err := readRequestBody(reader, 32)
	if string(got) != "prefix" || !errors.Is(err, wantErr) {
		t.Fatalf("body=%q err=%v", got, err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
