package proxy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/torana-edge/torana-edge/internal/directive"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

func TestDirectiveArgumentsAreTypedAndValidated(t *testing.T) {
	op := plugin.AgentOperation{Directive: &plugin.AgentDirective{Args: []string{"step", "turns?", "enabled?"}}, InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"string"},"turns":{"type":"integer","enum":[1,3,10]},"enabled":{"type":"boolean"}},"required":["step"],"additionalProperties":false}`)}
	for _, tc := range []struct{ text, want string }{{`"a step" 3 true`, `{"enabled":true,"step":"a step","turns":3}`}, {`'$HOME $(echo x)'`, `{"step":"$HOME $(echo x)"}`}, {`''`, `{"step":""}`}} {
		got, err := directiveInput(op, tc.text)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s: got %s err %v want %s", tc.text, got, err, tc.want)
		}
	}
	for _, text := range []string{"", "step 0", "step 11", "step 1.5", "step NaN", "step 1 yes", "step 1 true extra", "'unfinished", "step \\"} {
		if got, err := directiveInput(op, text); err == nil {
			t.Fatalf("invalid args accepted: %q => %s", text, got)
		}
	}
}

func TestDirectiveWordsNoShellExpansion(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{`one "two words" 'three words'`, []string{"one", "two words", "three words"}},
		{`one\ two "a\"b"`, []string{"one two", `a"b`}},
		{`"" '' literal* $HOME $(cmd)`, []string{"", "", "literal*", "$HOME", "$(cmd)"}},
		{`'a\b'`, []string{`a\b`}},
	} {
		got, err := directiveWords(tc.text)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %q %v", tc.text, got, err)
		}
	}
}

func TestDirectiveNoBodyOperation(t *testing.T) {
	op := plugin.AgentOperation{Directive: &plugin.AgentDirective{Command: "status"}}
	got, err := directiveInput(op, "")
	if err != nil || got != nil {
		t.Fatalf("no-body operation got %s: %v", got, err)
	}
	if _, err := directiveInput(op, "extra"); err == nil {
		t.Fatal("no-body operation accepted arguments")
	}
}

func TestNamespaceDirectiveUsesCanonicalOperationAndUserPolicy(t *testing.T) {
	p := catalogTestPolicy(t, 1)
	entry := p.registry.entries["logger"]
	op := plugin.AgentOperation{ID: "route.pin", Risk: "write", ModelAccess: "never", Directive: &plugin.AgentDirective{Command: "pin", Args: []string{"step"}, UserDirect: true}, InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"string"}},"required":["step"]}`)}
	entry.Operations = append(entry.Operations, namespaceOperation{ID: op.ID, Risk: op.Risk, ModelAccess: op.ModelAccess, Callable: true, Source: "plugin", Guest: &op, ConversationBinding: "required"})
	p.registry.entries["logger"] = entry
	command := directive.Command{Namespace: "logs", Verb: "pin", Args: "small", Known: true}
	call, err := p.resolveNamespaceDirective(command)
	if err != nil || call.Namespace != "logger" || call.Operation != "route.pin" || call.Confirm || call.ConversationBinding != "required" || string(call.Input) != `{"step":"small"}` {
		t.Fatalf("call=%+v err=%v", call, err)
	}
	op.Directive.UserDirect = false
	call, err = p.resolveNamespaceDirective(command)
	if err != nil || !call.Confirm {
		t.Fatalf("write bypassed consent: %+v %v", call, err)
	}
	p.protected["logger"] = true
	if _, err = p.resolveNamespaceDirective(command); err == nil {
		t.Fatal("protected write accepted")
	}
	delete(p.protected, "logger")
	entry.Operations[len(entry.Operations)-1].Callable = false
	p.registry.entries["logger"] = entry
	if _, err = p.resolveNamespaceDirective(command); err == nil {
		t.Fatal("unavailable operation accepted")
	}
	command.Verb = "/agent/route"
	if _, err = p.resolveNamespaceDirective(command); err == nil {
		t.Fatal("HTTP route accepted as command")
	}
}
