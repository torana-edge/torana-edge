package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestFormerCommandPrefixIsOrdinaryUserText(t *testing.T) {
	for _, shape := range bridgeProtocols {
		t.Run(string(shape), func(t *testing.T) {
			requests := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, bridgeUpstreamJSON(shape, false))
			}))
			defer upstream.Close()
			s := newMCPTestServer(t)
			cfg := s.GetConfig().Providers
			cfg.Providers = map[string]provider.Provider{"p": {URL: upstream.URL, Format: shape.Format(), Auth: provider.ProviderAuth{Mode: "none"}}}
			if err := s.SetProviders(cfg); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(s.Handler())
			defer proxy.Close()
			// Build the removed prefix without embedding obsolete instructions
			// in customer-facing documentation or production code.
			body := strings.ReplaceAll(bridgeClientBody(shape, false, false), "look up weather", "torana"+"> status")
			status, _, _ := callBridge(t, proxy, shape, body, false)
			if status != http.StatusOK {
				t.Fatalf("status=%d", status)
			}
			if got := <-requests; !bytes.Equal(got, []byte(body)) {
				t.Fatalf("user bytes changed: %s", got)
			}
		})
	}
}
