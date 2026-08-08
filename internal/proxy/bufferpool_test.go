package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestReverseProxyBufferPoolContract(t *testing.T) {
	pool := &reverseProxyBufferPool{}
	buffer := pool.Get()
	if len(buffer) != reverseProxyBufferBytes || cap(buffer) < reverseProxyBufferBytes {
		t.Fatalf("Get() returned len=%d cap=%d", len(buffer), cap(buffer))
	}
	pool.Put(buffer[:17]) // capacity, not current length, decides reusability.
	got := pool.Get()
	if len(got) != reverseProxyBufferBytes || cap(got) < reverseProxyBufferBytes {
		t.Fatalf("reused buffer has len=%d cap=%d", len(got), cap(got))
	}
	pool.Put(make([]byte, 1, reverseProxyBufferBytes-1))
	if got := pool.Get(); len(got) != reverseProxyBufferBytes {
		t.Fatalf("short-buffer rejection broke Get: len=%d", len(got))
	}
}

func TestReverseProxyBufferPoolConcurrentResponseIntegrity(t *testing.T) {
	const requests = 64
	responses := make([][]byte, requests)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(r.Header.Get("X-Bench-Index"))
		if err != nil || index < 0 || index >= len(responses) {
			http.Error(w, "bad index", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(responses[index])
	}))
	defer upstream.Close()

	for i := range requests {
		responses[i] = bytes.Repeat([]byte{byte(i + 1)}, reverseProxyBufferBytes*2+997)
	}
	srv, err := New(Config{Port: "0", Providers: testProviderConfig(upstream.URL, "test", "")})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer func() {
		proxy.Close()
		_ = srv.Shutdown(context.Background())
	}()

	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, proxy.URL+"/provider/test/data", nil)
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("X-Bench-Index", strconv.Itoa(i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				errs <- readErr
				return
			}
			if closeErr != nil {
				errs <- closeErr
				return
			}
			if !bytes.Equal(body, responses[i]) {
				errs <- fmt.Errorf("response %d corrupted: got %d bytes, want %d", i, len(body), len(responses[i]))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
