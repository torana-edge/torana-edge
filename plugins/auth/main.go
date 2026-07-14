package main

import (
	"context"

	"encoding/json"
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

type VerifyResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if req.ToranaMetaJson == nil {
			req.ToranaMetaJson = []byte(`{}`)
		}

		var meta map[string]any
		if err := json.Unmarshal(req.ToranaMetaJson, &meta); err != nil {
			return nil, nil // skip on err
		}

		// Try to extract headers
		headersRaw, ok := meta["_request_headers"]
		if !ok {
			return nil, nil
		}

		headers, ok := headersRaw.(map[string]any)
		if !ok {
			return nil, nil
		}

		// 2. Standard Headers: Parse Authorization: Bearer <jwt>, x-api-key, or x-torana-user.
		var token string
		var isVirtualKey bool
		var isJWT bool

		if authHeader, ok := headers["Authorization"].(string); ok && strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
			isJWT = true
		} else if apiKey, ok := headers["X-Api-Key"].(string); ok {
			token = apiKey
			if strings.HasPrefix(apiKey, "sk-torana-") {
				isVirtualKey = true
			}
		} else if apiKey, ok := headers["x-api-key"].(string); ok {
			token = apiKey
			if strings.HasPrefix(apiKey, "sk-torana-") {
				isVirtualKey = true
			}
		} else if toranaUser, ok := headers["x-torana-user"].(string); ok {
			// Extract simple user string
			meta["tenant_id"] = "default-tenant"
			meta["user_id"] = toranaUser
		}

		// 3. Normalization: Extract tenant_id, team_id, and user_id.
		if isVirtualKey {
			// 1. Auth Plugin: Have the plugins/auth plugin parse the sk-torana- key,
			// validate it via a host function env.verify_virtual_key, and inject the
			// associated identity metadata into the pipeline.
			resStr, err := sdk.HostCall("verify_virtual_key", token)
			if err == nil && resStr != "" {
				var vres VerifyResponse
				if err := json.Unmarshal([]byte(resStr), &vres); err == nil && vres.Status == "ok" {
					meta["tenant_id"] = vres.TenantID
					meta["team_id"] = vres.TeamID
					meta["user_id"] = vres.UserID
				}
			}
		} else if isJWT {
			// Enterprise auth is not available in the open-source edition.
			// Real JWT verification lives in torana-edge/private-nucleus.
			return nil, nil
		}

		// Save updated ToranaMeta back
		metaBytes, err := json.Marshal(meta)
		if err == nil {
			req.ToranaMetaJson = metaBytes
			return req, nil
		}

		return nil, nil
	})
}
