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
// The two arms form a SUM: a result is either a value or a refusal, never
// both. The fields are private so proxy callbacks can only build results
// through the constructors, which make the two-arm state unrepresentable;
// Validate remains as the dispatcher's defensive net (and as the in-package
// path for tests that deliberately construct malformed results).
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
	value   []byte
	refusal *pbv2.HostError
}

// Value returns the domain body. Nil with no refusal is a successful EMPTY
// value (an ack), distinct from a refusal and from absence — the same
// distinction the framed envelope preserves.
func (r ExtensionResult) Value() []byte {
	return r.value
}

// Refusal returns the classified HostError, or nil when the result is not a
// refusal.
func (r ExtensionResult) Refusal() *pbv2.HostError {
	return r.refusal
}

// ExtensionValue builds a successful result carrying a domain body. nil bytes
// are a valid empty success (an ack), distinct from a refusal and from
// absence — the same distinction the framed envelope preserves.
func ExtensionValue(value []byte) ExtensionResult {
	return ExtensionResult{value: value}
}

// ExtensionRefusal builds a classified refusal. The code MUST be a real
// classification; UNSPECIFIED and unknown numeric codes are rejected by
// Validate.
func ExtensionRefusal(code pbv2.ErrorCode, format string, args ...any) ExtensionResult {
	return ExtensionResult{refusal: &pbv2.HostError{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
	}}
}

// Validate reports whether the result is well-formed: a refusal and a value
// are mutually exclusive, and a refusal must carry a classified code.
// Both-nil is a valid empty success. A PRESENT value — even a zero-length
// one — alongside a refusal is malformed, because the guest cannot tell
// which arm to trust. Refusal codes are delegated to HostError.Validate,
// which rejects UNSPECIFIED and unknown numeric codes.
func (r ExtensionResult) Validate() error {
	if r.refusal != nil {
		if r.value != nil {
			return fmt.Errorf("extension result carries both a value and a refusal")
		}
		return r.refusal.Validate()
	}
	return nil
}

// applyExtensionResult validates a callback's result and frames it into the
// (value, HostError) pair the dispatcher marshals. An invalid result — both
// arms set (including a present empty value alongside a refusal), an
// UNSPECIFIED refusal code, or an unknown numeric code — is a host-side bug,
// not a guest input: it becomes a framed INTERNAL refusal and a loud log
// line, so the guest never sees a malformed envelope it cannot branch on.
func (r *Runtime) applyExtensionResult(cmd string, ext ExtensionResult) ([]byte, *pbv2.HostError) {
	if err := ext.Validate(); err != nil {
		log.Printf("extension %s: callback returned an invalid result: %v", cmd, err)
		return nil, hostErr(pbv2.ErrorCode_ERROR_CODE_INTERNAL, "extension callback returned an invalid result")
	}
	return ext.Value(), ext.Refusal()
}
