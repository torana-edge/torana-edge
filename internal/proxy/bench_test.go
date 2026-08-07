package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// BenchmarkHTTPDataPlane measures the full local HTTP data plane against the
// same in-process upstream, with and without Torana in between. The Torana arm
// includes request parsing, provider adaptation, routing, reverse proxying,
// response parsing, usage accounting, and response rendering. It deliberately
// loads no plugins; plugin-specific costs are measured in internal/plugin.
//
// Run on an otherwise idle machine:
//
//	go test ./internal/proxy -run '^$' -bench BenchmarkHTTPDataPlane \
//	  -benchmem -benchtime=2s -count=5
//
// Compare paired direct/torana rows at the same concurrency. The direct arm is
// the HTTP/client/httptest floor, not a claim about a production provider.
func BenchmarkHTTPDataPlane(b *testing.B) {
	logOutput := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(logOutput) })

	const completion = `{"id":"bench","object":"chat.completion","created":1,"model":"gpt-bench","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completion)
	}))
	b.Cleanup(upstream.Close)

	srv, err := New(Config{
		Port:      "0",
		Providers: testProviderConfig(upstream.URL, "test", "openai"),
	})
	if err != nil {
		b.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	b.Cleanup(func() {
		proxy.Close()
		_ = srv.Shutdown(context.Background())
	})

	payload := []byte(`{"model":"gpt-bench","messages":[{"role":"user","content":"hello"}]}`)
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
	}}
	b.Cleanup(client.CloseIdleConnections)

	for _, concurrency := range []int{1, 8, 32} {
		for _, target := range []struct {
			name string
			url  string
		}{
			{name: "direct", url: upstream.URL + "/v1/chat/completions"},
			{name: "torana", url: proxy.URL + "/provider/test/v1/chat/completions"},
		} {
			b.Run(fmt.Sprintf("%s/concurrency=%d", target.name, concurrency), func(b *testing.B) {
				benchmarkHTTPRequests(b, client, target.url, payload, concurrency)
			})
		}
	}
}

func benchmarkHTTPRequests(b *testing.B, client *http.Client, url string, payload []byte, concurrency int) {
	b.Helper()
	b.ReportAllocs()
	jobs := make(chan struct{})
	errs := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for range jobs {
				req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
				if err == nil {
					req.Header.Set("Content-Type", "application/json")
					var response *http.Response
					response, err = client.Do(req)
					if err == nil {
						_, readErr := io.Copy(io.Discard, response.Body)
						closeErr := response.Body.Close()
						if response.StatusCode != http.StatusOK {
							err = fmt.Errorf("status %d", response.StatusCode)
						} else if readErr != nil {
							err = readErr
						} else {
							err = closeErr
						}
					}
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
	}

	b.ResetTimer()
	for range b.N {
		jobs <- struct{}{}
	}
	close(jobs)
	workers.Wait()
	b.StopTimer()
	select {
	case err := <-errs:
		b.Fatal(err)
	default:
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "requests/s")
}
