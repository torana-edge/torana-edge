package proxy

import "context"

// responseHookContext models the invariant supplied by every real HTTP entry
// point: response hooks operate inside the durable request-state lifecycle.
// Tests that intentionally exercise missing state call runJSONResponseHooks
// with context.Background directly.
func responseHookContext(id uint64) context.Context {
	return context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: id})
}
