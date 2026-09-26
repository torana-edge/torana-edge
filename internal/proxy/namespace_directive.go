package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/torana-edge/torana-edge/internal/directive"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

type namespaceDirectiveCall struct {
	Namespace           string
	Operation           string
	Input               json.RawMessage
	Confirm             bool
	ConversationBinding string
}

// Resolve only the declared command vocabulary. User text is never treated as
// an HTTP path or an operation ID, and arguments never undergo shell expansion.
func (p *namespaceAccessPolicy) resolveNamespaceDirective(command directive.Command) (namespaceDirectiveCall, error) {
	if !command.Known || p == nil || p.registry == nil {
		return namespaceDirectiveCall{}, fmt.Errorf("try torana> help")
	}
	entry, ok := p.registry.resolve(command.Namespace, true)
	if !ok {
		return namespaceDirectiveCall{}, fmt.Errorf("unknown Torana namespace; try torana> help")
	}
	for _, op := range entry.Operations {
		if op.Guest == nil || op.Guest.Directive == nil || op.Guest.Directive.Command != command.Verb {
			continue
		}
		access := p.DirectiveAllowed(entry.Name, op.ID)
		if !access.Allowed {
			return namespaceDirectiveCall{}, fmt.Errorf("this command is unavailable; use Torana's UI or CLI")
		}
		input, err := directiveInput(*op.Guest, command.Args)
		if err != nil {
			return namespaceDirectiveCall{}, err
		}
		return namespaceDirectiveCall{Namespace: entry.Name, Operation: op.ID, Input: input, Confirm: access.Confirm, ConversationBinding: access.ConversationBinding}, nil
	}
	return namespaceDirectiveCall{}, fmt.Errorf("unknown command in %s; try torana> %s help", entry.Name, entry.Name)
}

func directiveInput(op plugin.AgentOperation, text string) (json.RawMessage, error) {
	if op.Directive == nil {
		return nil, fmt.Errorf("operation has no directive")
	}
	args, err := directiveWords(text)
	if err != nil {
		return nil, err
	}
	if len(args) > len(op.Directive.Args) {
		return nil, fmt.Errorf("too many command arguments")
	}
	if len(op.InputSchema) == 0 {
		if len(args) == 0 && len(op.Directive.Args) == 0 {
			return nil, nil // No-body operations stay no-body at dispatch.
		}
		return nil, fmt.Errorf("command schema is unavailable")
	}
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if len(op.InputSchema) > 0 && json.Unmarshal(op.InputSchema, &schema) != nil {
		return nil, fmt.Errorf("command schema is unavailable")
	}
	input := map[string]any{}
	for i, declared := range op.Directive.Args {
		name := strings.TrimSuffix(declared, "?")
		if i >= len(args) {
			if !strings.HasSuffix(declared, "?") {
				return nil, fmt.Errorf("missing command argument %s", name)
			}
			continue
		}
		switch schema.Properties[name].Type {
		case "string":
			input[name] = args[i]
		case "boolean":
			if args[i] != "true" && args[i] != "false" {
				return nil, fmt.Errorf("%s must be true or false", name)
			}
			input[name] = args[i] == "true"
		case "integer", "number":
			// JSON numbers preserve the literal and reject NaN/Infinity. The
			// normal operation validator enforces integer/range constraints.
			var value json.Number
			if json.Unmarshal([]byte(args[i]), &value) != nil || value == "" {
				return nil, fmt.Errorf("%s must be a number", name)
			}
			input[name] = value
		default:
			return nil, fmt.Errorf("command argument %s has no supported scalar type", name)
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("invalid command arguments")
	}
	if err := plugin.ValidateAgentPayload(op.InputSchema, encoded); err != nil {
		return nil, fmt.Errorf("invalid command arguments: %w", err)
	}
	return encoded, nil
}

// A small argument tokenizer supports spaces inside quoted strings without
// shell evaluation, environment interpolation, command substitution or globbing.
func directiveWords(text string) ([]string, error) {
	if len(text) > 4096 || !utf8.ValidString(text) {
		return nil, fmt.Errorf("command arguments are too long")
	}
	words := []string{}
	var word strings.Builder
	quote := rune(0)
	escaped, active := false, false
	flush := func() {
		if active {
			words = append(words, word.String())
			word.Reset()
			active = false
		}
	}
	for _, r := range text {
		if escaped {
			word.WriteRune(r)
			escaped = false
			active = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			active = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			flush()
			continue
		}
		word.WriteRune(r)
		active = true
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unfinished quote or escape in command arguments")
	}
	flush()
	return words, nil
}
