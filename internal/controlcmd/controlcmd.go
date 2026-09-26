// Package controlcmd implements live administration through the same API as
// the Web UI. Success output is JSON; diagnostics go to stderr; writes are
// explicit and never retried automatically.
package controlcmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func Handles(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "config", "pipeline", "stats", "feed", "agent", "suggestions", "conversations":
		return true
	case "plugin":
		return len(args) > 1 && slices.Contains([]string{"status", "inspect", "approve", "revoke", "enable", "disable", "config"}, args[1])
	}
	return false
}

func Usage(w io.Writer) {
	fmt.Fprint(w, `Live administration (running Torana required; JSON output):
  torana config get                         export a revisioned settings snapshot
  torana config apply --file settings.json --yes
  torana pipeline get                       export order, config, and approvals
  torana pipeline apply --file pipeline.json --yes
  torana pipeline order <name>... --yes     replace the enabled order
  torana pipeline order --empty --yes       disable the entire pipeline
  torana plugin status                      list installed AND loaded state
  torana plugin inspect <name>              inspect digest, permissions, resources
  torana plugin approve <name> --file approval.json --yes
  torana plugin revoke <name> --yes          revoke approval and disable
  torana plugin enable <name> --yes          enable an already-approved bundle
  torana plugin disable <name> --yes         retain its config and approval
  torana plugin config get <name>            export revisioned plugin settings
  torana plugin config apply <name> --file config.json --yes
  torana stats                              aggregate request statistics
  torana feed                               recent request snapshot
  torana feed --follow                      newline-delimited JSON, Ctrl-C to stop
  torana conversations                      recent conversation IDs and activity
  torana agent discover                     operations and schemas, including plugins
  torana agent call <operation-id> [--file input.json] [--yes]
  torana suggestions list --conversation <id>
  torana suggestions show <id> --conversation <id>
  torana suggestions accept <id> --conversation <id> --yes
  torana suggestions dismiss <id> --conversation <id> --yes

All commands accept --addr host:port (or a loopback HTTP(S) origin).
--file - reads stdin. --json is accepted; JSON is already the default.
Writes require --yes; an agent should obtain operator consent for approvals.
config/pipeline/plugin-config apply require the revision from their get command.
Edit the snapshot's config or pipeline, not its revision. Stale edits fail safely.
Settings apply does not change plugins; use pipeline or plugin commands for those.
Provider bridge settings are included in config get/apply. Set bridge to null to
remove a bridge; omitting it preserves the existing bridge configuration.
An approval file is a PluginApproval object with the reviewed digest, permissions,
failure_mode, and any resource bindings. Approval does not enable a disabled plugin.
plugin list/install/remove work on disk; plugin status inspects the running host.
`)
}

type options struct {
	addr, file, conversation string
	yes, follow, empty       bool
	args                     []string
}

// Accept options before or after positional arguments, but reject unknown,
// duplicate, and irrelevant flags rather than silently ignoring a typo.
func parseOptions(args []string, allowed string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("torana", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { Usage(stderr) }
	fs.StringVar(&o.addr, "addr", "", "loopback control-plane origin")
	fs.Bool("json", false, "JSON output (default)")
	if strings.Contains(allowed, "file") {
		fs.StringVar(&o.file, "file", "", "JSON file, or - for stdin")
	}
	if strings.Contains(allowed, "yes") {
		fs.BoolVar(&o.yes, "yes", false, "confirm the requested mutation")
	}
	if strings.Contains(allowed, "follow") {
		fs.BoolVar(&o.follow, "follow", false, "follow request events")
	}
	if strings.Contains(allowed, "empty") {
		fs.BoolVar(&o.empty, "empty", false, "explicitly disable every plugin")
	}
	if strings.Contains(allowed, "conversation") {
		fs.StringVar(&o.conversation, "conversation", "", "conversation ID from torana conversations")
	}
	var flags, positional []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "h" || name == "help" {
			Usage(stderr)
			return o, flag.ErrHelp
		}
		f := fs.Lookup(name)
		if f == nil {
			return o, fmt.Errorf("unknown option %q for this command", arg)
		}
		if seen[name] {
			return o, fmt.Errorf("option --%s was specified more than once", name)
		}
		seen[name] = true
		flags = append(flags, arg)
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); (!ok || !bf.IsBoolFlag()) && !hasValue {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
				return o, fmt.Errorf("--%s requires a value", name)
			}
			i++
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return o, err
	}
	o.args = positional
	return o, nil
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command required")
	}
	command, rest := args[0], args[1:]
	if command == "config" || command == "pipeline" || command == "agent" || command == "plugin" || command == "suggestions" {
		if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
			Usage(stdout)
			return nil
		}
		command += " " + rest[0]
		rest = rest[1:]
		if command == "plugin config" {
			if len(rest) == 0 || rest[0] == "--help" {
				Usage(stdout)
				return nil
			}
			command += " " + rest[0]
			rest = rest[1:]
		}
	}
	allowed := ""
	switch command {
	case "config get", "pipeline get", "plugin status", "plugin inspect", "plugin config get", "stats", "agent discover", "conversations":
	case "config apply", "pipeline apply", "plugin config apply", "plugin approve", "agent call":
		allowed = "file yes"
	case "plugin enable", "plugin disable", "plugin revoke":
		allowed = "yes"
	case "pipeline order":
		allowed = "yes empty"
	case "feed":
		allowed = "follow"
	case "suggestions list", "suggestions show":
		allowed = "conversation"
	case "suggestions accept", "suggestions dismiss":
		allowed = "conversation yes"
	default:
		return fmt.Errorf("unknown live command %q; run torana help", command)
	}
	o, err := parseOptions(rest, allowed, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	wantArgs := 0
	if strings.HasPrefix(command, "plugin ") && command != "plugin status" || command == "agent call" || command == "suggestions show" || command == "suggestions accept" || command == "suggestions dismiss" {
		wantArgs = 1
	}
	if command != "pipeline order" && len(o.args) != wantArgs {
		return fmt.Errorf("%s expects %d positional argument(s)", command, wantArgs)
	}
	if strings.Contains(allowed, "yes") && command != "agent call" && !o.yes {
		return fmt.Errorf("%s changes the running proxy; review the input and pass --yes", command)
	}
	if strings.HasPrefix(command, "suggestions ") && o.conversation == "" {
		return fmt.Errorf("%s requires --conversation <id>; run torana conversations to find it", command)
	}
	if command == "pipeline order" && ((len(o.args) == 0 && !o.empty) || (len(o.args) > 0 && o.empty)) {
		return fmt.Errorf("supply plugin names, or --empty to disable every plugin")
	}
	if strings.HasPrefix(command, "plugin ") && command != "plugin status" && !validName(o.args[0]) {
		return fmt.Errorf("invalid plugin name")
	}
	timeout := 60 * time.Second
	if o.follow {
		timeout = 0
	}
	client, err := controlclient.New(o.addr, timeout)
	if err != nil {
		return err
	}
	defer client.Close()
	c := runner{ctx: ctx, client: client, stdin: stdin, stdout: stdout, stderr: stderr}
	switch command {
	case "config get", "pipeline get", "plugin config get":
		return c.getSnapshot(command, o)
	case "config apply", "pipeline apply", "plugin config apply":
		return c.applySnapshot(command, o)
	case "plugin status":
		return c.read("/plugins")
	case "plugin inspect":
		info, err := c.inspect(o.args[0])
		if err != nil {
			return err
		}
		return output(stdout, info)
	case "plugin approve", "plugin revoke", "plugin enable", "plugin disable", "pipeline order":
		return c.changePlugin(command, o)
	case "stats":
		return c.read("/stats")
	case "conversations":
		return c.read("/conversations")
	case "feed":
		if o.follow {
			return c.follow()
		}
		return c.read("/feed")
	case "agent discover":
		return c.read("/")
	case "agent call":
		return c.call(o)
	case "suggestions list", "suggestions show", "suggestions accept", "suggestions dismiss":
		return c.suggestions(command, o)
	}
	return fmt.Errorf("unhandled command %q", command)
}

type runner struct {
	ctx            context.Context
	client         *controlclient.Client
	stdin          io.Reader
	stdout, stderr io.Writer
}

func output(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func (c *runner) read(path string) error {
	raw, _, err := c.client.JSON(c.ctx, http.MethodGet, controlclient.BasePath+path, nil, "")
	if err != nil {
		return err
	}
	return output(c.stdout, raw)
}

func (c *runner) suggestions(command string, o options) error {
	listPath := "/agent/suggestions?conversation_id=" + url.QueryEscape(o.conversation)
	if command == "suggestions list" || command == "suggestions show" {
		raw, _, err := c.client.JSON(c.ctx, http.MethodGet, controlclient.BasePath+listPath, nil, "")
		if err != nil {
			return err
		}
		if command == "suggestions list" {
			return output(c.stdout, raw)
		}
		var list struct {
			Suggestions []json.RawMessage `json:"suggestions"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, item := range list.Suggestions {
			var identity struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(item, &identity) == nil && identity.ID == o.args[0] {
				return output(c.stdout, item)
			}
		}
		return fmt.Errorf("suggestion %q was not found in conversation %q", o.args[0], o.conversation)
	}
	action := "accept"
	if command == "suggestions dismiss" {
		action = "dismiss"
	}
	body, _ := json.Marshal(map[string]string{"conversation_id": o.conversation})
	path := controlclient.BasePath + "/agent/suggestions/" + url.PathEscape(o.args[0]) + "/" + action
	raw, _, err := c.client.JSON(c.ctx, http.MethodPost, path, body, "")
	if err != nil {
		return err
	}
	return output(c.stdout, raw)
}

type snapshot struct {
	Revision string          `json:"revision"`
	Config   json.RawMessage `json:"config,omitempty"`
	Pipeline json.RawMessage `json:"pipeline,omitempty"`
}

func (c *runner) config() (provider.Config, string, error) {
	raw, revision, err := c.client.JSON(c.ctx, http.MethodGet, controlclient.BasePath+"/config", nil, "")
	if err != nil {
		return provider.Config{}, "", err
	}
	if revision == "" {
		return provider.Config{}, "", fmt.Errorf("server does not expose configuration revisions; update Torana before using live mutation commands")
	}
	var cfg provider.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, "", err
	}
	return cfg, revision, nil
}

type pipelineInput struct {
	Order     []string                           `json:"order"`
	HookOrder map[string][]string                `json:"hook_order"`
	Config    map[string]json.RawMessage         `json:"config"`
	Approvals map[string]provider.PluginApproval `json:"approvals"`
}

func (c *runner) getSnapshot(command string, o options) error {
	cfg, revision, err := c.config()
	if err != nil {
		return err
	}
	s := snapshot{Revision: revision}
	switch command {
	case "config get":
		// Plugins have their own atomic update endpoint. Excluding them makes
		// it impossible to edit a field this command would silently ignore.
		raw, _ := json.Marshal(cfg)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		delete(fields, "plugins")
		s.Config, _ = json.Marshal(fields)
	case "pipeline get":
		p := cfg.Plugins
		if p.Order == nil {
			p.Order = []string{}
		}
		if p.HookOrder == nil {
			p.HookOrder = map[string][]string{}
		}
		if p.Config == nil {
			p.Config = map[string]json.RawMessage{}
		}
		if p.Approvals == nil {
			p.Approvals = map[string]provider.PluginApproval{}
		}
		s.Pipeline, _ = json.Marshal(pipelineInput{p.Order, p.HookOrder, p.Config, p.Approvals})
	case "plugin config get":
		if _, err := c.inspect(o.args[0]); err != nil {
			return err
		}
		s.Config = cfg.Plugins.Config[o.args[0]]
		if len(s.Config) == 0 {
			s.Config = json.RawMessage(`{}`)
		}
	}
	return output(c.stdout, s)
}

func (c *runner) file(path string, out any) error {
	if path == "" {
		return fmt.Errorf("--file is required (use --file - for stdin)")
	}
	r := c.stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	raw, err := controlclient.ReadBounded(r)
	if err != nil {
		return err
	}
	return strictJSON(raw, out)
}

func strictJSON(raw []byte, out any) error {
	_, rawValue := out.(*json.RawMessage)
	if !json.Valid(raw) || (!rawValue && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))) {
		return fmt.Errorf("expected one non-null JSON value")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON input: %w", err)
	}
	return nil
}

func (c *runner) applySnapshot(command string, o options) error {
	var s snapshot
	if err := c.file(o.file, &s); err != nil {
		return err
	}
	if !validRevision(s.Revision) {
		return fmt.Errorf("snapshot requires the unchanged revision from the matching get command")
	}
	path, method, body := "/config", http.MethodPut, s.Config
	if command == "pipeline apply" {
		if s.Config != nil {
			return fmt.Errorf("pipeline snapshot must not contain config at the top level")
		}
		var p pipelineInput
		if err := strictJSON(s.Pipeline, &p); err != nil {
			return err
		}
		if p.Order == nil || p.HookOrder == nil || p.Config == nil || p.Approvals == nil {
			return fmt.Errorf("pipeline snapshot requires non-null order, hook_order, config, and approvals")
		}
		path, body = "/plugins", s.Pipeline
	} else {
		if s.Pipeline != nil {
			return fmt.Errorf("config snapshot must not contain pipeline")
		}
		var obj map[string]json.RawMessage
		if err := strictJSON(body, &obj); err != nil {
			return err
		}
		if command == "config apply" {
			if _, exists := obj["plugins"]; exists {
				return fmt.Errorf("use torana pipeline for plugin settings")
			}
			var cfg provider.Config
			if err := strictJSON(body, &cfg); err != nil {
				return err
			}
		} else {
			path, method = "/plugins/"+o.args[0]+"/config", http.MethodPost
		}
	}
	return c.write(method, path, body, s.Revision)
}

func validRevision(revision string) bool {
	if len(revision) < 3 || revision[0] != '"' || revision[len(revision)-1] != '"' {
		return false
	}
	for _, r := range revision[1 : len(revision)-1] {
		if r < 33 || r > 126 || r == '"' {
			return false
		}
	}
	return true
}

func (c *runner) write(method, path string, body []byte, revision string) error {
	raw, _, err := c.client.JSON(c.ctx, method, controlclient.BasePath+path, body, revision)
	if err != nil {
		return err
	}
	if err := output(c.stdout, raw); err != nil {
		return err
	}
	var result struct {
		Warnings []struct{ Name, Reason, Remedy string } `json:"warnings"`
	}
	_ = json.Unmarshal(raw, &result)
	for _, warning := range result.Warnings {
		fmt.Fprintf(c.stderr, "Warning: %s is not running: %s. %s\n", warning.Name, warning.Reason, warning.Remedy)
	}
	if len(result.Warnings) > 0 {
		return fmt.Errorf("configuration saved, but %d plugin(s) are not running; inspect torana plugin status before retrying", len(result.Warnings))
	}
	return nil
}

type pluginInfo struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Digest      string                   `json:"digest"`
	State       string                   `json:"state"`
	Permissions []string                 `json:"permissions"`
	Approval    *provider.PluginApproval `json:"approval"`
}

func (c *runner) inspect(name string) (json.RawMessage, error) {
	raw, _, err := c.client.JSON(c.ctx, http.MethodGet, controlclient.BasePath+"/plugins", nil, "")
	if err != nil {
		return nil, err
	}
	var list struct {
		Plugins []json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	for _, item := range list.Plugins {
		var info pluginInfo
		if err := json.Unmarshal(item, &info); err != nil {
			return nil, err
		}
		if info.Name == name {
			return item, nil
		}
	}
	return nil, fmt.Errorf("plugin %q was not found by the running host; check its plugin directory with torana plugin status", name)
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func (c *runner) changePlugin(command string, o options) error {
	cfg, revision, err := c.config()
	if err != nil {
		return err
	}
	patch := map[string]any{}
	if command == "pipeline order" {
		order := append([]string{}, o.args...)
		seen := map[string]bool{}
		for _, name := range order {
			if !validName(name) || seen[name] {
				return fmt.Errorf("invalid or duplicate plugin %q", name)
			}
			seen[name] = true
		}
		patch["order"] = order
	} else {
		name := o.args[0]
		raw, err := c.inspect(name)
		if err != nil {
			return err
		}
		var info pluginInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			return err
		}
		key := info.ID
		if key == "" {
			key = name
		}
		switch command {
		case "plugin approve":
			var approval provider.PluginApproval
			if err := c.file(o.file, &approval); err != nil {
				return err
			}
			if info.Digest == "" || approval.Digest != info.Digest {
				return fmt.Errorf("approval digest does not match the installed bundle; inspect and review the current digest")
			}
			if approval.FailureMode != "pass" && approval.FailureMode != "block" {
				return fmt.Errorf("approval must explicitly select failure_mode: pass or block")
			}
			permissions := append([]string{}, approval.Permissions...)
			requested := append([]string{}, info.Permissions...)
			slices.Sort(permissions)
			slices.Sort(requested)
			if !slices.Equal(permissions, requested) {
				return fmt.Errorf("approval permissions must match the reviewed requested permissions exactly")
			}
			if cfg.Plugins.Approvals == nil {
				cfg.Plugins.Approvals = map[string]provider.PluginApproval{}
			}
			delete(cfg.Plugins.Approvals, name)
			cfg.Plugins.Approvals[key] = approval
			patch["approvals"] = cfg.Plugins.Approvals
		case "plugin enable":
			approval, ok := cfg.Plugins.Approvals[key]
			if !ok {
				approval, ok = cfg.Plugins.Approvals[name]
			}
			if !ok || info.Digest == "" || approval.Digest != info.Digest {
				return fmt.Errorf("plugin %q needs explicit approval of its current digest before enabling", name)
			}
			order := append([]string{}, cfg.Plugins.Order...)
			if !slices.Contains(order, name) {
				order = append(order, name)
			}
			patch["order"] = order
		case "plugin disable", "plugin revoke":
			order := make([]string, 0, len(cfg.Plugins.Order))
			for _, n := range cfg.Plugins.Order {
				if n != name {
					order = append(order, n)
				}
			}
			patch["order"] = order
			if command == "plugin revoke" {
				if cfg.Plugins.Approvals == nil {
					cfg.Plugins.Approvals = map[string]provider.PluginApproval{}
				}
				delete(cfg.Plugins.Approvals, key)
				delete(cfg.Plugins.Approvals, name)
				patch["approvals"] = cfg.Plugins.Approvals
			}
		}
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.write(http.MethodPut, "/plugins", body, revision)
}

func (c *runner) follow() error {
	resp, err := c.client.Open(c.ctx, http.MethodGet, controlclient.BasePath+"/stream", nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("control plane did not return an event stream")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), controlclient.MaxBodyBytes)
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				if !json.Valid(data) {
					return fmt.Errorf("invalid JSON request event")
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, data); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(c.stdout, compact.String()); err != nil {
					return err
				}
				data = nil
			}
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(value, " ")...)
			if len(data) > controlclient.MaxBodyBytes {
				return fmt.Errorf("request event too large")
			}
		}
	}
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("request event stream ended; reconnect explicitly (snapshot events may repeat)")
}

func (c *runner) call(o options) error {
	raw, _, err := c.client.JSON(c.ctx, http.MethodGet, controlclient.BasePath+"/", nil, "")
	if err != nil {
		return err
	}
	var doc struct {
		Operations []struct {
			ID, Method, Path, Risk string
			Plugin                 string          `json:"plugin"`
			PluginDigest           string          `json:"plugin_digest"`
			Input                  json.RawMessage `json:"input_schema"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	for _, op := range doc.Operations {
		if op.ID != o.args[0] {
			continue
		}
		if strings.ContainsAny(op.Path, "{}") {
			return fmt.Errorf("operation requires a named resource; use its dedicated CLI command")
		}
		// Built-in mutations use revisioned snapshots; a generic invocation
		// must not become an escape hatch around their lost-update checks.
		if op.Method != http.MethodGet && op.Plugin == "" {
			return fmt.Errorf("use the dedicated torana command for built-in mutations")
		}
		if !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, op.Method) {
			return fmt.Errorf("unsupported operation method")
		}
		if op.Method != http.MethodGet || op.Risk != "read" {
			if !o.yes {
				return fmt.Errorf("operation %s has risk %q; review torana agent discover and pass --yes", op.ID, op.Risk)
			}
		}
		var body json.RawMessage
		if o.file != "" {
			if len(op.Input) == 0 {
				return fmt.Errorf("operation accepts no input body")
			}
			if err := c.file(o.file, &body); err != nil {
				return err
			}
		} else if len(op.Input) > 0 {
			return fmt.Errorf("operation requires --file input.json (or - for stdin)")
		}
		var result json.RawMessage
		if op.Plugin != "" {
			result, _, err = c.client.CallPlugin(c.ctx, op.Method, op.Path, body, op.PluginDigest)
		} else {
			result, _, err = c.client.JSON(c.ctx, op.Method, op.Path, body, "")
		}
		if err != nil {
			return err
		}
		return output(c.stdout, result)
	}
	return fmt.Errorf("operation %q is not advertised by the running host", o.args[0])
}
