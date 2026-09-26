// Package annotate adds host-signed user notices and removes them from replayed
// assistant history before the provider or a plugin can see them.
package annotate

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/internal/suggest"
)

const (
	markerPurpose = "torana/notice/v1"
	beginPrefix   = "[torana:begin:"
	endPrefix     = "[torana:end:"
)

var ErrStripFailed = errors.New("signed Torana notice could not be removed")

type Signer interface {
	MAC(purpose, value string) ([]byte, error)
}

func signature(signer Signer, conversation, id string) (string, error) {
	if conversation == "" || id == "" {
		return "", errors.New("conversation and notice ID are required")
	}
	mac, err := signer.MAC(markerPurpose, conversation+"\x00"+id)
	if err != nil {
		return "", err
	}
	if len(mac) < 6 {
		return "", errors.New("notice MAC is too short")
	}
	return hex.EncodeToString(mac[:6]), nil // 48-bit marker, bound to conversation and ID
}

// Render returns a suffix for a completed, tool-free assistant turn. The
// caller decides whether this harness is approved for the notice channel.
func Render(signer Signer, conversation string, item suggest.Suggestion) (string, error) {
	sig, err := signature(signer, conversation, item.ID)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "\n\n%s%s:%s]\n", beginPrefix, item.ID, sig)
	fmt.Fprintf(&body, "[torana] Suggestion: %s\n", item.Title)
	if item.Body != "" {
		fmt.Fprintf(&body, "Why: %s\n", item.Body)
	}
	if item.CostUSD != nil {
		fmt.Fprintf(&body, "One-time cost estimate: $%.2f\n", *item.CostUSD)
	}
	if item.HarnessTargetModel != "" {
		fmt.Fprintf(&body, "Switch your harness to %s, or review this suggestion in Torana (torana suggestions list).\n", item.HarnessTargetModel)
	} else {
		body.WriteString("Review this suggestion in Torana (torana suggestions list).\n")
	}
	fmt.Fprintf(&body, "%s%s:%s]\n", endPrefix, item.ID, sig)
	return body.String(), nil
}

// RenderProbe uses the normal signed notice grammar without creating a real
// suggestion or a confirmation code. The host strips it from later history.
func RenderProbe(signer Signer, conversation string) (string, error) {
	const id = "probe_v1"
	sig, err := signature(signer, conversation, id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\n\n%s%s:%s]\n[torana] Notice probe: this is a local display test.\n%s%s:%s]\n", beginPrefix, id, sig, endPrefix, id, sig), nil
}

// Strip removes every valid, conversation-bound notice span. It tolerates
// reflow inside the span, but not changed delimiters. A valid start with no
// matching end is a strip failure; the caller must suppress future notices
// for that conversation rather than risk feeding notice text to the model.
func Strip(signer Signer, conversation, text string) (string, bool, error) {
	changed := false
	search := 0
	for search < len(text) {
		relative := strings.Index(text[search:], beginPrefix)
		if relative < 0 {
			break
		}
		start := search + relative
		markerEnd := strings.IndexByte(text[start:], ']')
		if markerEnd < 0 {
			break
		}
		markerEnd += start
		marker := text[start+len(beginPrefix) : markerEnd]
		id, supplied, ok := strings.Cut(marker, ":")
		if !ok || len(supplied) != 12 {
			search = markerEnd + 1
			continue
		}
		expected, err := signature(signer, conversation, id)
		if err != nil {
			return text, changed, err
		}
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
			search = markerEnd + 1
			continue
		}
		endMarker := endPrefix + id + ":" + supplied + "]"
		endRelative := strings.Index(text[markerEnd+1:], endMarker)
		if endRelative < 0 {
			return text, changed, ErrStripFailed
		}
		end := markerEnd + 1 + endRelative + len(endMarker)
		if end < len(text) && text[end] == '\n' {
			end++
		}
		removeStart := start
		if start >= 2 && text[start-2:start] == "\n\n" {
			removeStart -= 2
		}
		text = text[:removeStart] + text[end:]
		search = removeStart
		changed = true
	}
	return text, changed, nil
}
