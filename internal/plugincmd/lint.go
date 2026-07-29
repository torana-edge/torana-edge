package plugincmd

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/torana-edge/torana-edge/internal/plugin"
	sdk "github.com/torana-edge/torana-plugin-sdk"
)

// sdkModulePath is the import path a plugin uses to reach the SDK. The local
// name varies — most plugins alias it to `sdk` — so the linter resolves the
// alias from the import block rather than assuming one.
const sdkModulePath = "github.com/torana-edge/torana-plugin-sdk"

// sdkPermission maps an SDK helper to the capability it reaches for. This is
// the table that lets the linter answer "does this plugin ask for something it
// never declared?", which nothing checks today in either direction.
//
// sdk.HostCall is handled separately: its capability comes from the command
// string, which is only knowable when that argument is a literal.
var sdkPermission = map[string]string{
	"Log":              "env.log",
	"EmitMetric":       "env.emit_metric",
	"PluginConfig":     "env.plugin_config",
	"StateGet":         "env.state_get",
	"StateGetJSON":     "env.state_get",
	"StateSet":         "env.state_set",
	"StateSetJSON":     "env.state_set",
	"StateDelete":      "env.state_set",
	"StateKeys":        "env.state_keys",
	"Now":              "env.now",
	"OriginalRequest":  "env.original_request",
	"OriginalResponse": "env.original_response",
	"GetCachePricing":  "env.host_call.torana_cache_pricing",
	"SendRequest":      "env.host_call.torana_send_request",
	"BlockRequest":     "env.block_request",
	"RespondRequest":   "env.respond_request",
	"RouteRequest":     "env.route_request",
}

// sdkHook maps a registration call to the hook it implements.
var sdkHook = map[string]string{
	"OnBeforeRequest": "run_before_request",
	"OnAfterResponse": "run_after_response",
	"OnStreamChunk":   "run_on_stream_chunk",
	"OnHTTPRequest":   "run_on_http_request",
	"OnTick":          "run_on_tick",
}

// hookGatePermission lists capabilities that are not host calls at all: the
// host checks them before dispatching a hook (discovery.go:1130, :1198), so
// declaring the hook is what uses them. Nothing appears in the plugin's source.
var hookGatePermission = map[string]string{
	"env.serve_http":      "run_on_http_request",
	"env.background_tick": "run_on_tick",
}

// unattributable lists capabilities static analysis cannot see. The host
// injects _request_headers into ToranaMeta when the grant is held
// (server.go:770); the plugin reads it out of a JSON blob, so there is no SDK
// call to attribute. Never report these as unused — a linter that cries wolf
// gets ignored, and then it is worse than no linter.
var unattributable = map[string]bool{
	"env.request_headers": true,
}

type severity int

const (
	sevError severity = iota
	sevWarning
)

type finding struct {
	sev severity
	msg string
}

// usage records what the source actually does, gathered by walking the AST.
type usage struct {
	permissions map[string]token.Position // capability → first reference
	hooks       map[string]token.Position // hook → registration site
	// registeredInMain records hooks registered inside func main(), which
	// never runs under -buildmode=c-shared. A plugin written this way loads
	// healthy and does nothing, forever, with no error anywhere.
	registeredInMain map[string]token.Position
	// dynamicHostCall notes a sdk.HostCall whose command is not a literal, so
	// the linter cannot attribute a capability to it and must not claim the
	// declared set is unused.
	dynamicHostCall []token.Position
}

func lintPlugin(args []string, stdout, stderr io.Writer) error {
	dir := "."
	if len(args) > 0 && args[0] != "" {
		dir = args[0]
	}
	findings, err := lintDir(dir)
	if err != nil {
		return err
	}

	var errors, warnings int
	for _, f := range findings {
		if f.sev == sevError {
			errors++
			_, _ = fmt.Fprintf(stderr, "error: %s\n", f.msg)
		} else {
			warnings++
			_, _ = fmt.Fprintf(stderr, "warning: %s\n", f.msg)
		}
	}

	if errors == 0 && warnings == 0 {
		_, _ = fmt.Fprintf(stdout, "%s: no problems found\n", dir)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "%s: %d error(s), %d warning(s)\n", dir, errors, warnings)
	if errors > 0 {
		return fmt.Errorf("lint found %d error(s)", errors)
	}
	return nil
}

func lintDir(dir string) ([]finding, error) {
	manifest, manifestErr := plugin.ValidateManifestDir(dir)
	var findings []finding
	if manifestErr != nil {
		findings = append(findings, finding{sevError, fmt.Sprintf("plugin.json: %v", manifestErr)})
		// A manifest that will not load cannot be cross-referenced usefully,
		// but the source checks below are still worth running.
	}

	u, err := scanSource(dir)
	if err != nil {
		return nil, err
	}

	findings = append(findings, lintHooks(manifest, u)...)
	findings = append(findings, lintPermissions(manifest, u, declaredHooks(manifest))...)
	findings = append(findings, lintSchema(dir)...)

	sort.SliceStable(findings, func(i, j int) bool { return findings[i].sev < findings[j].sev })
	return findings, nil
}

func lintHooks(manifest plugin.PluginManifest, u *usage) []finding {
	var out []finding

	declared := map[string]bool{}
	for _, h := range manifest.Hooks {
		if !sdk.IsHook(h.Name) {
			out = append(out, finding{sevError, fmt.Sprintf(
				"plugin.json declares unknown hook %q — valid hooks: %s",
				h.Name, strings.Join(sdk.Hooks, ", "))})
			continue
		}
		declared[h.Name] = true
	}

	for hook, pos := range u.registeredInMain {
		out = append(out, finding{sevError, fmt.Sprintf(
			"%s: %s is registered inside func main(), which never runs — "+
				"with -buildmode=c-shared the host calls _initialize, so registration must be in func init(). "+
				"This plugin would load healthy and never act",
			pos, hook)})
	}

	for hook := range declared {
		_, registered := u.hooks[hook]
		_, inMain := u.registeredInMain[hook]
		if !registered && !inMain {
			out = append(out, finding{sevError, fmt.Sprintf(
				"plugin.json declares hook %q but no sdk.%s call registers a handler — "+
					"the host loads the plugin healthy and the hook never acts",
				hook, registrationFor(hook))})
		}
	}

	for hook, pos := range u.hooks {
		if !declared[hook] {
			out = append(out, finding{sevError, fmt.Sprintf(
				"%s: a handler is registered for %q but plugin.json does not declare it — "+
					"the host skips undeclared hooks, so this handler is never called",
				pos, hook)})
		}
	}
	return out
}

func registrationFor(hook string) string {
	for call, h := range sdkHook {
		if h == hook {
			return call
		}
	}
	return "On…"
}

func declaredHooks(manifest plugin.PluginManifest) map[string]bool {
	out := map[string]bool{}
	for _, h := range manifest.Hooks {
		out[h.Name] = true
	}
	return out
}

func lintPermissions(manifest plugin.PluginManifest, u *usage, hooks map[string]bool) []finding {
	var out []finding

	declared := map[string]bool{}
	for _, p := range manifest.Permissions {
		if !sdk.IsPermission(p.Name) {
			out = append(out, finding{sevError, fmt.Sprintf(
				"plugin.json requests unknown capability %q", p.Name)})
			continue
		}
		if declared[p.Name] {
			out = append(out, finding{sevWarning, fmt.Sprintf(
				"plugin.json requests %q more than once", p.Name)})
		}
		declared[p.Name] = true
		if strings.TrimSpace(p.Description) == "" {
			out = append(out, finding{sevWarning, fmt.Sprintf(
				"capability %q has no description — an operator approving this "+
					"bundle sees the description and nothing else", p.Name)})
		}
	}

	// Used but not declared: the host refuses the call at runtime and hands
	// back a denial envelope most plugins discard, so this fails silently.
	for perm, pos := range u.permissions {
		if !declared[perm] {
			out = append(out, finding{sevError, fmt.Sprintf(
				"%s: uses %q but plugin.json does not request it — "+
					"the host refuses the call and returns a permission-denied envelope",
				pos, perm)})
		}
	}

	// Declared but unused: an operator is being asked to approve a capability
	// the plugin never exercises. Only a warning, and suppressed entirely when
	// a dynamic HostCall means the linter cannot see every capability used.
	if len(u.dynamicHostCall) == 0 {
		var unused []string
		for perm := range declared {
			if _, used := u.permissions[perm]; used {
				continue
			}
			if unattributable[perm] {
				continue
			}
			// A hook gate is used by declaring its hook, not by calling
			// anything. Report it only when the hook is absent — in which case
			// the grant really is dead weight.
			if hook, ok := hookGatePermission[perm]; ok {
				if hooks[hook] {
					continue
				}
				out = append(out, finding{sevWarning, fmt.Sprintf(
					"plugin.json requests %q but does not declare the %q hook it gates",
					perm, hook)})
				continue
			}
			unused = append(unused, perm)
		}
		sort.Strings(unused)
		for _, perm := range unused {
			out = append(out, finding{sevWarning, fmt.Sprintf(
				"plugin.json requests %q but no code uses it — "+
					"asking an operator to approve a capability the plugin never exercises", perm)})
		}
	}
	return out
}

func lintSchema(dir string) []finding {
	path := filepath.Join(dir, "schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil // schema.json is optional
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return []finding{{sevWarning, "schema.json is empty"}}
	}
	return nil
}

// scanSource walks the plugin's Go sources and records every SDK capability
// and hook registration it can see.
func scanSource(dir string) (*usage, error) {
	u := &usage{
		permissions:      map[string]token.Position{},
		hooks:            map[string]token.Position{},
		registeredInMain: map[string]token.Position{},
	}

	// Follow the plugin's own import graph, starting at its root package.
	//
	// Two earlier versions of this were wrong in opposite directions. Scanning
	// only the root directory missed a capability used from a subpackage — the
	// silent failure this command exists to prevent — and reported a handler
	// registered from one as never registered, blocking a valid build. Scanning
	// the whole directory tree then went too far: a `tools/` or `internal/gen`
	// package that nothing imports is not compiled into the plugin, so
	// reporting its capabilities rejects a plugin that is perfectly correct.
	//
	// Reachability is the property that matters, because it is what the
	// compiler uses. Local imports are resolved against the module path in
	// go.mod, which is also the boundary `torana plugin install` stages: a
	// plugin cannot import local code from outside its own directory, or the
	// staged build would not compile.
	//
	// Third-party dependencies are deliberately NOT followed. A module calling
	// sdk.HostCall on the plugin's behalf is invisible here, and the host
	// refuses it at runtime like any other ungranted call. Auditing
	// dependencies is a different job from checking a manifest against its own
	// source.
	fset := token.NewFileSet()
	files, err := reachableGoFiles(fset, dir)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		scanFile(fset, file, u)
	}
	return u, nil
}

// wasiBuildContext selects files the way the compiler will for the target a
// plugin is actually built for: GOOS=wasip1 GOARCH=wasm with cgo off. A
// //go:build linux file, a _darwin.go, or a cgo-only file is not in the binary
// the host loads, so scanning it would credit the plugin with handlers and
// capabilities that do not exist at runtime.
func wasiBuildContext() build.Context {
	ctx := build.Default
	ctx.GOOS = "wasip1"
	ctx.GOARCH = "wasm"
	ctx.CgoEnabled = false
	return ctx
}

// reachableGoFiles parses the plugin's root package and every local package it
// transitively imports, and returns the parsed files in visit order.
func reachableGoFiles(fset *token.FileSet, root string) ([]*ast.File, error) {
	modulePath, err := modulePathOf(root)
	if err != nil {
		return nil, err
	}

	buildCtx := wasiBuildContext()
	var out []*ast.File
	visited := map[string]bool{}

	var visit func(dir string) error
	visit = func(dir string) error {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		if visited[abs] {
			return nil
		}
		visited[abs] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			// An import that resolves to no directory is the compiler's problem
			// to report, not the linter's.
			return nil //nolint:nilerr
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			included, err := buildCtx.MatchFile(dir, name)
			if err != nil || !included {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				// A file that does not parse is the compiler's problem too;
				// skip it rather than duplicating a worse version of the error.
				continue
			}
			out = append(out, file)

			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				sub, ok := localImportDir(root, modulePath, path)
				if !ok {
					continue // stdlib or a third-party module
				}
				if err := visit(sub); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := visit(root); err != nil {
		return nil, err
	}
	return out, nil
}

// localImportDir maps an import path to a directory inside the plugin, or
// reports that it is not local to this module.
func localImportDir(root, modulePath, importPath string) (string, bool) {
	if modulePath == "" || importPath == modulePath {
		return root, importPath == modulePath
	}
	prefix := modulePath + "/"
	if !strings.HasPrefix(importPath, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(importPath, prefix)
	return filepath.Join(root, filepath.FromSlash(rel)), true
}

// modulePathOf reads the module path from the plugin's go.mod. A plugin without
// one has no local packages to reach, so its root is the whole graph.
//
// Parsed with modfile rather than by hand. A hand-rolled "strip the module
// prefix and trim" reads `module example.com/plug // the plugin` as a path
// including the comment, which then matches no import — and the linter silently
// stops seeing every subpackage. That is the worst possible failure for this
// command: it reports success on a plugin it never really examined.
func modulePathOf(dir string) (string, error) {
	path := filepath.Join(dir, "go.mod")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	return modfile.ModulePath(raw), nil
}

func scanFile(fset *token.FileSet, file *ast.File, u *usage) {
	alias := sdkAlias(file)
	if alias == "" {
		return
	}

	// Walk each declaration separately rather than inspecting the whole file
	// with a running "which function am I in" variable. That variable is set on
	// entering a FuncDecl and never cleared on leaving it, so everything after
	// `func main()` — including a package-level var initializer, which is a
	// perfectly good place to register a handler — inherited "main" and was
	// wrongly reported as dead code. Walking per-declaration makes the scope
	// exact by construction.
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue // external or assembly declaration
			}
			enclosing := ""
			if d.Recv == nil {
				enclosing = d.Name.Name
			}
			scanNode(fset, d.Body, alias, enclosing, u)
		default:
			// Package-level var/const initializers run before main and are a
			// valid registration site.
			scanNode(fset, decl, alias, "", u)
		}
	}
}

func scanNode(fset *token.FileSet, node ast.Node, alias, enclosing string, u *usage) {
	ast.Inspect(node, func(n ast.Node) bool {
		// A function literal is its own scope for this purpose: a handler
		// registered inside one still runs wherever the literal is invoked.
		// Its enclosing declaration is what matters, so it is inherited.
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != alias {
			return true
		}

		pos := fset.Position(sel.Pos())
		fn := sel.Sel.Name

		if hook, ok := sdkHook[fn]; ok {
			if enclosing == "main" {
				u.registeredInMain[hook] = pos
			} else if _, seen := u.hooks[hook]; !seen {
				u.hooks[hook] = pos
			}
			return true
		}

		if perm, ok := sdkPermission[fn]; ok {
			if _, seen := u.permissions[perm]; !seen {
				u.permissions[perm] = pos
			}
			return true
		}

		if fn == "HostCall" {
			cmd, ok := literalString(call, 0)
			if !ok {
				u.dynamicHostCall = append(u.dynamicHostCall, pos)
				return true
			}
			perm := permissionForCommand(cmd)
			if _, seen := u.permissions[perm]; !seen {
				u.permissions[perm] = pos
			}
		}
		return true
	})
}

// permissionForCommand mirrors the host's derivation at
// internal/wasm/runtime.go: an `env.`-prefixed command IS its own permission,
// anything else is namespaced under env.host_call. The two names are not
// derivable from each other, which is exactly why a linter is worth having.
func permissionForCommand(cmd string) string {
	if strings.HasPrefix(cmd, "env.") {
		return cmd
	}
	return "env.host_call." + cmd
}

func sdkAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != sdkModulePath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return "plugin_sdk" // the SDK's package name when unaliased
	}
	return ""
}

func literalString(call *ast.CallExpr, index int) (string, bool) {
	if len(call.Args) <= index {
		return "", false
	}
	lit, ok := call.Args[index].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}
