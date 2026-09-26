package harnesscmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/harness"
)

func runHooks(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		Usage(stdout)
		return nil
	}
	if len(args) < 2 || args[1] != "claude-code" || (args[0] != "setup" && args[0] != "teardown") {
		return fmt.Errorf("usage: torana harness hooks <setup|teardown> claude-code [--scope user|project] [--addr origin] [--pre-model-switch] [--dry-run] [--yes]")
	}
	fs := flag.NewFlagSet("harness hooks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("scope", "user", "settings scope")
	addr := fs.String("addr", "", "loopback Torana origin")
	pre := fs.Bool("pre-model-switch", false, "also install optional warning (a timeout blocks model switching)")
	dry := fs.Bool("dry-run", false, "preview without writing")
	yes := fs.Bool("yes", false, "apply without prompting")
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || (*scope != "user" && *scope != "project") {
		return fmt.Errorf("choose user or project scope without extra arguments")
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if *scope == "project" {
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	} else if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return fmt.Errorf("custom CLAUDE_CONFIG_DIR: configure hooks in Claude settings manually")
	}
	client, err := controlclient.New(*addr, time.Second)
	if err != nil {
		return err
	}
	origin := client.Address()
	client.Close()
	settings := "settings.json"
	if *scope == "project" {
		settings = "settings.local.json"
	}
	plan, err := harness.PlanClaudeHooks(filepath.Join(dir, ".claude", settings), origin, *pre, args[0] == "teardown")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s Torana HTTP hooks in %s\nOrigin: %s\nStop/PostModelSwitch timeout: 3s\n", args[0], plan.Path, origin); err != nil {
		return err
	}
	if *pre && args[0] == "setup" {
		if _, err := fmt.Fprintln(stdout, "Optional PreModelSwitch timeout: 1s. If Torana is unreachable or times out, Claude can block the switch."); err != nil {
			return err
		}
	}
	if !plan.Changed || *dry {
		_, err := fmt.Fprintln(stdout, "No files changed.")
		return err
	}
	if !*yes {
		if _, err := fmt.Fprint(stdout, "Apply this change? [y/N] "); err != nil {
			return err
		}
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			_, err := fmt.Fprintln(stdout, "No files changed.")
			return err
		}
	}
	backup, err := plan.Apply()
	if backup != "" {
		fmt.Fprintf(stdout, "Private recovery backup: %s\n", backup)
	}
	if err != nil {
		return err
	}
	if args[0] == "setup" {
		if *scope == "project" && projectHooksNeedIgnoreWarning(dir) {
			fmt.Fprintln(stderr, "Warning: .claude/settings.local.json is not ignored by Git. Add it to .gitignore; it contains your per-user Torana hooks.")
		}
		_, err = fmt.Fprintln(stdout, "Hooks installed. Enable suggestions.claude_code.enabled in Torana and export TORANA_MCP_TOKEN before starting Claude. Optional PreModelSwitch also needs suggestions.claude_code.pre_model_switch.")
	} else {
		_, err = fmt.Fprintln(stdout, "Owned hooks removed. Restart Claude to reload settings.")
	}
	return err
}

// Git is optional. Only warn when it positively identifies a worktree and
// reports that the local settings file is not ignored; never change ignore rules.
func projectHooksNeedIgnoreWarning(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return false
	}
	cmd = exec.CommandContext(ctx, "git", "check-ignore", "-q", "--", ".claude/settings.local.json")
	cmd.Dir = dir
	err = cmd.Run()
	var exit *exec.ExitError
	return ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1
}
