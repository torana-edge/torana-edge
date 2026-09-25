package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/annotate"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func (s *Server) renderDirectiveReply(ctx context.Context, f *format.Format, chat *engine.ChatRequest, conversation string, requestBody []byte, message string) (*BlockResponse, error) {
	if s.secrets == nil || conversation == "" {
		return nil, fmt.Errorf("signed directive reply requires a conversation and secret store")
	}
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, err
	}
	id := "local_" + hex.EncodeToString(entropy[:])
	text, err := annotate.RenderLocalReply(s.secrets, conversation, id, message)
	if err != nil {
		return nil, err
	}
	responseID := "torana_" + hex.EncodeToString(entropy[:])
	if isOpenAIResponsesRequest(chat) {
		previous := ""
		if raw, ok := rawJSONSpan(requestBody, "previous_response_id"); ok {
			if err := json.Unmarshal(raw, &previous); err != nil {
				return nil, fmt.Errorf("invalid previous response ID")
			}
		}
		responseID, err = annotate.EncodeLocalResponseID(s.secrets, previous)
		if err != nil {
			return nil, err
		}
	}
	return renderRespondWithID(ctx, f, chat, &wasm.RespondVerdict{Response: &pb.SyntheticResponse{
		Message:      &pb.ResponseMessage{Blocks: []*pb.ResponseBlock{{Kind: &pb.ResponseBlock_Text{Text: &pb.ResponseTextBlock{Text: text}}}}},
		FinishReason: "stop",
	}}, responseID)
}
