package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestLimiterAccountsDuringDisabledWindow(t *testing.T) {
	rl := NewRateLimiter(0, 2)
	defer rl.Close()
	if !rl.Acquire("same") {
		t.Fatal("A refused")
	}
	rl.Update(0, 0)
	if !rl.Acquire("same") {
		t.Fatal("B refused")
	}
	rl.Release("same")
	rl.Update(0, 1)
	if rl.Acquire("same") {
		t.Fatal("B release stole A's slot")
	}
	rl.Release("same")
	if !rl.Acquire("same") {
		t.Fatal("A release did not free slot")
	}
	rl.Release("same")
	rl.Update(0, 0)
	releaseOld, ok := rl.acquireLease("new")
	if !ok {
		t.Fatal("disabled admission refused")
	}
	rl.Update(0, 1)
	releaseNew, ok := rl.acquireLease("new")
	if !ok {
		t.Fatal("disabled request unexpectedly created a counted bucket")
	}
	releaseOld()
	if rl.Acquire("new") {
		t.Fatal("uncounted old request stole the new request's slot")
	}
	releaseNew()
	releaseNewest, ok := rl.acquireLease("new")
	if !ok {
		t.Fatal("released slot not reusable")
	}
	releaseNew()
	if rl.Acquire("new") {
		t.Fatal("double lease release stole the newest slot")
	}
	releaseNewest()
}

func TestLimiterEnablingRPMAndDisabledCardinality(t *testing.T) {
	rl := NewRateLimiter(0, 0)
	defer rl.Close()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		release, ok := rl.acquireLease(id)
		if !ok {
			t.Fatal("disabled admission refused")
		}
		release()
	}
	if len(rl.limits) != 0 {
		t.Fatal("disabled traffic allocated buckets")
	}
	for _, rpm := range []int{1, 6, 60} {
		rl.Update(rpm, 0)
		for i := 0; i < rpm; i++ {
			release, ok := rl.acquireLease("a")
			if !ok {
				t.Fatalf("rpm %d did not start full at request %d", rpm, i)
			}
			release()
		}
		if rl.Acquire("a") {
			t.Fatal("exhausted bucket admitted request")
		}
		rl.Update(0, 0)
	}
}

func TestMalformedQueryIdentityRemainsDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, raw := range []string{"", "key=%zz", "key=%yy", "key=%25zz", "key=valid&api_key=%zz"} {
		r, _ := http.NewRequest(http.MethodGet, "http://example.invalid/?"+raw, nil)
		id := callerCredentialsFrom(r).rateIdentity()
		if seen[id] {
			t.Fatalf("identity collision for %q", raw)
		}
		seen[id] = true
	}
}

func TestTokenBudgetWaitHonorsCancellation(t *testing.T) {
	m := newEgressMeter()
	b := provider.EgressBudget{MaxCallsPerMinute: 10, MaxTokensPerHour: 100}
	unlock, err := m.lockTokenBudget(context.Background(), "p", b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		release, err := m.lockTokenBudget(ctx, "p", b)
		if release != nil {
			release()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled caller stuck behind in-flight request")
	}
	unlock()
	release, err := m.lockTokenBudget(context.Background(), "p", b)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(m.calls["p"]) != 0 {
		t.Fatal("waiting consumed a call budget")
	}
}

func TestFailoverRejectsMixedTransparentFormat(t *testing.T) {
	for _, formats := range [][2]string{{"", "anthropic"}, {"anthropic", ""}, {"", ""}, {"openai", "openai"}} {
		t.Run(formats[0]+"-"+formats[1], func(t *testing.T) {
			calls := 0
			tr := &failoverRoundTripper{cfg: func() provider.Config {
				return provider.Config{Providers: map[string]provider.Provider{
					"p": {URL: "https://p.invalid", Format: formats[0], Fallback: []string{"f"}},
					"f": {URL: "https://f.invalid", Format: formats[1]},
				}}
			}, base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Body != nil {
					r.Body.Close()
				}
				return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("busy")), Request: r}, nil
			})}
			ctx := context.WithValue(context.Background(), routeContextKey{}, &RouteContext{ProviderName: "p"})
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://p.invalid/infer", nil)
			resp, err := tr.RoundTrip(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := 1
			if formats[0] == formats[1] {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}
