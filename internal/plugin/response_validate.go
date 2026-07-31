package plugin

import (
	"bytes"
	"fmt"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// validateResponseReplacement reports whether a plugin's replace_response output
// is a valid mutation of the accepted response. These are RELATIVE constraints
// the SDK cannot check (it sees the replacement alone): content presence is
// host-owned (fixed across accepted->output), tool_calls has fixed cardinality
// with positional correspondence, and the observed provider/host facts (model,
// id, usage, signatures) are immutable under plugin mutation.
func validateResponseReplacement(current, replacement *pbv2.ChatResponse) error {
	// A nil replacement is pass-through, not a mutation to judge.
	if replacement == nil {
		return nil
	}
	// Message presence is fixed. A response IS one message; dropping it is a
	// structural lie and inventing one where the provider sent none fabricates
	// an assistant turn.
	if replacement.Message == nil && current.Message != nil {
		return fmt.Errorf("response replacement dropped the assistant message")
	}
	if replacement.Message != nil && current.Message == nil {
		return fmt.Errorf("invented an assistant message")
	}
	if current.Message == nil { // both messages absent: nothing relative left to check
		return nil
	}
	// Content presence is host-owned: the provider body either has a writable
	// text slot or it does not, and no plugin changes that. Only the value may
	// change (present-empty and present-nonempty are the same presence).
	if (replacement.Message.Content != nil) != (current.Message.Content != nil) {
		return fmt.Errorf("changed content presence")
	}
	// Tool calls have fixed cardinality with positional correspondence: output
	// element N mutates accepted element N, so the counts must agree.
	if len(replacement.Message.ToolCalls) != len(current.Message.ToolCalls) {
		return fmt.Errorf("changed tool-call cardinality")
	}
	// The ChatResponse facts below are host-owned: observed provider/host
	// measurements (model, id, finish reason, usage, upstream status, duration)
	// and opaque provider output (extensions). Re-emitting them identically is
	// fine; changing any of them lies about the exchange — request grants do
	// not authorise forging what the provider reported. Compared field-wise:
	// reflect.DeepEqual on proto messages is a false oracle because of the
	// unexported runtime state they carry.
	if replacement.Model != current.Model {
		return fmt.Errorf("changed host-owned field model")
	}
	if replacement.Id != current.Id {
		return fmt.Errorf("changed host-owned field id")
	}
	if replacement.FinishReason != current.FinishReason {
		return fmt.Errorf("changed host-owned field finish_reason")
	}
	if !usageEqual(current.Usage, replacement.Usage) {
		return fmt.Errorf("changed host-owned field usage")
	}
	if replacement.UpstreamStatus != current.UpstreamStatus {
		return fmt.Errorf("changed host-owned field upstream_status")
	}
	if replacement.DurationMs != current.DurationMs {
		return fmt.Errorf("changed host-owned field duration_ms")
	}
	if !bytes.Equal(replacement.ProviderExtensionsJson, current.ProviderExtensionsJson) {
		return fmt.Errorf("changed host-owned field provider_extensions_json")
	}
	// Positional tool calls: id is the provider's identity for the call and is
	// host-owned; name and arguments are assistant-writable in place. The bound
	// signature is host-owned with TWO exceptions — clearing it is the prescribed
	// response to changing the content it covers, and leaving it UNCHANGED over
	// changed content is also valid: the apply block itself invalidates the
	// token (invalidateSignature) before it could ship, so a pass-through
	// signature never signs content the provider didn't. What stays rejected is
	// outright provenance fraud: dropping the token over unchanged content,
	// replacing it with another non-empty token, or minting one where the
	// provider sent none.
	for i, cur := range current.Message.ToolCalls {
		rep := replacement.Message.ToolCalls[i]
		if rep.Id != cur.Id {
			return fmt.Errorf("tool call %d changed host-owned id", i)
		}
		nameOrArgsChanged := rep.Name != cur.Name || !bytes.Equal(rep.ArgumentsJson, cur.ArgumentsJson)
		class := outboundpolicy.ClassifySignatureMutation(cur.Signature, rep.Signature, nameOrArgsChanged)
		if !class.Allowed() && class != outboundpolicy.SignatureStale {
			return fmt.Errorf("tool call %d signature %s", i, class)
		}
	}
	return nil
}

// usageEqual compares the provider token tallies field-wise. Both-nil and
// both-non-nil with equal counts are the only accepted states: usage presence
// is itself a host-owned fact (a plugin inventing a Usage block where the
// provider reported none forges the bill). The unexported proto runtime state
// makes reflect.DeepEqual unreliable here.
func usageEqual(current, replacement *pbv2.Usage) bool {
	if current == nil || replacement == nil {
		return current == replacement
	}
	return current.InputTokens == replacement.InputTokens &&
		current.OutputTokens == replacement.OutputTokens &&
		current.CacheReadTokens == replacement.CacheReadTokens &&
		current.CacheWriteTokens == replacement.CacheWriteTokens
}
