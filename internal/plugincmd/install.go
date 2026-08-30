package plugincmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
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

const (
	maxSourceFiles = 10_000
	maxSourceBytes = 100 << 20
)

// officialCatalog lists every plugin in the official repository and says, for
// each, whether `--official` installs it.
//
// It was a bare list of names, and two plugins were simply missing from it:
// cache_tier_selector and cache_warmer shipped, and `--official` quietly did
// not install them. Nothing was wrong in the list — the entries just were not
// there, which is the failure mode a list of names cannot show you.
//
// Naming every plugin and requiring a reason to exclude one makes an omission
// visible. A new plugin has to be added here to be installable, and leaving it
// out is a decision someone wrote down rather than an oversight.
type officialPlugin struct {
	name string
	// install is false for plugins that exist but must not be installed by
	// default; excludedBecause must then say why.
	install         bool
	excludedBecause string
}

var officialCatalog = []officialPlugin{
	{name: "cache_tier_selector", install: true},
	{name: "cache_warmer", install: true},
	{name: "compactor", install: true},
	{name: "intent", install: true},
	{name: "keyword_compactor", install: true},
	{name: "otel", install: true},
	{name: "pii", install: true},
	{name: "schema_translator", install: true},
	{name: "tool_governor", install: true},
	{
		name:    "auth",
		install: false,
		excludedBecause: "its own plugin.json says it is not published to the public registry and " +
			"is a reference for the capability surface only — installing it by default would put " +
			"something explicitly not built as an access control into an access-control position",
	},
}

// officialPlugins returns the names `--official` installs.
func officialPlugins() []string {
	names := make([]string, 0, len(officialCatalog))
	for _, p := range officialCatalog {
		if p.install {
			names = append(names, p.name)
		}
	}
	return names
}

// pluginsDir resolves where bundles are installed. Mirrors the server's
// plugins.dir default so a plugin installed by the CLI is the one the proxy
// discovers.
func pluginsDir() string {
	if v := os.Getenv("TORANA_PLUGINS_DIR"); v != "" {
		return v
	}
	return provider.DefaultPluginsDir
}

// source describes where a plugin is being installed from.
type source struct {
	local   string // filesystem path, when installing from disk
	repoURL string // git remote, when installing from a repository
	subPath string // directory within the repository
	ref     string // branch, tag, or commit; empty means the default branch
	name    string // plugin directory name
}

// parseSource accepts a local path, a browser directory URL, or a repository
// path of the form
//
//	github.com/owner/repo/path/to/plugin[@ref]
//
// Browser URLs use the ordinary GitHub /tree/ref/path or GitLab
// /-/tree/ref/path form. Any git host also works through the explicit
// https://host/group/repo.git//path/to/plugin@ref coordinate. Plugin
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
	spec = strings.TrimSuffix(spec, "/")

	// Canonical remote syntax makes the repository/subdirectory boundary
	// explicit, including for GitLab groups of arbitrary depth:
	//   https://host/group/repo.git//plugins/foo@ref
	if marker := strings.Index(spec, ".git//"); marker >= 0 {
		repoEnd := marker + len(".git")
		repoURL := spec[:repoEnd]
		subAndRef := spec[repoEnd+2:]
		if at := strings.LastIndex(subAndRef, "@"); at >= 0 {
			ref = subAndRef[at+1:]
			subAndRef = subAndRef[:at]
		}
		subPath := filepath.FromSlash(subAndRef)
		cleanSub := filepath.Clean(subPath)
		if repoURL == "" || subAndRef == "" || cleanSub == "." ||
			cleanSub != subPath || filepath.IsAbs(cleanSub) ||
			cleanSub == ".." || strings.HasPrefix(cleanSub, ".."+string(filepath.Separator)) {
			return source{}, fmt.Errorf("%q is not a valid repository plugin source", arg)
		}
		return source{
			repoURL: repoURL,
			subPath: filepath.ToSlash(cleanSub),
			ref:     ref,
			name:    filepath.Base(cleanSub),
		}, nil
	}

	if browser, recognized, err := parseBrowserSource(spec); recognized {
		return browser, err
	}

	if at := strings.LastIndex(spec, "@"); at > 0 {
		spec, ref = spec[:at], spec[at+1:]
	}
	parts := strings.Split(spec, "/")
	if len(parts) < 3 {
		return source{}, fmt.Errorf("%q is not a plugin source — expected a local path or host/owner/repo/path/to/plugin", arg)
	}
	repo := strings.Join(parts[:3], "/")
	sub := strings.Join(parts[3:], "/")
	subPath := filepath.FromSlash(sub)
	cleanSub := filepath.Clean(subPath)
	if sub == "" || cleanSub == "." || cleanSub != subPath ||
		filepath.IsAbs(cleanSub) || cleanSub == ".." ||
		strings.HasPrefix(cleanSub, ".."+string(filepath.Separator)) {
		return source{}, fmt.Errorf("%q names a repository but not a plugin directory inside it", arg)
	}
	return source{
		repoURL: "https://" + repo + ".git",
		subPath: filepath.ToSlash(cleanSub),
		ref:     ref,
		name:    filepath.Base(cleanSub),
	}, nil
}

// parseBrowserSource recognizes directory URLs copied from GitHub-compatible
// and GitLab repository browsers. Browser routes cannot unambiguously split a
// slash-bearing ref from the directory that follows it, so this form accepts a
// single path-segment ref. The explicit .git//subdirectory@ref coordinate
// remains available for refs containing slashes.
func parseBrowserSource(arg string) (source, bool, error) {
	u, err := url.Parse(arg)
	if err != nil || u.Scheme == "" {
		return source{}, false, nil
	}
	invalid := func() (source, bool, error) {
		return source{}, true, fmt.Errorf("%q is not a valid repository browser URL", arg)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalid()
	}
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return invalid()
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 {
		return invalid()
	}

	var repoParts, tail []string
	// GitLab and compatible hosts make the repository boundary explicit.
	for i := 2; i+1 < len(parts); i++ {
		if parts[i] == "-" && parts[i+1] == "tree" {
			repoParts = parts[:i]
			tail = parts[i+2:]
			break
		}
	}
	// GitHub and compatible hosts use /owner/repository/tree/ref/path.
	if repoParts == nil && len(parts) >= 5 && parts[2] == "tree" {
		repoParts = parts[:2]
		tail = parts[3:]
	}
	if len(repoParts) < 2 || len(tail) < 2 {
		return invalid()
	}
	allParts := append(append([]string(nil), repoParts...), tail...)
	for _, part := range allParts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "\\") {
			return invalid()
		}
	}

	repoPath := strings.Join(repoParts, "/")
	if !strings.HasSuffix(repoPath, ".git") {
		repoPath += ".git"
	}
	repoURL := (&url.URL{Scheme: "https", Host: u.Host, Path: "/" + repoPath}).String()
	return source{
		repoURL: repoURL,
		subPath: strings.Join(tail[1:], "/"),
		ref:     tail[0],
		name:    tail[len(tail)-1],
	}, true, nil
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

	args := []string{"clone", "--depth", "1", "--quiet", src.repoURL, tmp}
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
	if src.ref != "" {
		fetchCmd := exec.Command("git", "-C", tmp, "fetch", "--depth", "1", "origin", src.ref)
		fetchCmd.Stderr = stderr
		if err := fetchCmd.Run(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("fetch %s @ %s: %w", src.repoURL, src.ref, err)
		}
		checkoutCmd := exec.Command("git", "-C", tmp, "checkout", "--quiet", "--detach", "FETCH_HEAD")
		checkoutCmd.Stderr = stderr
		if err := checkoutCmd.Run(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("checkout %s @ %s: %w", src.repoURL, src.ref, err)
		}
	}

	dir := filepath.Join(tmp, filepath.FromSlash(src.subPath))
	if _, err := os.Stat(dir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s does not contain %s", src.repoURL, src.subPath)
	}
	return dir, cleanup, nil
}

// copyTree recursively copies a bounded plugin source tree. Nested Go/Rust
// packages and embedded assets are ordinary plugin inputs; symlinks and special
// files are rejected so a hostile repository cannot escape the source root.
// Toolchain output is never source and can exceed the entire staging budget, so
// top-level Go/Rust build directories are omitted from local installs.
func copyTree(src, dst string) error {
	files, bytesCopied := 0, int64(0)
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && filepath.Dir(rel) == "." && (entry.Name() == ".git" || entry.Name() == "target" || entry.Name() == ".torana-cargo-target") {
			return filepath.SkipDir
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("source path escapes root: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source contains unsupported symlink %s", rel)
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source contains unsupported special file %s", rel)
		}
		files++
		bytesCopied += info.Size()
		if files > maxSourceFiles || bytesCopied > maxSourceBytes {
			return fmt.Errorf("plugin source exceeds staging limit (%d files, %d bytes)", maxSourceFiles, maxSourceBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
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
		for _, name := range officialPlugins() {
			sources = append(sources, officialPluginsRepo+"/plugins/"+name)
		}
	}
	if len(sources) == 0 {
		return errors.New("nothing to install — pass a plugin source or --official")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create plugins dir: %w", err)
	}

	type cachedRepository struct {
		root    string
		cleanup func()
	}
	repositories := make(map[string]cachedRepository)
	defer func() {
		for _, cached := range repositories {
			cached.cleanup()
		}
	}()

	var installed []string
	for _, arg := range sources {
		src, err := parseSource(arg)
		if err != nil {
			return err
		}
		var srcDir string
		cleanup := func() {}
		if src.local != "" {
			srcDir, cleanup, err = fetch(src, stdout, stderr)
			if err != nil {
				return err
			}
		} else {
			cacheKey := src.repoURL + "\x00" + src.ref
			if cached, ok := repositories[cacheKey]; ok {
				srcDir = filepath.Join(cached.root, filepath.FromSlash(src.subPath))
			} else {
				var fetchedCleanup func()
				srcDir, fetchedCleanup, err = fetch(src, stdout, stderr)
				if err != nil {
					return err
				}
				root := srcDir
				for range strings.Split(filepath.ToSlash(src.subPath), "/") {
					root = filepath.Dir(root)
				}
				repositories[cacheKey] = cachedRepository{root: root, cleanup: fetchedCleanup}
			}
			if _, err := os.Stat(srcDir); err != nil {
				return fmt.Errorf("%s does not contain %s: %w", src.repoURL, src.subPath, err)
			}
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
		wasm := filepath.Join(stage, "plugin.wasm")
		language, err := detectPluginLanguage(stage)
		if err != nil {
			cleanup()
			_ = os.RemoveAll(stage)
			return fmt.Errorf("build %s: %w", src.name, err)
		}
		if err := validateSourceBuildPolicy(src, language); err != nil {
			cleanup()
			_ = os.RemoveAll(stage)
			return fmt.Errorf("build %s: %w", src.name, err)
		}
		language, err = buildPluginSource(stage, wasm, true, io.Discard, stderr)
		if err != nil {
			cleanup()
			_ = os.RemoveAll(stage)
			return fmt.Errorf("build %s: %w", src.name, err)
		}
		fmt.Fprintf(stdout, "Built %s plugin locally\n", language)
		srcDir = stage
		stageCleanup := func() { _ = os.RemoveAll(stage) }
		defer stageCleanup()

		target := filepath.Join(dest, src.name)
		installStage, err := os.MkdirTemp(dest, "."+src.name+".install-*")
		if err != nil {
			cleanup()
			return fmt.Errorf("stage install for %s: %w", src.name, err)
		}
		installStageCleanup := func() { _ = os.RemoveAll(installStage) }
		for _, name := range bundleFiles {
			data, err := os.ReadFile(filepath.Join(srcDir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				cleanup()
				installStageCleanup()
				return err
			}
			if err := os.WriteFile(filepath.Join(installStage, name), data, 0o644); err != nil {
				cleanup()
				installStageCleanup()
				return err
			}
		}

		if _, err := plugin.ValidateBundleDir(installStage); err != nil {
			cleanup()
			installStageCleanup()
			return fmt.Errorf("validate %s: %w", src.name, err)
		}
		digest, err := plugin.BundleDigestForDir(installStage)
		if err != nil {
			cleanup()
			installStageCleanup()
			return fmt.Errorf("digest %s: %w", src.name, err)
		}
		if err := activateBundle(installStage, target); err != nil {
			cleanup()
			installStageCleanup()
			return err
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

func validateSourceBuildPolicy(src source, language pluginLanguage) error {
	if language == pluginRust && src.local == "" {
		return errors.New("remote Rust plugins may contain native Cargo build scripts; clone and review the source, then install its local path")
	}
	if language == pluginRust {
		hasLock, err := regularFileExists(filepath.Join(src.local, "Cargo.lock"))
		if err != nil {
			return err
		}
		if !hasLock {
			return errors.New("local Rust plugin needs a reviewed Cargo.lock; run `cargo generate-lockfile`, review it, then install again")
		}
	}
	return nil
}

// localPluginBuildEnv prevents source being installed from selecting and
// downloading a different Go toolchain through its go.mod. Installation runs
// before a plugin digest can be reviewed, so that boundary must use the
// operator's already-installed toolchain. GOWORK is disabled as well: a
// process-level workspace must not silently replace the staged module graph.
func localPluginBuildEnv(extra ...string) []string {
	blocked := map[string]struct{}{"GOTOOLCHAIN": {}, "GOWORK": {}, "GOOS": {}, "GOARCH": {}}
	env := make([]string, 0, len(os.Environ())+2+len(extra))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, skip := blocked[name]; !skip {
			env = append(env, entry)
		}
	}
	env = append(env, "GOTOOLCHAIN=local", "GOWORK=off")
	return append(env, extra...)
}

func activateBundle(installStage, target string) error {
	dest := filepath.Dir(target)
	name := filepath.Base(target)
	backup, err := os.MkdirTemp(dest, "."+name+".previous-*")
	if err != nil {
		return fmt.Errorf("prepare rollback for %s: %w", target, err)
	}
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("prepare rollback for %s: %w", target, err)
	}
	hadTarget := false
	if _, err := os.Stat(target); err == nil {
		hadTarget = true
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("replace %s: %w", target, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(installStage, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("activate %s: %w", target, err)
	}
	if hadTarget {
		_ = os.RemoveAll(backup)
	}
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
