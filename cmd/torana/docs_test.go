package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The help text and the README both claim to list every environment variable
// Torana reads. Both claims were false, and one of them said so in writing:
// "torana help prints the same table, so it cannot drift out of the binary"
// sat directly under a table missing six variables.
//
// A list restated in a test cannot catch that — it drifts alongside the thing
// it checks. These three tests derive each list from the source that defines
// it, so adding a variable to the product fails the build until it is
// documented in both places.

// envReadPattern finds a literal environment variable name being read.
var envReadPattern = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("([A-Z0-9_]+)"\)`)

// envReadIndirect finds a read whose name this check cannot resolve, so an
// indirected read is reported rather than silently passed over.
var envReadIndirect = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(([^")][^)]*)\)`)

// documentedEnvPrefixes are the namespaces Torana is responsible for
// documenting. PATH and the like are the operating system's, and the OTLP
// exporter's own variables beyond the four we act on are OpenTelemetry's.
var documentedEnvPrefixes = []string{"TORANA_", "OTEL_"}

// operatorNamedEnvReads are the reads whose variable name is chosen by the
// operator in configuration rather than fixed by Torana. There is no name to
// document for these — the operator picks it — so they are exempt, by file
// and by the expression that names them, and a new indirection anywhere else
// still fails. Written down rather than skipped silently: an unexplained
// hole here is how "documents every variable" stops being true.
var operatorNamedEnvReads = map[string]string{
	"credential/credential.go:key":                   "the env var a named credential reads its secret from",
	"internal/cache/config.go:cfg.Redis.PasswordEnv": "the env var holding the Redis password",
	"internal/proxy/server.go:envName":               "the env var a provider reads its API key from",
}

func ours(name string) bool {
	for _, p := range documentedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// productionGoFiles lists the tracked Go files that end up in a release
// binary: no tests, and no build-tagged file excluded from release builds.
func productionGoFiles(t *testing.T, includeFixtures bool) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// torana_benchmark_profile is excluded from release builds on
		// purpose, so its variables are not part of the product's surface.
		if bytes.Contains(body, []byte("//go:build torana_benchmark_profile")) {
			return nil
		}
		// internal/testfixture is test scaffolding that happens not to live
		// in _test.go files; TestFixtureHelperIsTestOnly below proves no
		// production file imports it, so TORANA_E2E is not product surface.
		if !includeFixtures && strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(filepath.Join(root, "internal/testfixture"))) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no production Go files found; this check has stopped seeing what it guards")
	}
	return out
}

// envVarsRead is every TORANA_/OTEL_ variable the shipped binary reads.
func envVarsRead(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for rel, body := range productionGoFiles(t, false) {
		for _, m := range envReadPattern.FindAllStringSubmatch(body, -1) {
			if ours(m[1]) {
				seen[m[1]] = true
			}
		}
		for _, m := range envReadIndirect.FindAllStringSubmatch(body, -1) {
			expr := strings.TrimSpace(m[1])
			if _, ok := operatorNamedEnvReads[rel+":"+expr]; ok {
				continue
			}
			t.Errorf("%s reads an environment variable through %s, which this check cannot resolve. "+
				"Use a literal name at the call site, or — if the operator names this variable in "+
				"configuration — add it to operatorNamedEnvReads with the reason", rel, expr)
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestUsageDocumentsEveryEnvironmentVariable(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	help := buf.String()

	read := envVarsRead(t)
	if len(read) < 5 {
		t.Fatalf("only %d environment variables found (%v); the pattern has stopped matching", len(read), read)
	}
	for _, name := range read {
		if !strings.Contains(help, name) {
			t.Errorf("%s is read by Torana but absent from the help text", name)
		}
	}
}

// usageEnvNames are the variables the help text's Environment section names.
func usageEnvNames(t *testing.T) map[string]bool {
	t.Helper()
	var buf bytes.Buffer
	usage(&buf)
	help := buf.String()

	start := strings.Index(help, "Environment:\n")
	if start < 0 {
		t.Fatal("the help text has no Environment section")
	}
	section := help[start:]
	// The section ends at the first blank line followed by prose.
	if end := strings.Index(section, "\n\nThe control plane"); end >= 0 {
		section = section[:end]
	}
	names := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		// A name starts a line at two spaces of indent; continuation lines
		// are indented further and must not be mistaken for one.
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) > 0 && ours(f[0]) {
			names[f[0]] = true
		}
	}
	return names
}

// readmeEnvNames are the variables the README's Environment Variables table
// names in its first column.
func readmeEnvNames(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	text := string(body)
	start := strings.Index(text, "## Environment Variables")
	if start < 0 {
		t.Fatal("README.md has no Environment Variables section")
	}
	section := text[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	cell := regexp.MustCompile("(?m)^\\| `([A-Z0-9_]+)` \\|")
	names := map[string]bool{}
	for _, m := range cell.FindAllStringSubmatch(section, -1) {
		names[m[1]] = true
	}
	return names
}

// The README table and the help text must name the same variables. This is
// the check the README's own sentence claimed to be, and was not.
func TestREADMEEnvironmentTableMatchesUsage(t *testing.T) {
	inHelp := usageEnvNames(t)
	inREADME := readmeEnvNames(t)

	if len(inHelp) == 0 || len(inREADME) == 0 {
		t.Fatalf("parsed %d names from the help text and %d from the README; one of the parsers has stopped working", len(inHelp), len(inREADME))
	}
	for name := range inHelp {
		if !inREADME[name] {
			t.Errorf("%s is in `torana help` but not in the README's Environment Variables table", name)
		}
	}
	for name := range inREADME {
		if !inHelp[name] {
			t.Errorf("%s is in the README's Environment Variables table but not in `torana help`", name)
		}
	}
}

// subcommandPattern finds the string literals main() dispatches on. Both
// dispatch shapes are here: the early `os.Args[1] == "x"` guards and the
// switch that follows them.
var (
	argEquals  = regexp.MustCompile(`os\.Args\[1\] == "([a-z-]+)"`)
	argCase    = regexp.MustCompile(`(?m)^\t\tcase "([a-z-]+)"(?:, "[^"]+")*:`)
	caseAlt    = regexp.MustCompile(`"([a-z][a-z-]*)"`)
	mainSwitch = regexp.MustCompile(`(?s)switch os\.Args\[1\] \{.*?\n\t\}`)
)

// Every subcommand main() dispatches must be in the help text, or a command
// exists that nobody can discover. Derived from main.go rather than restated:
// the previous version of this test listed the commands by hand.
func TestUsageDocumentsEverySubcommand(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	src := string(body)

	cmds := map[string]bool{}
	for _, m := range argEquals.FindAllStringSubmatch(src, -1) {
		cmds[m[1]] = true
	}
	if sw := mainSwitch.FindString(src); sw != "" {
		for _, line := range argCase.FindAllString(sw, -1) {
			for _, m := range caseAlt.FindAllStringSubmatch(line, -1) {
				cmds[m[1]] = true
			}
		}
	} else {
		t.Error("main.go no longer has a switch on os.Args[1]; this check has stopped seeing the dispatch")
	}
	// --debug is a flag, not a subcommand, and is documented as one line of
	// the Usage block rather than as a command name.
	delete(cmds, "--debug")

	if len(cmds) < 4 {
		t.Fatalf("found only %d dispatched subcommands (%v); the patterns have stopped matching", len(cmds), cmds)
	}

	var buf bytes.Buffer
	usage(&buf)
	help := buf.String()
	for cmd := range cmds {
		// serve is documented as the default, `torana [serve]`, so match the
		// command word inside the usage line rather than an exact prefix.
		if !strings.Contains(help, "torana "+cmd) && !strings.Contains(help, "torana ["+cmd+"]") {
			t.Errorf("subcommand %q is dispatched but absent from the help text", cmd)
		}
	}
}

// The exclusion of internal/testfixture from the environment-variable scan is
// only sound while the package really is test-only. If production code ever
// imports it, TORANA_E2E becomes product surface and the exclusion has to go.
func TestFixtureHelperIsTestOnly(t *testing.T) {
	for rel, body := range productionGoFiles(t, true) {
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/testfixture/") {
			continue
		}
		if strings.Contains(body, "internal/testfixture") {
			t.Errorf("%s is a production file importing internal/testfixture; "+
				"TORANA_E2E is now product surface and must be documented", rel)
		}
	}
}

// harnessList captures the harnesses the README's opening sentence promises.
var harnessList = regexp.MustCompile(`your harness \(([^)]+)\)`)

// The README names the harnesses Torana sits behind, and then sends the
// reader to the quickstart for "a worked example for each of those". Those
// two lists disagreed: the README advertised Codex, which the quickstart
// never mentioned, while the quickstart led with oh-my-pi, which the README
// never mentioned. A reader following the promise found nothing.
func TestEveryAdvertisedHarnessHasAQuickstartSection(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	m := harnessList.FindSubmatch(readme)
	if m == nil {
		t.Fatal("README.md no longer names the harnesses in its opening sentence; this check has stopped seeing the promise")
	}
	quickstart, err := os.ReadFile(filepath.Join("..", "..", "docs", "QUICKSTART.md"))
	if err != nil {
		t.Fatalf("reading docs/QUICKSTART.md: %v", err)
	}
	var headings []string
	for _, line := range strings.Split(string(quickstart), "\n") {
		if strings.HasPrefix(line, "### ") {
			headings = append(headings, line)
		}
	}
	if len(headings) == 0 {
		t.Fatal("docs/QUICKSTART.md has no harness sections")
	}

	for _, name := range strings.Split(string(m[1]), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := false
		for _, h := range headings {
			if strings.Contains(strings.ToLower(h), strings.ToLower(name)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("README.md advertises %q but docs/QUICKSTART.md has no section for it, "+
				"and the README sends the reader there for a worked example", name)
		}
	}
}

// The startup banner prints a control-plane URL for an operator to open, so
// that URL has to actually answer. A wildcard bind is an address to LISTEN on,
// not one to connect to; and a SPECIFIC non-loopback bind serves the control
// plane nowhere at all — the bind address is refused by the guard as a
// non-loopback source (403), and loopback is refused by the kernel because
// nothing is listening there.
//
// An earlier version of this printed the bind address in every case and called
// the result "always reachable", which advertised a URL that cannot work.
func TestControlPlaneHostNamesSomethingThatAnswers(t *testing.T) {
	for _, tc := range []struct {
		bind      string
		want      string
		reachable bool
	}{
		{bind: "127.0.0.1", want: "127.0.0.1", reachable: true},
		{bind: "127.0.0.2", want: "127.0.0.2", reachable: true},
		{bind: "::1", want: "::1", reachable: true},
		// Unbracketed: net.JoinHostPort does the bracketing, and returning
		// "[::1]" here printed http://[[::1]]:8143/.
		{bind: "[::1]", want: "::1", reachable: true},
		{bind: "localhost", want: "localhost", reachable: true},
		{bind: "", want: "127.0.0.1", reachable: true},
		{bind: "0.0.0.0", want: "127.0.0.1", reachable: true},
		// An IPv6 wildcard is covered by [::1], not by 127.0.0.1: the socket
		// may be v6-only, and then nothing answers on IPv4 loopback.
		{bind: "::", want: "::1", reachable: true},
		{bind: "[::]", want: "::1", reachable: true},
		// Serves the control plane nowhere.
		{bind: "192.168.1.10", reachable: false},
		{bind: "10.0.0.5", reachable: false},
		{bind: "not-an-address", reachable: false},
	} {
		got, reachable := controlPlaneHost(tc.bind)
		if reachable != tc.reachable {
			t.Errorf("controlPlaneHost(%q) reachable = %v, want %v", tc.bind, reachable, tc.reachable)
			continue
		}
		if tc.reachable && got != tc.want {
			t.Errorf("controlPlaneHost(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

// And the mapping must correspond to a socket that really answers. This binds
// for real and asks the control plane, rather than trusting the string.
//
// Only the bind shapes a test can portably create are exercised: 127.0.0.1 and
// the IPv4 wildcard. A specific non-loopback bind cannot be simulated here —
// every 127.0.0.0/8 address is loopback — which is why the case above is
// asserted as "advertise nothing" rather than as a live 403.
func TestAdvertisedControlPlaneURLAnswers(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "0.0.0.0"} {
		t.Run(bind, func(t *testing.T) {
			host, reachable := controlPlaneHost(bind)
			if !reachable {
				t.Fatalf("controlPlaneHost(%q) says unreachable; this test assumes it is", bind)
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
			if err != nil {
				t.Skipf("cannot bind %s here: %v", bind, err)
			}
			defer ln.Close()
			_, port, err := net.SplitHostPort(ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}

			// A stand-in for the control plane with the same loopback rule the
			// real guard applies to the remote address.
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ip, _, err := net.SplitHostPort(r.RemoteAddr)
				if err != nil || !net.ParseIP(ip).IsLoopback() {
					http.Error(w, "control plane is localhost-only", http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusOK)
			})}
			go srv.Serve(ln)
			defer srv.Close()

			url := "http://" + net.JoinHostPort(strings.Trim(host, "[]"), port) + "/_torana/"
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
			if err != nil {
				t.Fatalf("the advertised control-plane URL %s does not answer: %v", url, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("the advertised control-plane URL %s answered %d; an operator "+
					"following the banner would be refused", url, resp.StatusCode)
			}
		})
	}
}

// Every package under internal/ must say what it is.
//
// Nine of twenty-three did not, so `go doc ./internal/engine` opened on a
// variable declaration and `go doc ./internal/wasm` on a helper — for the two
// packages that define the IR and run the sandbox. A reader arriving at the
// tree has no entry point when the packages do not introduce themselves, and
// this is the repository a plugin author reads to understand the host.
//
// Derived from the directory listing rather than a list restated here, so a
// new package is covered the day it is added.
func TestEveryInternalPackageSaysWhatItIs(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	var undocumented []string
	checked := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var goFiles []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				goFiles = append(goFiles, filepath.Join(path, e.Name()))
			}
		}
		if len(goFiles) == 0 {
			return nil
		}
		checked++
		want := "// Package " + filepath.Base(path)
		for _, f := range goFiles {
			body, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			if strings.HasPrefix(string(body), want) {
				return nil
			}
		}
		rel, _ := filepath.Rel(root, path)
		undocumented = append(undocumented, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
	if checked == 0 {
		t.Fatal("no packages found under internal/; this check has stopped seeing what it guards")
	}
	for _, pkg := range undocumented {
		t.Errorf("internal/%s has no package comment, so `go doc` on it opens on whichever "+
			"declaration happens to come first", pkg)
	}
}
