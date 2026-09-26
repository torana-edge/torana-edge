package mcpserver

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestCorrelationUniqueReorderedAndSingleUse(t *testing.T) {
	c := NewCorrelator()
	now := time.Now()
	binding := Binding{"conversation", "call"}
	if !c.Record("mcp__torana__torana_invoke", json.RawMessage(`{"operation":"get","input":{"a":1,"b":2}}`), binding, now) {
		t.Fatal("not recorded")
	}
	input := json.RawMessage(`{"input":{"b":2,"a":1},"operation":"get"}`)
	if got, ok := c.Consume("torana_invoke", input, now); !ok || got != binding {
		t.Fatalf("binding=%+v ok=%t", got, ok)
	}
	if c.Record("torana_invoke", input, binding, now.Add(time.Second)) {
		t.Fatal("replay recorded")
	}
	if _, ok := c.Consume("torana_invoke", input, now); ok {
		t.Fatal("replay consumed")
	}
}

func TestStagedEvidenceCommitPreservesReplayAmbiguityAndExpiry(t *testing.T) {
	now := time.Now()
	input := json.RawMessage(`{"query":"status"}`)
	main, staged := NewCorrelator(), NewCorrelator()
	staged.Record("torana_search", input, Binding{"conversation", "call"}, now)
	if _, ok := main.Consume("torana_search", input, now); ok {
		t.Fatal("uncommitted stream bound")
	}
	main.CommitFrom(staged, now)
	if _, ok := main.Consume("torana_search", input, now); !ok {
		t.Fatal("committed stream did not bind")
	}
	main.CommitFrom(staged, now)
	if _, ok := main.Consume("torana_search", input, now); ok {
		t.Fatal("repeated commit revived consumed evidence")
	}
	main = NewCorrelator()
	main.Record("torana_search", input, Binding{"other", "other-call"}, now)
	main.CommitFrom(staged, now)
	if _, ok := main.Consume("torana_search", input, now); ok {
		t.Fatal("commit erased ambiguity")
	}
	main = NewCorrelator()
	main.CommitFrom(staged, now.Add(correlationTTL+time.Second))
	if _, ok := main.Consume("torana_search", input, now.Add(correlationTTL+time.Second)); ok {
		t.Fatal("expired staged evidence bound")
	}
	main = NewCorrelator()
	main.Record("torana_search", json.RawMessage(`{"query":"different"}`), Binding{"conversation", "call"}, now)
	main.CommitFrom(staged, now)
	if _, ok := main.Consume("torana_search", json.RawMessage(`{"query":"different"}`), now); ok {
		t.Fatal("commit erased contradictory evidence")
	}
	staged.saturatedTools = map[string]time.Time{"torana_search": now.Add(correlationTTL)}
	main = NewCorrelator()
	main.CommitFrom(staged, now)
	if _, ok := main.Consume("torana_search", input, now); ok {
		t.Fatal("commit discarded saturation")
	}
}

func TestCorrelationAmbiguousMissingExpiredAndInvalid(t *testing.T) {
	now := time.Now()
	c := NewCorrelator()
	input := json.RawMessage(`{}`)
	if _, ok := c.Consume("torana_search", input, now); ok {
		t.Fatal("missing bound")
	}
	c.Record("torana_search", input, Binding{"a", "1"}, now)
	c.Record("torana_search", input, Binding{"b", "2"}, now)
	if _, ok := c.Consume("torana_search", input, now); ok {
		t.Fatal("ambiguous bound")
	}
	if _, ok := c.Consume("torana_search", input, now.Add(correlationTTL)); ok {
		t.Fatal("expired bound")
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{} {}`} {
		name := "torana_search"
		if raw == `{}` {
			name = "not_torana_search"
		}
		if c.Record(name, json.RawMessage(raw), Binding{"x", "x"}, now) {
			t.Fatalf("invalid evidence accepted: %s", raw)
		}
	}
}

func TestCorrelationCapacityFailsClosed(t *testing.T) {
	now := time.Now()
	c := NewCorrelator()
	for i := 0; i < correlationLimit; i++ {
		c.Record("torana_search", json.RawMessage(fmt.Sprintf(`{"q":%d}`, i)), Binding{"c", fmt.Sprint(i)}, now)
	}
	if c.Record("torana_search", json.RawMessage(`{"q":0}`), Binding{"other", "overflow"}, now) {
		t.Fatal("capacity exceeded")
	}
	if _, ok := c.Consume("torana_search", json.RawMessage(`{"q":0}`), now); ok {
		t.Fatal("overflow hid ambiguity")
	}
}

func TestCorrelationNumericReserializationFailsClosed(t *testing.T) {
	c := NewCorrelator()
	now := time.Now()
	c.Record("torana_invoke", json.RawMessage(`{"input":{"amount":1.0}}`), Binding{"c", "call"}, now)
	if _, ok := c.Consume("torana_invoke", json.RawMessage(`{"input":{"amount":1}}`), now); ok {
		t.Fatal("numeric rewrite unexpectedly bound")
	}
}

func TestCorrelationContradictoryCallEvidenceNeverBinds(t *testing.T) {
	c := NewCorrelator()
	now := time.Now()
	binding := Binding{"c", "call"}
	c.Record("torana_search", json.RawMessage(`{"query":"first"}`), binding, now)
	c.Record("torana_search", json.RawMessage(`{"query":"second"}`), binding, now)
	for _, input := range []string{`{"query":"first"}`, `{"query":"second"}`} {
		if _, ok := c.Consume("torana_search", json.RawMessage(input), now); ok {
			t.Fatal("contradictory evidence bound")
		}
	}
}
