package plugin

import (
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// v2 hook dispatch: envelope in, action out.
//
// v1 sent a bare message and treated any non-empty reply as a replacement. That
// made three things indistinguishable — "no opinion", "replace with this", and
// "I could not decode what you sent" — which is why v1 needed a `handled` flag
// on three of five result types and still could not express suppression.
//
// v2 wraps every payload in a HookInput and every reply in a HookResult. These
// helpers exist so the wrapping is written once: five call sites each doing
// their own marshal/unmarshal is how the v1 conventions drifted into eight
// different reply shapes.

// hostABIMinor is the minor version of the v2 contract this host speaks.
//
// Bump it when the host starts SENDING a field added to HookInput, never for
// host-internal changes. Guests read it to decide whether an additive field can
// be relied on; claiming a version whose fields are not populated is worse than
// claiming none.
const hostABIMinor uint32 = 0

// encodeHookInput builds the envelope for one dispatch.
//
// Mutability is NOT on the envelope. v2 moved it into AfterResponse, because
// only that hook can be immutable — a global flag invited every other hook to
// consult a field that is always true for it. Callers express it through
// responsePayload{mutable: …}.
func encodeHookInput(reqID uint64, payload isHookPayload) ([]byte, error) {
	in := &pbv2.HookInput{
		AbiMinor:  hostABIMinor,
		RequestId: reqID,
	}
	payload.applyTo(in)
	if err := in.Validate(); err != nil {
		// The host built this, so a failure here is a host bug, not a guest
		// one. Saying so stops it being reported as a misbehaving plugin.
		return nil, fmt.Errorf("host built an invalid hook input: %w", err)
	}
	return proto.Marshal(in)
}

// isHookPayload keeps the oneof selection at the call site, so a hook cannot be
// dispatched with another hook's payload by passing the wrong argument.
type isHookPayload interface{ applyTo(*pbv2.HookInput) }

type requestPayload struct{ req *pbv2.ChatRequest }
type responsePayload struct {
	resp    *pbv2.ChatResponse
	mutable bool
}
type streamPayload struct{ ev *pbv2.StreamEvent }
type httpPayload struct{ req *pbv2.HttpRequest }
type tickPayload struct{ tick *pbv2.TickRequest }

func (p requestPayload) applyTo(in *pbv2.HookInput) {
	in.Payload = &pbv2.HookInput_ChatRequest{ChatRequest: p.req}
}

func (p responsePayload) applyTo(in *pbv2.HookInput) {
	// mutable says whether a returned replacement will actually be applied.
	// False on the streamed and upstream-error paths, where the bytes have
	// already gone to the caller or there is no body to rewrite. v1 offered no
	// such signal, so a plugin learned its edits were discarded only by their
	// having no effect.
	in.Payload = &pbv2.HookInput_AfterResponse{
		AfterResponse: &pbv2.AfterResponse{Response: p.resp, Mutable: p.mutable},
	}
}

func (p streamPayload) applyTo(in *pbv2.HookInput) {
	in.Payload = &pbv2.HookInput_StreamEvent{StreamEvent: p.ev}
}

func (p httpPayload) applyTo(in *pbv2.HookInput) {
	in.Payload = &pbv2.HookInput_HttpRequest{HttpRequest: p.req}
}

func (p tickPayload) applyTo(in *pbv2.HookInput) {
	in.Payload = &pbv2.HookInput_TickRequest{TickRequest: p.tick}
}

// decodeHookResult turns a guest's reply into an action, or nil for
// pass-through.
//
// Zero bytes is pass-through and its ONLY encoding — it is a length, not a
// message, so it cannot be ambiguous. An all-defaults HookResult marshals to
// zero bytes too, which is why v2 has one spelling of "leave it alone" instead
// of v1's flag.
//
// Everything else is validated against the hook it answers before the caller
// sees it, so a misdispatched or malformed action is refused here rather than
// interpreted at five separate call sites.
func decodeHookResult(raw []byte, hook pbv2.Hook) (*pbv2.HookResult, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	res, err := pbv2.DecodeHookResult(raw)
	if err != nil {
		return nil, err
	}
	if err := res.ValidateFor(hook); err != nil {
		return nil, err
	}
	if res.Action == nil {
		// A frame that carried bytes but no action. Validation accepts it as
		// an empty result; treat it as pass-through, the same as zero bytes.
		return nil, nil
	}
	return res, nil
}

// manifestHooks converts declared manifest hook names into the v2 enum.
//
// An unknown name is refused rather than skipped. Skipping would enable the
// plugin with fewer hooks than its manifest claims, and the manifest is what
// the operator approved — a typo would silently narrow what was authorised
// instead of failing where someone can see it.
func manifestHooks(hooks []Hook) ([]pbv2.Hook, error) {
	out := make([]pbv2.Hook, 0, len(hooks))
	for _, h := range hooks {
		hk, ok := sdk.ManifestHookName(h.Name)
		if !ok {
			return nil, fmt.Errorf("manifest declares unknown hook %q", h.Name)
		}
		out = append(out, hk)
	}
	return out, nil
}
