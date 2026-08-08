package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOneRequestValidatesCompleteStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"complete", "data: {\"x\":1}\n\ndata: {\"x\":2}\n\ndata: [DONE]\n\n", 2, true},
		{"missing done", "data: {\"x\":1}\n\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := oneRequest(srv.Client(), srv.URL, []byte(`{}`), true)
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("oneRequest = (%d, %v), want (%d, ok=%v)", got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestPercentileUsesStableIndex(t *testing.T) {
	values := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond}
	if got := percentile(values, 0.50); got != 2*time.Millisecond {
		t.Fatalf("p50 = %s", got)
	}
	if got := percentile(values, 0.99); got != 4*time.Millisecond {
		t.Fatalf("p99 = %s", got)
	}
}

func TestParseProcessCPUTicksUsesFieldsAfterFinalCommandParen(t *testing.T) {
	stat := "42 (torana bench) S 1 2 3 4 5 6 7 8 9 10 120 30 16"
	got, err := parseProcessCPUTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if got != 150 {
		t.Fatalf("ticks = %d, want 150", got)
	}
}
