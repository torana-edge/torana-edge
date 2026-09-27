package suggest

import (
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

func TestTranscriptTicketClaimPersistsAcrossStores(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	expires := time.Now().Add(time.Minute)
	if err := New(state).ClaimTranscriptTicket("nonce", expires); err != nil {
		t.Fatal(err)
	}
	if err := New(state).ClaimTranscriptTicket("nonce", expires); err == nil {
		t.Fatal("durably consumed ticket was accepted again")
	}
	if err := New(state).ClaimTranscriptTicket("expired", time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expired ticket was accepted")
	}
}
