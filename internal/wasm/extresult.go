package wasm

import (
	"fmt"
	"log"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// ExtensionResult is the classified outcome of an extension host call
// (torana_send_request, torana_cache_pricing, torana_offload_completion,
// verify_virtual_key — every callback that can refuse).
//
// Exactly one arm is meaningful:
//
//   - Value carries the domain body (the opaque JSON the guest decodes). Nil
//     with no refusal is a successful EMPTY value — an acknowledgement.
//   - Refusal carries a classified *HostError, framed into the error arm so
//     the guest SDK branches on the code, never on a status string smuggled
//     through the value arm.
//
// The (value, refusal) exclusivity and the code classification are enforced
// here, in one place, rather than by every callback adopting the convention
// on its own. The dispatcher validates every callback's result via Validate
// before framing; an invalid one becomes a framed INTERNAL refusal with a
// loud log, so a misbehaving host callback cannot leak a malformed envelope
// to the guest.
type ExtensionResult struct {
	Value   []byte
	Refusal *pbv2.HostError
}

// ExtensionValue builds a successful result carrying a domain body. nil bytes
// are a valid empty success (an ack), distinct from a refusal and from
// absence — the same distinction the framed envelope preserves.
func ExtensionValue(value []byte) ExtensionResult {
	return ExtensionResult{Value: value}
}

// ExtensionRefusal builds a classified refusal. The code MUST be a real
// classification; UNSPECIFIED is rejected by Validate.
func ExtensionRefusal(code pbv2.ErrorCode, format string, args ...any) ExtensionResult {
	return ExtensionResult{Refusal: &pbv2.HostError{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
	}}
}

// Validate reports whether the result is well-formed: a refusal and a value
// are mutually exclusive, and a refusal must carry a classified code.
// Both-nil is a valid empty success.
func (r ExtensionResult) Validate() error {
	if r.Refusal != nil {
		if len(r.Value) > 0 {
			return fmt.Errorf("extension result carries both a value and a refusal")
		}
		if r.Refusal.Code == pbv2.ErrorCode_ERROR_CODE_UNSPECIFIED {
			return fmt.Errorf("extension refusal has no code; UNSPECIFIED is not a classification")
		}
	}
	return nil
}

// applyExtensionResult validates a callback's result and frames it into the
// (value, HostError) pair the dispatcher marshals. An invalid result — both
// arms set, or an UNSPECIFIED refusal code — is a host-side bug, not a guest
// input: it becomes a framed INTERNAL refusal and a loud log line, so the
// guest never sees a malformed envelope it cannot branch on.
func (r *Runtime) applyExtensionResult(cmd string, ext ExtensionResult) ([]byte, *pbv2.HostError) {
	if err := ext.Validate(); err != nil {
		log.Printf("extension %s: callback returned an invalid result: %v", cmd, err)
		return nil, hostErr(pbv2.ErrorCode_ERROR_CODE_INTERNAL, "extension callback returned an invalid result")
	}
	return ext.Value, ext.Refusal
}
