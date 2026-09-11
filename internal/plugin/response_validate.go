package plugin

import (
	"bytes"
	"fmt"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// validateResponseReplacement reports whether a plugin's replace_response output
// is a valid mutation of the accepted response. These are RELATIVE constraints
// the SDK cannot check (it sees the replacement alone): blocks have fixed
// cardinality, arm topology, and positional correspondence, and the observed provider/host facts (model,
// id, usage, signatures) are immutable under plugin mutation.
func validateResponseReplacement(current, replacement *pbv1.ChatResponse) error {
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
	// The ChatResponse facts below are host-owned: observed provider/host
	// measurements (model, id, finish reason, usage, upstream status, duration)
	// and opaque provider output (extensions). Re-emitting them identically is
	// fine; changing any of them lies about the exchange — request grants do
	// not authorise forging what the provider reported. Compared field-wise:
	// reflect.DeepEqual on proto messages is a false oracle because of the
	// unexported runtime state they carry.
	//
	// These run even when BOTH messages are absent: a mutable no-message
	// response (e.g. a no-candidate Gemini body) must not let a plugin forge
	// the response facts just because there is no assistant turn to compare.
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
	if current.Message == nil { // both messages absent: message-relative checks below have nothing to compare
		return nil
	}
	if len(replacement.Message.Blocks) != len(current.Message.Blocks) {
		return fmt.Errorf("changed response-block cardinality")
	}
	// Positional blocks cannot change arm. For tool calls, id is the provider's identity for the call and is
	// host-owned; name and arguments are assistant-writable in place. The bound
	// signature is host-owned with TWO exceptions — clearing it is the prescribed
	// response to changing the content it covers, and leaving it UNCHANGED over
	// changed content is also valid: the apply block itself invalidates the
	// token (invalidateSignature) before it could ship, so a pass-through
	// signature never signs content the provider didn't. What stays rejected is
	// outright provenance fraud: dropping the token over unchanged content,
	// replacing it with another non-empty token, or minting one where the
	// provider sent none.
	for i, curBlock := range current.Message.Blocks {
		repBlock := replacement.Message.Blocks[i]
		if curBlock == nil || repBlock == nil {
			return fmt.Errorf("response block %d is nil", i)
		}
		switch cur := curBlock.Kind.(type) {
		case *pbv1.ResponseBlock_Text:
			if _, ok := repBlock.Kind.(*pbv1.ResponseBlock_Text); !ok {
				return fmt.Errorf("response block %d changed arm", i)
			}
		case *pbv1.ResponseBlock_ToolCall:
			rep, ok := repBlock.Kind.(*pbv1.ResponseBlock_ToolCall)
			if !ok || cur.ToolCall == nil || rep.ToolCall == nil {
				return fmt.Errorf("response block %d changed arm", i)
			}
			if rep.ToolCall.Id != cur.ToolCall.Id {
				return fmt.Errorf("tool call at block %d changed host-owned id", i)
			}
			nameOrArgsChanged := rep.ToolCall.Name != cur.ToolCall.Name || !bytes.Equal(rep.ToolCall.ArgumentsJson, cur.ToolCall.ArgumentsJson)
			class := outboundpolicy.ClassifySignatureMutation(cur.ToolCall.Signature, rep.ToolCall.Signature, nameOrArgsChanged)
			if !class.Allowed() && class != outboundpolicy.SignatureStale {
				return fmt.Errorf("tool call at block %d signature %s", i, class)
			}
		default:
			return fmt.Errorf("response block %d has unknown arm %T", i, cur)
		}
	}
	return nil
}

// clearStaleSignatures normalizes an ACCEPTED replacement before it becomes
// the next plugin's input: any tool call whose provider token was left
// UNCHANGED while its covered content (name or arguments) changed is a stale
// signature — the pipeline must not carry provenance over content the provider
// never signed, even though the apply block will clear the wire token later.
//
// Must be called only after validateResponseReplacement returned nil, so a
// later violation can never partially normalize an output that is being
// rejected: rejection is whole-replacement, normalization is whole-replacement.
// A guest that already cleared the token after a covered mutation is untouched
// (nothing to normalize); forged/added/dropped tokens never reach here.
func clearStaleSignatures(current, replacement *pbv1.ChatResponse) {
	if current.Message == nil || replacement.Message == nil {
		return
	}
	for i := range replacement.Message.Blocks {
		if i >= len(current.Message.Blocks) {
			return // unreachable post-validation; defensive
		}
		curBlock, curOK := current.Message.Blocks[i].Kind.(*pbv1.ResponseBlock_ToolCall)
		repBlock, repOK := replacement.Message.Blocks[i].Kind.(*pbv1.ResponseBlock_ToolCall)
		if !curOK || !repOK {
			continue
		}
		cur, rep := curBlock.ToolCall, repBlock.ToolCall
		if cur == nil || rep == nil || rep.Signature == "" {
			continue
		}
		if rep.Signature != cur.Signature {
			continue // forged/added — rejected upstream, never reaches here
		}
		nameOrArgsChanged := rep.Name != cur.Name || !bytes.Equal(rep.ArgumentsJson, cur.ArgumentsJson)
		if nameOrArgsChanged {
			rep.Signature = ""
		}
	}
}

// usageEqual compares the provider token tallies field-wise. Both-nil and
// both-non-nil with equal counts are the only accepted states: usage presence
// is itself a host-owned fact (a plugin inventing a Usage block where the
// provider reported none forges the bill). The unexported proto runtime state
// makes reflect.DeepEqual unreliable here.
func usageEqual(current, replacement *pbv1.Usage) bool {
	if current == nil || replacement == nil {
		return current == replacement
	}
	return current.InputTokens == replacement.InputTokens &&
		current.OutputTokens == replacement.OutputTokens &&
		current.CacheReadTokens == replacement.CacheReadTokens &&
		current.CacheWriteTokens == replacement.CacheWriteTokens
}
