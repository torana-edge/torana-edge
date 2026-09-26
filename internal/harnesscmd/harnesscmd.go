// Package harnesscmd connects verified harnesses without changing their trust
// or permission policies. Configuration previews never expose other entries.
package harnesscmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/harness"
)

func Usage(w io.Writer) {
	fmt.Fprint(w, `Connect Torana MCP to your harness:
  torana harness list
  torana harness setup <claude-code|codex> [--scope project|user] [--dry-run] [--yes]
  torana harness teardown <claude-code|codex> [--scope project|user] [--dry-run] [--yes]
  --addr <loopback origin> pins the MCP connection to a specific Torana instance.
Project scope is the default. Preview shows only Torana's entry; writes make a
private recovery backup. Harness approval and workspace trust remain unchanged.
Start Torana and run 'torana mcp enable --yes' before using the connection.
`)
}

func configPath(name, scope, cwd, home, codexHome string) (string, error) {
	if scope != "project" && scope != "user" {
		return "", fmt.Errorf("scope must be project or user")
	}
	switch name {
	case "claude-code":
		if scope == "project" {
			return filepath.Join(cwd, ".mcp.json"), nil
		}
		if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
			return "", fmt.Errorf("custom CLAUDE_CONFIG_DIR: use Claude's MCP configuration command instead")
		}
		return filepath.Join(home, ".claude.json"), nil
	case "codex":
		if scope == "project" {
			return filepath.Join(cwd, ".codex", "config.toml"), nil
		}
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		return filepath.Join(codexHome, "config.toml"), nil
	default:
		return "", fmt.Errorf("no verified config writer for %q; add a stdio MCP server named torana with command torana and args [mcp, stdio] in your harness", name)
	}
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "harness" {
		args = args[1:]
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		Usage(stdout)
		return nil
	}
	if args[0] == "list" {
		if len(args) != 1 {
			return fmt.Errorf("usage: torana harness list")
		}
		_, err := fmt.Fprintln(stdout, "claude-code  project/user  stdio MCP\ncodex        project/user  stdio MCP")
		return err
	}
	if args[0] != "setup" && args[0] != "teardown" {
		return fmt.Errorf("unknown harness command %q", args[0])
	}
	if len(args) == 2 && args[1] == "--help" {
		Usage(stdout)
		return nil
	}
	if len(args) < 2 {
		return fmt.Errorf("choose a harness: claude-code or codex")
	}
	fs := flag.NewFlagSet("harness "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("scope", "project", "configuration scope")
	addr := fs.String("addr", "", "loopback MCP origin")
	dry := fs.Bool("dry-run", false, "preview without writing")
	yes := fs.Bool("yes", false, "apply the displayed change without prompting")
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected harness arguments")
	}
	serverArgs := []string{"mcp", "stdio"}
	if *addr != "" {
		if _, err := controlclient.New(*addr, time.Second); err != nil {
			return err
		}
		serverArgs = append(serverArgs, "--addr", *addr)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path, err := configPath(args[1], *scope, cwd, home, os.Getenv("CODEX_HOME"))
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	plan, err := harness.PlanFile(path, args[1], harness.Server{Command: binary, Args: serverArgs}, args[0] == "teardown")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s Torana MCP in %s\nCommand: %q\nArgs: %q\n", args[0], plan.Path, binary, serverArgs)
	if !plan.Changed {
		_, err := fmt.Fprintln(stdout, "No change needed.")
		return err
	}
	if *dry {
		_, err := fmt.Fprintln(stdout, "Preview only; no files changed.")
		return err
	}
	if !*yes {
		fmt.Fprint(stdout, "Apply this change? [y/N] ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if strings.ToLower(strings.TrimSpace(line)) != "y" && strings.ToLower(strings.TrimSpace(line)) != "yes" {
			_, err := fmt.Fprintln(stdout, "No files changed.")
			return err
		}
	}
	backup, err := plan.Apply()
	if backup != "" {
		fmt.Fprintf(stdout, "Recovery backup: %s\n", backup)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "Applied. Restart your harness to load its MCP configuration. Harness approvals still apply.")
	return err
}
