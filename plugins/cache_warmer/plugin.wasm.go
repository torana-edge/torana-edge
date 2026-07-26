// cache_warmer keeps a chosen conversation's prompt cache alive across an idle
// gap, so resuming work does not pay to rebuild a prefix that was about to be
// thrown away.
//
// # The arithmetic, and why this plugin refuses to run forever
//
// Holding an entry open costs one cache read per refresh. Letting it lapse
// costs one cache write on the next turn. So refreshing wins only while:
//
//	refreshes_spent  <  (write_rate / read_rate) - 1
//
// The prefix size cancels out of both sides — this is a pure price ratio,
// independent of how large the conversation is. On Anthropic's 5-minute tier,
// where writes cost 12.5x reads, that is about eleven refreshes: roughly
// forty-five minutes at the default interval.
//
// Past that point refreshing has cost more than the miss it was avoiding, and
// it keeps diverging rather than settling at break-even. That is the whole
// reason warming is opt-in per conversation and bounded by a deadline and a
// refresh budget. A plugin that quietly warmed everything forever would lose
// money on every conversation nobody came back to.
//
// # What it will not do
//
//   - Warm a provider whose cache lifetime does not refresh on read. There is
//     nothing to keep alive, and every refresh is pure cost.
//   - Warm without pricing. Unknown economics is exactly when guessing is most
//     expensive.
//   - Keep warming after a refresh reports a cache WRITE. That means the entry
//     had already lapsed and the refresh paid to rebuild it, so continuing
//     would be paying to hold something the user may never return to.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// warmEntry is everything needed to refresh one conversation, stored durably so
// a restart does not lose track of what it was keeping alive.
type warmEntry struct {
	ConversationID string `json:"conversation_id"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Path           string `json:"path"`

	// PrefixPB is the base64 protobuf of the cached prefix: the conversation
	// truncated at its last cache breakpoint. Storing the prefix rather than
	// the whole conversation keeps the refresh request small and means the
	// bytes sent are exactly the bytes the provider has cached.
	PrefixPB string `json:"prefix_pb"`

	LastRefreshMillis int64 `json:"last_refresh_millis"`
	LastSeenMillis    int64 `json:"last_seen_millis"`
	RefreshesSpent    int   `json:"refreshes_spent"`
	DeadlineMillis    int64 `json:"deadline_millis"`

	// Stopped records why warming ended, so an operator reading state can see
	// that it finished deliberately rather than silently failing.
	Stopped string `json:"stopped,omitempty"`
}

type config struct {
	// Conversations lists conversation IDs to keep warm, comma-separated.
	// Empty means the plugin observes but never spends — the safe default,
	// since warming everything is how this feature loses money.
	//
	// A string rather than an array because a config schema field renders as a
	// scalar in the control plane, and a value that disagrees with its declared
	// type is rejected before it reaches the plugin.
	Conversations string `json:"conversations"`
	// WarmForMinutes bounds how long after the last real turn a conversation
	// stays warm. Zero uses the break-even count alone.
	WarmForMinutes int `json:"warm_for_minutes"`
	// IntervalSecondsOverride replaces the provider's derived refresh cadence.
	// The host rejects an interval at or beyond the cache TTL, since such a
	// refresh always arrives after the entry has already expired.
	IntervalSecondsOverride int `json:"interval_seconds_override"`
}

func loadConfig() config {
	var cfg config
	if raw := sdk.PluginConfig(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg
}

// warms reports whether this conversation is opted in.
func (c config) warms(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	for _, id := range strings.Split(c.Conversations, ",") {
		if strings.TrimSpace(id) == conversationID {
			return true
		}
	}
	return false
}

func (c config) any() bool { return strings.TrimSpace(c.Conversations) != "" }

const entryPrefix = "warm/"

func init() {
	// Request path: remember the cached prefix of any conversation the operator
	// opted in. This hook only observes and stores — it never modifies the
	// request, so it cannot affect the prefix it is trying to preserve.
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		cfg := loadConfig()
		if !cfg.any() {
			return nil, nil
		}
		meta := readHostMeta(req)
		if meta.ConversationID == "" || !cfg.warms(meta.ConversationID) {
			return nil, nil
		}

		prefix := prefixThroughBreakpoint(req)
		if prefix == nil {
			// No breakpoint means no explicitly cached prefix to refresh.
			return nil, nil
		}
		encoded, err := sdk.EncodeRequest(prefix)
		if err != nil {
			return nil, nil
		}

		now := sdk.Now()
		entry := warmEntry{
			ConversationID: meta.ConversationID,
			Provider:       meta.Provider,
			Model:          req.Model,
			Path:           meta.Path,
			PrefixPB:       encoded,
			LastSeenMillis: now,
		}
		// A real turn resets the budget: the user is active, the cache was just
		// refreshed for free, and whatever was spent before is spent.
		if cfg.WarmForMinutes > 0 {
			entry.DeadlineMillis = now + int64(cfg.WarmForMinutes)*60_000
		}
		_ = sdk.StateSetJSON(entryPrefix+meta.ConversationID, entry)

		// Deliberately returns nil: this plugin must never mutate a request.
		return nil, nil
	})

	// Tick path: refresh whatever is still worth refreshing.
	sdk.OnTick(func(ctx context.Context, tick *pb.TickRequest) (*pb.TickResult, error) {
		cfg := loadConfig()
		keys, err := sdk.StateKeys()
		if err != nil || len(keys) == 0 {
			return nil, nil
		}

		refreshed := 0
		var notes []string
		for _, key := range keys {
			if len(key) <= len(entryPrefix) || key[:len(entryPrefix)] != entryPrefix {
				continue
			}
			var entry warmEntry
			if found, err := sdk.StateGetJSON(key, &entry); !found || err != nil {
				continue
			}
			if entry.Stopped != "" {
				continue
			}
			if !cfg.warms(entry.ConversationID) {
				// Opted out since the entry was written.
				_ = sdk.StateSet(key, "")
				continue
			}

			action, note := refreshOne(&entry, cfg, tick.UnixMillis)
			if note != "" {
				notes = append(notes, note)
			}
			if action {
				refreshed++
			}
			if entry.Stopped != "" {
				// Keep the stopped entry so an operator can see why it ended,
				// rather than having it vanish and look like it never ran.
				_ = sdk.StateSetJSON(key, entry)
				continue
			}
			_ = sdk.StateSetJSON(key, entry)
		}

		if refreshed == 0 && len(notes) == 0 {
			return nil, nil
		}
		return &pb.TickResult{
			Handled: true,
			Actions: int32(refreshed),
			Note:    joinNotes(notes),
		}, nil
	})
}

// refreshOne decides and, if warranted, sends. It mutates entry in place and
// reports whether a refresh was sent plus any human-readable outcome.
func refreshOne(entry *warmEntry, cfg config, now int64) (bool, string) {
	pricing, err := sdk.GetCachePricing(entry.Provider, entry.Model)
	if err != nil || !pricing.Available() {
		entry.Stopped = "pricing unavailable"
		return false, fmt.Sprintf("%s: stopped, pricing unavailable", short(entry.ConversationID))
	}
	if !pricing.Warmable() {
		// Automatic prefix caching: no lifetime the caller owns, so nothing a
		// request can keep alive.
		entry.Stopped = "provider cache does not refresh on read"
		return false, fmt.Sprintf("%s: stopped, %s cache cannot be refreshed", short(entry.ConversationID), entry.Provider)
	}

	if entry.DeadlineMillis > 0 && now >= entry.DeadlineMillis {
		entry.Stopped = "deadline reached"
		return false, fmt.Sprintf("%s: stopped, deadline reached", short(entry.ConversationID))
	}
	if pricing.BreakEvenRefreshes > 0 && entry.RefreshesSpent >= pricing.BreakEvenRefreshes {
		// Past this point refreshing costs more than the cache miss it avoids.
		entry.Stopped = "break-even reached"
		return false, fmt.Sprintf("%s: stopped after %d refreshes, past break-even",
			short(entry.ConversationID), entry.RefreshesSpent)
	}

	interval := int64(pricing.WarmIntervalSeconds) * 1000
	if cfg.IntervalSecondsOverride > 0 {
		interval = int64(cfg.IntervalSecondsOverride) * 1000
	}
	if interval <= 0 {
		entry.Stopped = "no refresh interval available"
		return false, ""
	}
	last := entry.LastRefreshMillis
	if last == 0 {
		last = entry.LastSeenMillis
	}
	if now-last < interval {
		return false, "" // not due yet
	}

	req, err := sdk.DecodeRequest(entry.PrefixPB)
	if err != nil {
		entry.Stopped = "stored prefix is unreadable"
		return false, ""
	}
	// A minimal trailing turn, after the last breakpoint so it is not part of
	// the cached prefix, and one output token. The point is to touch the entry,
	// not to get an answer.
	req.Messages = append(req.Messages, &pb.Message{Role: "user", Content: "."})
	one := int32(1)
	req.MaxTokens = &one

	res, err := sdk.SendRequest(req, sdk.SendRequestOptions{
		Provider: entry.Provider,
		Path:     entry.Path,
	})
	entry.LastRefreshMillis = now
	entry.RefreshesSpent++

	if err != nil {
		entry.Stopped = "refresh failed"
		return false, fmt.Sprintf("%s: stopped, refresh failed: %v", short(entry.ConversationID), err)
	}
	if res.CacheRebuilt() {
		// The entry had already lapsed and this refresh paid to recreate it.
		// Continuing would mean paying to hold a conversation nobody may return
		// to, which is exactly what the budget exists to prevent.
		entry.Stopped = "cache had already expired"
		return true, fmt.Sprintf("%s: stopped, cache had already expired", short(entry.ConversationID))
	}
	return true, ""
}

// prefixThroughBreakpoint returns a copy of the request truncated at its last
// cache breakpoint — the bytes the provider actually has cached.
func prefixThroughBreakpoint(req *pb.ChatRequest) *pb.ChatRequest {
	last := -1
	for i, m := range req.Messages {
		if len(m.CacheControlJson) > 0 {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	clone := &pb.ChatRequest{
		Model:    req.Model,
		Tools:    req.Tools,
		Messages: req.Messages[:last+1],
	}
	return clone
}

type hostMeta struct {
	Provider       string `json:"_provider"`
	ConversationID string `json:"_conversation_id"`
	Path           string `json:"_path"`
}

func readHostMeta(req *pb.ChatRequest) hostMeta {
	var meta hostMeta
	if len(req.ToranaMetaJson) == 0 {
		return meta
	}
	_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	return meta
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func joinNotes(notes []string) string {
	switch len(notes) {
	case 0:
		return ""
	case 1:
		return notes[0]
	}
	out := notes[0]
	for _, n := range notes[1:] {
		out += "; " + n
	}
	return out
}
