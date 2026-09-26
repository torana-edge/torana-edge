package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

type namespaceInvokeInput struct {
	Namespace string          `json:"namespace"`
	Operation string          `json:"operation"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// operationCall is host-resolved: no caller chooses a route, digest, source or
// conversation ID. Keep it intact when passing to execution or consent so
// neither path can silently resolve a newer plugin than the one reviewed.
type operationCall struct {
	Entry     namespaceEntry
	Operation namespaceOperation
	Input     json.RawMessage
	Binding   plugin.MCPBinding
	Catalog   []catalogNamespace
}

type operationDispatch struct {
	policy  *namespaceAccessPolicy
	execute func(context.Context, operationCall) (any, *mcpserver.DomainError, error)
	propose func(context.Context, operationCall) (mcpserver.Result, error)
}

func operationError(code, message string) mcpserver.Result {
	return mcpserver.Result{Error: &mcpserver.DomainError{Code: code, Message: message}}
}

func (d *operationDispatch) invoke(ctx context.Context, raw json.RawMessage, binding plugin.MCPBinding) (mcpserver.Result, error) {
	var input namespaceInvokeInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var trailing any
	if len(raw) > 64<<10 || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") || decoder.Decode(&input) != nil || decoder.Decode(&trailing) != io.EOF || input.Namespace == "" || input.Operation == "" {
		return operationError("invalid_input", "Choose a namespace and operation, with an optional input object."), nil
	}
	return d.dispatch(ctx, input, binding, false)
}

// invokeDirective receives the resolver's typed input, not raw command text.
// Recheck policy here too: command resolution is not an execution grant.
func (d *operationDispatch) invokeDirective(ctx context.Context, call namespaceDirectiveCall, binding plugin.MCPBinding) (mcpserver.Result, error) {
	return d.dispatch(ctx, namespaceInvokeInput{Namespace: call.Namespace, Operation: call.Operation, Input: call.Input}, binding, true)
}

func (d *operationDispatch) dispatch(ctx context.Context, input namespaceInvokeInput, binding plugin.MCPBinding, userDirective bool) (mcpserver.Result, error) {
	if d == nil || d.policy == nil || d.policy.registry == nil {
		return operationError("not_configured", "Torana operations are unavailable."), nil
	}
	entry, exists := d.policy.registry.resolve(input.Namespace, false)
	if !exists {
		return operationError("unknown_namespace", "Use a canonical namespace from torana_namespaces."), nil
	}
	_, op, exists := d.policy.lookup(entry.Name, input.Operation, false)
	if !exists {
		return operationError("unknown_operation", "Describe this namespace to find an available operation."), nil
	}
	confirm := false
	if userDirective {
		access := d.policy.DirectiveAllowed(entry.Name, op.ID)
		if !access.Allowed {
			return operationError("access_denied", "Use Torana's UI or CLI for this operation."), nil
		}
		confirm = access.Confirm
	} else {
		if !op.Callable && !d.policy.floor(entry, op) {
			result := operationError("namespace_unavailable", "This operation is not currently available.")
			result.Error.Details = &mcpserver.ErrorDetails{Status: entry.Status}
			return result, nil
		}
		access := d.policy.ModelReachable(entry.Name, op.ID)
		if access == "never" {
			return operationError("access_denied", "Use Torana's UI or CLI for this operation."), nil
		}
		confirm = access == "confirm"
	}
	var object map[string]json.RawMessage
	if len(input.Input) == 0 {
		input.Input = json.RawMessage(`{}`)
	}
	if json.Unmarshal(input.Input, &object) != nil || object == nil {
		return operationError("invalid_input", "Operation input must be a JSON object."), nil
	}
	schema := json.RawMessage(`{"type":"object","additionalProperties":false}`)
	if op.Guest != nil {
		schema = op.Guest.InputSchema
		if len(schema) == 0 && len(object) == 0 {
			input.Input = nil
		}
	} else if op.ID == "_config.set" {
		// The consent executor validates the candidate against the plugin's
		// configuration schema and the operator revision before proposing it.
		schema = arbitraryObjectSchema
	}
	if err := plugin.ValidateAgentPayload(schema, input.Input); err != nil {
		// Descriptor-authored schema text can contain arbitrary content. Do
		// not echo its error to a model or reuse directive-local diagnostics.
		result := operationError("invalid_input", "Input does not match the operation's declared schema; describe it and try again.")
		return result, nil
	}
	ctx, err := plugin.WithMCPBinding(ctx, binding)
	if err != nil {
		return operationError("unbound_conversation", "Torana could not verify this call's conversation."), nil
	}
	if op.ConversationBinding == "required" && !binding.Bound {
		result := operationError("unbound_conversation", "This operation needs the current conversation; retry after Torana observes the tool call.")
		result.Error.Retryable = true
		return result, nil
	}
	call := operationCall{Entry: entry, Operation: op, Input: append(json.RawMessage(nil), input.Input...), Binding: binding}
	if op.Source == "core" && op.ID == "plugins.list" {
		call.Catalog = []catalogNamespace{}
		for _, item := range d.policy.registry.list() {
			if item.Name != "torana" {
				call.Catalog = append(call.Catalog, catalogNamespace{Name: item.Name, Title: catalogText(item.Title, 60), Summary: catalogText(item.Summary, 300), Status: item.Status, Categories: append([]string(nil), item.Categories...)})
			}
		}
	}
	if confirm {
		if d.propose == nil {
			return operationError("not_configured", "User confirmation is not configured."), nil
		}
		result, err := d.propose(ctx, call)
		result.Namespace, result.Operation = entry.Name, op.ID
		return result, err
	}
	if d.execute == nil {
		return operationError("not_configured", "Operation execution is not configured."), nil
	}
	output, domainError, err := d.execute(ctx, call)
	if domainError != nil || err != nil {
		output = nil
	}
	return mcpserver.Result{OK: domainError == nil && err == nil, Namespace: entry.Name, Operation: op.ID, Result: output, Error: domainError, Conversation: &mcpserver.ConversationBinding{Binding: map[bool]string{true: "bound", false: "unbound"}[binding.Bound]}}, err
}
