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
	"unicode"

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

func catalogText(text string, limit int) string {
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text))
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
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
			if op.Source != "plugin" && op.ID != "_config.set" {
				item.InputSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
			}
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
			items = append(items, catalogNamespace{Name: entry.Name, Title: catalogText(entry.Title, 60), Summary: catalogText(entry.Summary, 300), Status: entry.Status, Categories: entry.Categories, ReachableOperations: counts[entry.Name]})
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
		terms := catalogSearchTerms(input.Query)
		if len(terms) == 0 {
			return catalogFailure("invalid_input", "Describe what you want to do in query.")
		}
		type scoredOperation struct {
			operation catalogOperation
			score     int
		}
		scored := []scoredOperation{}
		for _, item := range p.catalog(input.Namespace, false) {
			if item.Deprecated {
				continue
			}
			entry := p.registry.entries[item.Namespace]
			text := item.Namespace + " " + item.ID + " " + item.Description + " " + entry.Title + " " + catalogText(entry.Summary, 300) + " " + strings.Join(entry.Categories, " ")
			for _, op := range entry.Operations {
				if op.ID == item.ID && op.Guest != nil {
					text += " " + strings.Join(op.Guest.Examples, " ")
					break
				}
			}
			switch item.ID {
			case "_disable":
				text += " turn off stop disable"
			case "_enable":
				text += " turn on start enable"
			}
			text = strings.ToLower(text)
			words := map[string]bool{}
			for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
				words[word] = true
			}
			score := 0
			for _, term := range terms {
				// Short intent words such as on/off must be whole words: "on"
				// inside "configuration" must not tie enable with disable.
				if words[term] || len(term) > 3 && strings.Contains(text, term) {
					score++
				}
			}
			if score > 0 {
				scored = append(scored, scoredOperation{item, score})
			}
		}
		sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score }) // Catalog order breaks ties by namespace/ID.
		matches := make([]catalogOperation, 0, min(10, len(scored)))
		for _, match := range scored[:min(10, len(scored))] {
			matches = append(matches, match.operation)
		}
		return mcpserver.Result{OK: true, Result: matches}
	default:
		return catalogFailure("unknown_operation", "Not a discovery tool.")
	}
}

func catalogSearchTerms(query string) []string {
	stop := map[string]bool{"the": true, "a": true, "an": true, "please": true, "can": true, "you": true, "to": true, "my": true, "this": true, "for": true, "of": true, "with": true, "me": true, "is": true, "i": true, "want": true, "and": true, "in": true}
	seen := map[string]bool{}
	terms := []string{}
	for _, term := range strings.Fields(strings.ToLower(query)) {
		term = strings.Trim(term, ".,!?;:")
		if term != "" && !stop[term] && !seen[term] {
			terms = append(terms, term)
			seen[term] = true
		}
	}
	return terms
}
