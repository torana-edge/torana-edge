package controlcmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/resultrelease"
)

func (c *runner) resultApproval(command string, o options) error {
	if command == "approvals list" {
		return c.read("/approvals?cursor=" + url.QueryEscape(o.cursor))
	}
	ref := o.args[0]
	if !resultrelease.ValidReference(ref) {
		return fmt.Errorf("invalid result reference; run torana approvals list")
	}
	path := controlclient.BasePath + "/approvals/" + ref
	raw, _, err := c.client.JSON(c.ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if command == "approvals show" {
		return output(c.stdout, raw)
	}
	var item resultrelease.Record
	if json.Unmarshal(raw, &item) != nil || item.Reference != ref {
		return fmt.Errorf("Torana returned an invalid approval record")
	}
	if command == "approvals approve" {
		if _, err := fmt.Fprintln(c.stderr, "Allowing this exact tool result to reach the configured upstream model. This is an exception, not a clean scan verdict."); err != nil {
			return fmt.Errorf("print approval warning: %w", err)
		}
	}
	body, _ := json.Marshal(map[string]string{"expected_status": item.Status})
	raw, _, err = c.client.JSON(c.ctx, http.MethodPost, path+"/"+strings.TrimPrefix(command, "approvals "), body, "")
	if err != nil {
		return err
	}
	return output(c.stdout, raw)
}
