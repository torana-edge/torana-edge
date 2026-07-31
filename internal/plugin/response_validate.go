package plugin

import (
	"fmt"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// validateResponseReplacement reports whether a plugin's replace_response output
// is a valid mutation of the accepted response. These are RELATIVE constraints
// the SDK cannot check (it sees the replacement alone): content presence is
// host-owned (fixed across accepted->output) and tool_calls has fixed
// cardinality with positional correspondence.
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
	return nil
}
