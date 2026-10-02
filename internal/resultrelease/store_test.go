package resultrelease

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

func testStore(t *testing.T, path string) *Store {
	t.Helper()
	state, err := pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return &Store{State: state, MAC: func(domain, value string) ([]byte, error) {
		h := hmac.New(sha256.New, []byte("synthetic-test-secret"))
		h.Write([]byte(domain))
		h.Write([]byte{0})
		h.Write([]byte(value))
		return h.Sum(nil), nil
	}}
}

func testScope() Scope {
	return Scope{Conversation: "conversation-a", Plugin: "pii", Digest: "bundle-a", CallID: "call-a", ContentHash: strings.Repeat("a", 64)}
}

func TestReleaseIsolationPersistenceAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := testStore(t, path)
	scope := testScope()
	if _, found, err := s.Observe(scope, false); err != nil || found {
		t.Fatalf("absent=%v %v", found, err)
	}
	item, found, err := s.Observe(scope, true)
	if err != nil || !found || item.Status != "withheld" {
		t.Fatalf("observe=%+v %v", item, err)
	}
	if _, err := s.Decide(item.Reference, "withheld", "approved"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrequested approval=%v", err)
	}
	if _, err := s.Request(item.Reference, "conversation-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross conversation=%v", err)
	}
	if _, err := s.Request(item.Reference, scope.Conversation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(item.Reference, "pending", "approved"); err != nil {
		t.Fatal(err)
	}
	for _, other := range []Scope{
		{Conversation: "conversation-b", Plugin: scope.Plugin, Digest: scope.Digest, CallID: scope.CallID, ContentHash: scope.ContentHash},
		{Conversation: scope.Conversation, Plugin: "pii_guard", Digest: scope.Digest, CallID: scope.CallID, ContentHash: scope.ContentHash},
		{Conversation: scope.Conversation, Plugin: scope.Plugin, Digest: "bundle-b", CallID: scope.CallID, ContentHash: scope.ContentHash},
		{Conversation: scope.Conversation, Plugin: scope.Plugin, Digest: scope.Digest, CallID: "call-b", ContentHash: scope.ContentHash},
		{Conversation: scope.Conversation, Plugin: scope.Plugin, Digest: scope.Digest, CallID: scope.CallID, ContentHash: strings.Repeat("b", 64)},
	} {
		if record, found, err := s.Observe(other, false); err != nil || found || record.Status == "approved" {
			t.Fatalf("scope leaked=%+v %v %v", other, found, err)
		}
	}
	if err := s.State.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path)
	replayed, found, err := s.Observe(scope, false)
	if err != nil || !found || replayed.Reference != item.Reference || replayed.Status != "approved" {
		t.Fatalf("restart=%+v %v", replayed, err)
	}
	if _, err := s.Decide(item.Reference, "approved", "revoked"); err != nil {
		t.Fatal(err)
	}
	again, err := s.Request(item.Reference, scope.Conversation)
	if err != nil || again.Status != "revoked" {
		t.Fatalf("model reopened=%+v %v", again, err)
	}
}

func TestConcurrentRequestsDoNotEraseDecision(t *testing.T) {
	s := testStore(t, "")
	item, _, err := s.Observe(testScope(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Request(item.Reference, item.Conversation); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Request(item.Reference, item.Conversation)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	if _, err := s.Decide(item.Reference, "pending", "declined"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	record, err := s.Get(item.Reference)
	if err != nil || record.Status != "declined" {
		t.Fatalf("decision=%+v %v", record, err)
	}
	raw, found, err := s.State.Get(Namespace, "result/"+item.Reference)
	if err != nil || !found || strings.Contains(raw, "tool output") {
		t.Fatalf("record=%s %v", raw, err)
	}
}
