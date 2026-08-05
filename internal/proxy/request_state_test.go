package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/engine"
)

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
