package mcpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

const correlationTTL = 120 * time.Second
const correlationLimit = 4096

// Binding is host-owned evidence, not a conversation identifier supplied in
// model tool arguments. Recording response calls is wired separately.
type Binding struct {
	ConversationID string
	CallID         string
}

type correlationRecord struct {
	binding  Binding
	tool     string
	hash     [32]byte
	expires  time.Time
	consumed bool
}

// Correlator binds an out-of-band MCP call only to one unconsumed response
// call. It retains consumed records until expiry so response replay cannot
// grant a second binding. It stores hashes, not tool arguments.
type Correlator struct {
	mu             sync.Mutex
	records        map[Binding]correlationRecord
	saturatedUntil time.Time
	saturatedTools map[string]time.Time
}

func NewCorrelator() *Correlator {
	return &Correlator{records: make(map[Binding]correlationRecord)}
}

func canonicalTool(name string) string {
	for _, tool := range []string{"torana_namespaces", "torana_describe", "torana_search", "torana_invoke"} {
		if name == tool || strings.HasSuffix(name, "__"+tool) {
			return tool
		}
	}
	return ""
}

func argumentHash(input json.RawMessage) ([32]byte, bool) {
	// Preserve numeric literals rather than round through float64. A harness
	// rewriting 1.0 to 1 fails closed; transcript binding handles that later.
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil {
		return [32]byte{}, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return [32]byte{}, false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(canonical), true
}

func (c *Correlator) prune(now time.Time) {
	for tool, expires := range c.saturatedTools {
		if !now.Before(expires) {
			delete(c.saturatedTools, tool)
		}
	}
	for key, record := range c.records {
		if !now.Before(record.expires) {
			delete(c.records, key)
		}
	}
}

// Record accepts only host-observed complete response calls. A Gemini adapter
// supplies response ID plus part index as CallID where no native ID exists.
func (c *Correlator) Record(name string, input json.RawMessage, binding Binding, now time.Time) bool {
	tool := canonicalTool(name)
	hash, ok := argumentHash(input)
	if tool == "" || !ok || binding.ConversationID == "" || binding.CallID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	return c.recordLocked(correlationRecord{binding: binding, tool: tool, hash: hash, expires: now.Add(correlationTTL)}, now)
}

func (c *Correlator) recordLocked(record correlationRecord, now time.Time) bool {
	if old, exists := c.records[record.binding]; exists {
		if old.tool != record.tool || old.hash != record.hash {
			old.consumed = true // Contradictory evidence must never bind.
			c.records[record.binding] = old
		}
		return false
	}
	if len(c.records) >= correlationLimit {
		// Do not evict an ambiguity and accidentally leave a unique match.
		c.saturatedUntil = now.Add(correlationTTL)
		return false
	}
	c.records[record.binding] = record
	return true
}

// CommitFrom publishes staged stream evidence only after successful client
// serialization. Preserve consumed/contradictory records and saturation: never
// make an ambiguous call unique by dropping the other side of its evidence.
func (c *Correlator) CommitFrom(staged *Correlator, now time.Time) {
	if c == nil || staged == nil || c == staged {
		return
	}
	staged.mu.Lock()
	records := make([]correlationRecord, 0, len(staged.records))
	for _, record := range staged.records {
		records = append(records, record)
	}
	saturatedUntil := staged.saturatedUntil
	saturatedTools := make(map[string]time.Time, len(staged.saturatedTools))
	for tool, expires := range staged.saturatedTools {
		saturatedTools[tool] = expires
	}
	staged.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	if saturatedUntil.After(c.saturatedUntil) {
		c.saturatedUntil = saturatedUntil
	}
	if c.saturatedTools == nil {
		c.saturatedTools = map[string]time.Time{}
	}
	for tool, expires := range saturatedTools {
		if expires.After(c.saturatedTools[tool]) {
			c.saturatedTools[tool] = expires
		}
	}
	for _, record := range records {
		if now.Before(record.expires) {
			c.recordLocked(record, now)
		}
	}
}

func (c *Correlator) Consume(name string, input json.RawMessage, now time.Time) (Binding, bool) {
	tool := canonicalTool(name)
	hash, ok := argumentHash(input)
	if tool == "" || !ok {
		return Binding{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	if now.Before(c.saturatedUntil) || now.Before(c.saturatedTools[tool]) {
		return Binding{}, false
	}
	var match correlationRecord
	count := 0
	for _, record := range c.records {
		if !record.consumed && record.tool == tool && record.hash == hash {
			match = record
			count++
		}
	}
	if count != 1 {
		return Binding{}, false
	}
	match.consumed = true
	c.records[match.binding] = match
	return match.binding, true
}
