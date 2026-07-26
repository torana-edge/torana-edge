package plugincmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

// bundleFiles are the files that make up an installed plugin bundle. plugin.wasm
// is built locally; the rest are copied from source. agent.json is optional and
// participates in the digest only when present, which keeps three-file bundles
// digest-compatible with installs that predate agent contracts.
var bundleFiles = []string{"plugin.wasm", "plugin.json", "schema.json", "agent.json"}

// officialPluginsRepo is a convenience source, not a privileged one. Installing
// from it goes through exactly the same path as any other repository: fetch,
// build locally, digest what was built, hand it to the operator for approval.
const officialPluginsRepo = "github.com/torana-edge/torana-plugins"

var officialPlugins = []string{
	"compactor", "intent", "keyword_compactor", "otel", "pii", "schema_translator",
}

// pluginsDir resolves where bundles are installed. Mirrors the server's
// plugins.dir default so a plugin installed by the CLI is the one the proxy
// discovers.
func pluginsDir() string {
	if v := os.Getenv("TORANA_PLUGINS_DIR"); v != "" {
		return v
	}
	return "./plugins"
}

// source describes where a plugin is being installed from.
type source struct {
	local   string // filesystem path, when installing from disk
	repoURL string // git remote, when installing from a repository
	subPath string // directory within the repository
	ref     string // branch, tag, or commit; empty means the default branch
	name    string // plugin directory name
}

// parseSource accepts either a local path or a repository path of the form
//
//	github.com/owner/repo/path/to/plugin[@ref]
//
// Any git host works — the host is not special-cased, so a self-hosted GitLab
// or a private mirror is as installable as GitHub. That is the point: plugin
// distribution needs no central index and no publishing step.
func parseSource(arg string) (source, error) {
	if arg == "" {
		return source{}, errors.New("a plugin source is required")
	}
	if strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "~") {
		abs, err := filepath.Abs(os.ExpandEnv(strings.Replace(arg, "~", os.Getenv("HOME"), 1)))
		if err != nil {
			return source{}, fmt.Errorf("resolve %q: %w", arg, err)
		}
		return source{local: abs, name: filepath.Base(abs)}, nil
	}

	spec, ref := arg, ""
	if at := strings.LastIndex(arg, "@"); at > 0 {
		spec, ref = arg[:at], arg[at+1:]
	}
	spec = strings.TrimSuffix(spec, "/")
	parts := strings.Split(spec, "/")
	if len(parts) < 3 {
		return source{}, fmt.Errorf("%q is not a plugin source — expected a local path or host/owner/repo/path/to/plugin", arg)
	}
	repo := strings.Join(parts[:3], "/")
	sub := strings.Join(parts[3:], "/")
	if sub == "" {
		return source{}, fmt.Errorf("%q names a repository but not a plugin directory inside it", arg)
	}
	return source{
		repoURL: "https://" + repo + ".git",
		subPath: sub,
		ref:     ref,
		name:    filepath.Base(sub),
	}, nil
}

// fetch materializes the plugin's source directory and returns its path plus a
// cleanup function.
func fetch(src source, stdout, stderr io.Writer) (string, func(), error) {
	if src.local != "" {
		if _, err := os.Stat(src.local); err != nil {
			return "", nil, fmt.Errorf("plugin source %s: %w", src.local, err)
		}
		return src.local, func() {}, nil
	}

	tmp, err := os.MkdirTemp("", "torana-plugin-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	args := []string{"clone", "--depth", "1", "--quiet"}
	if src.ref != "" {
		args = append(args, "--branch", src.ref)
	}
	args = append(args, src.repoURL, tmp)
	fmt.Fprintf(stdout, "Fetching %s", src.repoURL)
	if src.ref != "" {
		fmt.Fprintf(stdout, " @ %s", src.ref)
	}
	_, _ = fmt.Fprintln(stdout)

	cmd := exec.Command("git", args...)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("clone %s: %w", src.repoURL, err)
	}

	dir := filepath.Join(tmp, filepath.FromSlash(src.subPath))
	if _, err := os.Stat(dir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s does not contain %s", src.repoURL, src.subPath)
	}
	return dir, cleanup, nil
}

// copyTree copies a plugin source directory one level deep. Plugin bundles are
// flat by construction, and refusing to recurse keeps a hostile repository from
// staging something enormous or escaping via a symlinked subdirectory.
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func installPlugin(args []string, stdout, stderr io.Writer) error {
	var sources []string
	official := false
	dest := pluginsDir()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--official":
			official = true
		case "--dir":
			if i+1 >= len(args) {
				return errors.New("--dir requires a path")
			}
			dest = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q", args[i])
			}
			sources = append(sources, args[i])
		}
	}
	if official {
		for _, name := range officialPlugins {
			sources = append(sources, officialPluginsRepo+"/plugins/"+name)
		}
	}
	if len(sources) == 0 {
		return errors.New("nothing to install — pass a plugin source or --official")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create plugins dir: %w", err)
	}

	var installed []string
	for _, arg := range sources {
		src, err := parseSource(arg)
		if err != nil {
			return err
		}
		srcDir, cleanup, err := fetch(src, stdout, stderr)
		if err != nil {
			return err
		}

		if _, err := os.Stat(filepath.Join(srcDir, "plugin.json")); err != nil {
			cleanup()
			return fmt.Errorf("%s has no plugin.json — is it a Torana plugin?", arg)
		}

		// Stage into a scratch directory and build there. Never build in the
		// source tree: for a local source that would drop plugin.wasm and a
		// rewritten go.sum into the user's own working copy.
		stage, err := os.MkdirTemp("", "torana-build-*")
		if err != nil {
			cleanup()
			return fmt.Errorf("create build dir: %w", err)
		}
		if err := copyTree(srcDir, stage); err != nil {
			cleanup()
			_ = os.RemoveAll(stage)
			return fmt.Errorf("stage %s: %w", src.name, err)
		}

		fmt.Fprintf(stdout, "Building %s from source\n", src.name)
		// Plugin modules ship go.mod without go.sum, so a fresh checkout has
		// nothing to verify against. Resolve first, then build.
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = stage
		tidy.Stderr = stderr
		if err := tidy.Run(); err != nil {
			cleanup()
			_ = os.RemoveAll(stage)
			return fmt.Errorf("resolve dependencies for %s: %w", src.name, err)
		}
		wasm := filepath.Join(stage, "plugin.wasm")
		build := exec.Command("go", "build", "-buildmode=c-shared", "-buildvcs=false", "-o", wasm, ".")
		build.Dir = stage
		build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		build.Stderr = stderr
		if err := build.Run(); err != nil {
			cleanup()
			_ = os.RemoveAll(stage)
			return fmt.Errorf("build %s: %w (plugins are compiled locally, never downloaded prebuilt, so a Go toolchain is required)", src.name, err)
		}
		srcDir = stage
		stageCleanup := func() { _ = os.RemoveAll(stage) }
		defer stageCleanup()

		target := filepath.Join(dest, src.name)
		if err := os.MkdirAll(target, 0o755); err != nil {
			cleanup()
			return fmt.Errorf("create %s: %w", target, err)
		}
		for _, name := range bundleFiles {
			data, err := os.ReadFile(filepath.Join(srcDir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				cleanup()
				return err
			}
			if err := os.WriteFile(filepath.Join(target, name), data, 0o644); err != nil {
				cleanup()
				return err
			}
		}

		digest, err := plugin.BundleDigestForDir(target)
		if err != nil {
			cleanup()
			return fmt.Errorf("digest %s: %w", src.name, err)
		}
		cleanup()
		fmt.Fprintf(stdout, "Installed %s -> %s\n  digest %s\n", src.name, target, digest)
		installed = append(installed, src.name)
	}

	fmt.Fprintf(stdout, "\n%d plugin(s) installed. They are NOT running yet.\n", len(installed))
	_, _ = fmt.Fprintln(stdout, "Torana never loads a plugin you have not approved. Open the control plane")
	_, _ = fmt.Fprintln(stdout, "at http://127.0.0.1:8080/_torana/, review what each one requests, and approve")
	_, _ = fmt.Fprintln(stdout, "its digest. Approval is bound to that digest — rebuild it and you approve again.")
	return nil
}

func listPlugins(args []string, stdout io.Writer) error {
	dest := pluginsDir()
	if len(args) >= 2 && args[0] == "--dir" {
		dest = args[1]
	}
	entries, err := os.ReadDir(dest)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "No plugins directory at %s.\n", dest)
		_, _ = fmt.Fprintln(stdout, "Install the official set with: torana plugin install --official")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dest, err)
	}

	type row struct{ name, version, digest, built string }
	var rows []row
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(dest, e.Name())
		manifest, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
		if err != nil {
			continue
		}
		var m struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		_ = json.Unmarshal(manifest, &m)
		if m.Name == "" {
			m.Name = e.Name()
		}
		digest, _ := plugin.BundleDigestForDir(dir)
		built := "yes"
		if _, err := os.Stat(filepath.Join(dir, "plugin.wasm")); err != nil {
			built = "NOT BUILT"
			digest = "-"
		}
		rows = append(rows, row{m.Name, m.Version, digest, built})
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "No plugins installed in %s.\n", dest)
		_, _ = fmt.Fprintln(stdout, "Install the official set with: torana plugin install --official")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	fmt.Fprintf(stdout, "%-22s %-10s %-8s %s\n", "PLUGIN", "VERSION", "BUILT", "DIGEST")
	for _, r := range rows {
		fmt.Fprintf(stdout, "%-22s %-10s %-8s %s\n", r.name, r.version, r.built, r.digest)
	}
	fmt.Fprintf(stdout, "\nEnabling and approving happens in the control plane, not here.\n")
	return nil
}

func removePlugin(args []string, stdout io.Writer) error {
	dest := pluginsDir()
	var names []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" {
			if i+1 >= len(args) {
				return errors.New("--dir requires a path")
			}
			dest = args[i+1]
			i++
			continue
		}
		names = append(names, args[i])
	}
	if len(names) == 0 {
		return errors.New("a plugin name is required")
	}
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			return fmt.Errorf("invalid plugin name %q", name)
		}
		dir := filepath.Join(dest, name)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not installed in %s", name, dest)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
		fmt.Fprintf(stdout, "Removed %s\n", name)
	}
	_, _ = fmt.Fprintln(stdout, "\nRemove it from plugins.order in the control plane too, or the next")
	_, _ = fmt.Fprintln(stdout, "reload will report it as enabled but missing.")
	return nil
}
