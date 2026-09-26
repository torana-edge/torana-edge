package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
)

var obsoleteChatSettingsWarning sync.Once

// Ignore only the removed chat-control settings. All other unknown fields
// still go through Config's strict decoder and are rejected as before.
func discardObsoleteChatSettings(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	changed := false
	for _, key := range []string{"directives", "assistant"} {
		if _, exists := fields[key]; exists {
			delete(fields, key)
			changed = true
		}
	}
	if rawHarness, exists := fields["harness"]; exists {
		var harness map[string]json.RawMessage
		if json.Unmarshal(rawHarness, &harness) != nil || harness == nil {
			return nil, fmt.Errorf("harness must contain only the obsolete setup_from_directive setting")
		}
		delete(harness, "setup_from_directive")
		if len(harness) != 0 {
			return nil, fmt.Errorf("unknown harness setting")
		}
		delete(fields, "harness")
		changed = true
	}
	if !changed {
		return raw, nil
	}
	obsoleteChatSettingsWarning.Do(func() { log.Print("[config] obsolete chat-control settings ignored; use MCP or Torana's UI/CLI") })
	return json.Marshal(fields)
}
