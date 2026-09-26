package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
)

// catalogOperation deliberately excludes guest HTTP routes, configuration
// values and operator-only metadata. Discovery uses the invocation policy.
type catalogOperation struct {
	Namespace           string          `json:"namespace"`
	ID                  string          `json:"id"`
	Description         string          `json:"description"`
	ModelAccess         string          `json:"model_access"`
	ConversationBinding string          `json:"conversation_binding"`
	InputSchema         json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema        json.RawMessage `json:"output_schema,omitempty"`
	Examples            []string        `json:"examples,omitempty"`
	Deprecated          bool            `json:"deprecated,omitempty"`
	ReplacedBy          string          `json:"replaced_by,omitempty"`
}

type catalogNamespace struct {
	Name                string   `json:"name"`
	Title               string   `json:"title"`
	Summary             string   `json:"summary"`
	Status              string   `json:"status"`
	Categories          []string `json:"categories,omitempty"`
	ReachableOperations int      `json:"reachable_operations"`
}

func catalogFailure(code, message string) mcpserver.Result {
	return mcpserver.Result{Error: &mcpserver.DomainError{Code: code, Message: message}}
}

func (p *namespaceAccessPolicy) catalog(namespace string, detail bool) []catalogOperation {
	result := []catalogOperation{}
	if p == nil || p.registry == nil {
		return result
	}
	for _, entry := range p.registry.list() {
		if namespace != "" && entry.Name != namespace {
			continue
		}
		for _, op := range entry.Operations {
			access := p.ModelReachable(entry.Name, op.ID)
			if access == "never" {
				continue
			}
			item := catalogOperation{Namespace: entry.Name, ID: op.ID, Description: op.Description, ModelAccess: access, ConversationBinding: op.ConversationBinding}
			if op.Guest != nil {
				item.InputSchema = op.Guest.InputSchema
				item.Deprecated, item.ReplacedBy = op.Guest.Deprecated, op.Guest.ReplacedBy
				if detail {
					item.OutputSchema, item.Examples = op.Guest.OutputSchema, op.Guest.Examples
				}
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Namespace == result[j].Namespace {
			return result[i].ID < result[j].ID
		}
		return result[i].Namespace < result[j].Namespace
	})
	return result
}

// catalogDispatch implements only the three discovery tools. Invocation remains
// a separate host dispatcher with binding, consent and revision enforcement.
func (p *namespaceAccessPolicy) catalogDispatch(tool string, raw json.RawMessage) mcpserver.Result {
	if p == nil || p.registry == nil {
		return catalogFailure("not_configured", "Torana discovery is unavailable.")
	}
	var input struct {
		Namespace string `json:"namespace"`
		Operation string `json:"operation"`
		Cursor    string `json:"cursor"`
		Query     string `json:"query"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var extra any
	if len(raw) > 4096 || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") || decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
		return catalogFailure("invalid_input", "Use a small JSON object for discovery input.")
	}
	if tool != "torana_namespaces" && input.Namespace != "" {
		if _, exists := p.registry.resolve(input.Namespace, false); !exists {
			return catalogFailure("unknown_namespace", "Use a canonical namespace from torana_namespaces.")
		}
	}
	switch tool {
	case "torana_namespaces":
		items := []catalogNamespace{}
		counts := map[string]int{}
		for _, operation := range p.catalog("", false) {
			counts[operation.Namespace]++
		}
		for _, entry := range p.registry.list() {
			items = append(items, catalogNamespace{Name: entry.Name, Title: entry.Title, Summary: entry.Summary, Status: entry.Status, Categories: entry.Categories, ReachableOperations: counts[entry.Name]})
		}
		return mcpserver.Result{OK: true, Result: items}
	case "torana_describe":
		if input.Namespace == "" || (input.Operation != "" && input.Cursor != "") {
			return catalogFailure("invalid_input", "Choose a namespace, and either an operation or a page cursor.")
		}
		items := p.catalog(input.Namespace, input.Operation != "")
		if input.Operation != "" {
			for _, item := range items {
				if item.ID == input.Operation {
					return mcpserver.Result{OK: true, Namespace: input.Namespace, Operation: input.Operation, Result: item}
				}
			}
			return catalogFailure("unknown_operation", "This operation is not available to the model.")
		}
		// Tie cursors to the exact visible catalog: a policy/plugin change
		// cannot silently skip entries or expose a stale page.
		encoded, _ := json.Marshal(items)
		sum := sha256.Sum256(encoded)
		stamp := hex.EncodeToString(sum[:])
		start := 0
		if input.Cursor != "" {
			parts := strings.Split(input.Cursor, ":")
			if len(parts) != 2 || parts[0] != stamp {
				return catalogFailure("invalid_input", "The catalog changed; describe this namespace again without a cursor.")
			}
			var err error
			start, err = strconv.Atoi(parts[1])
			if err != nil || start <= 0 || start >= len(items) || start%50 != 0 {
				return catalogFailure("invalid_input", "Invalid discovery cursor.")
			}
		}
		end := min(start+50, len(items))
		next := ""
		if end < len(items) {
			next = fmt.Sprintf("%s:%d", stamp, end)
		}
		return mcpserver.Result{OK: true, Namespace: input.Namespace, Result: struct {
			Operations []catalogOperation `json:"operations"`
			NextCursor string             `json:"next_cursor,omitempty"`
		}{items[start:end], next}}
	case "torana_search":
		terms := strings.Fields(strings.ToLower(input.Query))
		if len(terms) == 0 {
			return catalogFailure("invalid_input", "Describe what you want to do in query.")
		}
		matches := []catalogOperation{}
		for _, item := range p.catalog(input.Namespace, false) {
			if item.Deprecated {
				continue
			}
			text := strings.ToLower(item.Namespace + " " + item.ID + " " + strings.ReplaceAll(item.ID, "_", " ") + " " + item.Description)
			match := true
			for _, term := range terms {
				if !strings.Contains(text, term) {
					match = false
					break
				}
			}
			if match {
				matches = append(matches, item)
				if len(matches) == 10 {
					break
				}
			}
		}
		return mcpserver.Result{OK: true, Result: matches}
	default:
		return catalogFailure("unknown_operation", "Not a discovery tool.")
	}
}
