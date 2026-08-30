package plugin

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/cache"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

// httpEcho dispatches a direct RunOnHTTPRequest to the named fixture and
// decodes the JSON echo.
func httpEcho(t *testing.T, pp *PluginPipeline, pluginName string, httpReq *pbv1.HttpRequest, raw map[string][]string) map[string]any {
	t.Helper()
	resp, err := pp.RunOnHTTPRequest(context.Background(), 1, pluginName, httpReq, raw)
	if err != nil {
		t.Fatalf("RunOnHTTPRequest: %v", err)
	}
	if resp == nil {
		t.Fatal("plugin did not serve the request")
	}
	var echo map[string]any
	if err := json.Unmarshal(resp.Body, &echo); err != nil {
		t.Fatalf("decode echo: %v (%s)", err, resp.Body)
	}
	return echo
}

func echoHeaders(t *testing.T, echo map[string]any) map[string][]string {
	t.Helper()
	raw, err := json.Marshal(echo["headers"])
	if err != nil {
		t.Fatal(err)
	}
	var headers map[string][]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		t.Fatal(err)
	}
	return headers
}

// TestHTTPDispatchBypassIsFiltered — H4/H5/H7: a DIRECT call into the
// dispatch boundary (no route builder anywhere) cannot smuggle headers: a
// pre-populated headers_json carrying sensitive, cookie, and custom-secret
// headers is overwritten from the raw map, and a nil raw map yields an empty
// object rather than exposing the pre-populated field.
func TestHTTPDispatchBypassIsFiltered(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server-nogrant/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-http-server-nogrant"})

	for name, tc := range map[string]struct {
		raw      map[string][]string
		wantAuth bool
	}{
		"raw headers supplied": {
			raw: map[string][]string{
				"Accept":            {"text/html"},
				"Authorization":     {"Bearer sk-torana-secret"},
				"Cookie":            {"session=secret"},
				"X-Customer-Secret": {"s3cr3t"},
			},
			wantAuth: false, // no grant
		},
		"nil raw map": {
			raw: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// A pre-populated headers_json the caller tried to smuggle in.
			httpReq := &pbv1.HttpRequest{
				Method:      "GET",
				Path:        "/echo",
				HeadersJson: []byte(`{"Authorization":["Bearer smuggled"],"Cookie":["session=secret"]}`),
			}
			echo := httpEcho(t, pp, "test-http-server-nogrant", httpReq, tc.raw)
			headers := echoHeaders(t, echo)
			if _, ok := headers["Authorization"]; ok {
				t.Fatalf("smuggled/pre-populated credential reached the guest: %v", headers)
			}
			if _, ok := headers["Cookie"]; ok {
				t.Fatalf("smuggled cookie reached the guest: %v", headers)
			}
			if tc.raw == nil {
				if len(headers) != 0 {
					t.Fatalf("nil raw map must yield an empty header set, got %v", headers)
				}
			} else if _, ok := headers["Accept"]; !ok {
				t.Fatalf("operational header missing: %v", headers)
			}
		})
	}
}

// TestHTTPDispatchCallerRequestIsUntouched — the ruling: filtering operates
// on a clone, so the caller's HttpRequest argument stays byte-identical after
// dispatch even when its pre-populated headers_json would have been
// overwritten.
func TestHTTPDispatchCallerRequestIsUntouched(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server-nogrant/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-http-server-nogrant"})

	httpReq := &pbv1.HttpRequest{
		Method:      "GET",
		Path:        "/echo",
		HeadersJson: []byte(`{"Authorization":["Bearer smuggled"]}`),
	}
	before, err := proto.Marshal(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	echo := httpEcho(t, pp, "test-http-server-nogrant", httpReq, map[string][]string{"Accept": {"text/html"}})
	if headers := echoHeaders(t, echo); len(headers) != 1 {
		t.Fatalf("echo headers = %v", headers)
	}
	after, err := proto.Marshal(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the caller's HttpRequest was mutated by dispatch")
	}
}

// httpMutatingStore mutates the caller's raw header map inside Get — the
// deterministic barrier firing during the guest's CacheGet host call.
type httpMutatingStore struct {
	cache.Store
	mu      sync.Mutex
	mutated bool
	raw     map[string][]string
}

func (s *httpMutatingStore) Get(key string) (string, bool) {
	s.mu.Lock()
	if !s.mutated {
		s.mutated = true
		s.raw["Authorization"] = []string{"Bearer MUTATED"}
	}
	s.mu.Unlock()
	return s.Store.Get(key)
}

// TestHTTPDispatchSnapshotIsImmuneToCallerMutation — the HTTP dispatch
// snapshots the raw map at admission: a mutation firing mid-dispatch (inside
// the guest's cache host call) cannot change what the guest observes.
func TestHTTPDispatchSnapshotIsImmuneToCallerMutation(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	raw := map[string][]string{
		"Accept":        {"text/html"},
		"Authorization": {"Bearer sk-torana-real"},
	}
	store := &httpMutatingStore{Store: cache.NewLocalCache(0), raw: raw}
	pp := newTestPipelineWith(t, fixturesDir, []string{"test-http-server"}, store, nil)

	httpReq := &pbv1.HttpRequest{Method: "GET", Path: "/echo"}
	echo := httpEcho(t, pp, "test-http-server", httpReq, raw)
	headers := echoHeaders(t, echo)
	if got := headers["Authorization"]; len(got) != 1 || got[0] != "Bearer sk-torana-real" {
		t.Fatalf("the guest observed the mutated map: %v", headers)
	}
}
