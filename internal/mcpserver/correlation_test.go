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
