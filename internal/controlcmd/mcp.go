package controlcmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/controlclient"
)

func (c *runner) mcpToken(rotate bool) (string, error) {
	path := controlclient.BasePath + "/mcp/token"
	if rotate {
		path += "/rotate"
	}
	raw, _, err := c.client.JSON(c.ctx, http.MethodPost, path, []byte(`{}`), "")
	if err != nil {
		return "", err
	}
	var result struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return "", fmt.Errorf("Torana returned an invalid MCP token response")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(result.Token)
	if err != nil || len(decoded) != 32 || len(result.Token) != 43 {
		return "", fmt.Errorf("Torana returned an invalid MCP token response")
	}
	return result.Token, nil
}

func (c *runner) mcp(command string, o options) error {
	if command == "mcp token" || command == "mcp rotate" {
		token, err := c.mcpToken(command == "mcp rotate")
		if err != nil {
			return err
		}
		if o.json {
			return output(c.stdout, struct {
				Token string `json:"token"`
			}{token})
		}
		_, err = fmt.Fprintln(c.stdout, token)
		return err
	}
	cfg, revision, err := c.config()
	if err != nil {
		return err
	}
	if command != "mcp status" {
		if command == "mcp enable" {
			// Prepare authentication before opening the endpoint. The credential
			// stays private: enable's success output is status, not the token.
			if _, err := c.mcpToken(false); err != nil {
				return err
			}
		}
		cfg.MCP.Enabled = command == "mcp enable"
		body, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, _, err := c.client.JSON(c.ctx, http.MethodPut, controlclient.BasePath+"/config", body, revision); err != nil {
			return err
		}
	}
	return output(c.stdout, struct {
		Enabled bool `json:"enabled"`
	}{cfg.MCP.Enabled})
}
