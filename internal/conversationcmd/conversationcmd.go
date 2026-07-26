// Package conversationcmd implements `torana conversations`.
//
// Unlike `torana plugin`, which works on files on disk, conversations exist
// only in a running proxy's memory. This command therefore queries the live
// control plane over HTTP rather than reading anything local — if the proxy is
// not running there is nothing to list, and saying so plainly is more useful
// than printing an empty table.
package conversationcmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/conversation"
	"github.com/torana-edge/torana-edge/internal/provider"
)

const requestTimeout = 5 * time.Second

// Run executes the conversations command. args starts with the subcommand name.
func Run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("conversations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut bool
		addr    string
	)
	fs.BoolVar(&jsonOut, "json", false, "emit raw JSON instead of a table")
	fs.StringVar(&addr, "addr", "", "control-plane address (default: the configured port on localhost)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: torana conversations [--json] [--addr host:port]\n\n"+
			"Lists conversations the running proxy has seen recently, most recent first.\n"+
			"Metadata only — message content is never recorded.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if addr == "" {
		addr = defaultAddr()
	}

	records, err := fetch(addr)
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(records)
	}
	writeTable(stdout, records)
	return nil
}

// defaultAddr resolves the port the proxy is configured to listen on, using the
// same precedence as the server: TORANA_PORT overrides the managed store, which
// overrides the seed file.
func defaultAddr() string {
	if v := os.Getenv("TORANA_PORT"); v != "" {
		if _, err := strconv.Atoi(v); err == nil {
			return "127.0.0.1:" + v
		}
	}
	seedPath := "config.json"
	if v := os.Getenv("TORANA_CONFIG"); v != "" {
		seedPath = v
	}
	if storePath, err := provider.ManagedStorePath(); err == nil {
		if cfg, err := provider.ResolveConfig(seedPath, storePath); err == nil && cfg.Port > 0 {
			return "127.0.0.1:" + strconv.Itoa(cfg.Port)
		}
	}
	return "127.0.0.1:8080"
}

func fetch(addr string) ([]conversation.Record, error) {
	url := "http://" + addr + "/_torana/api/conversations"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// The control plane refuses cross-origin mutations; this is a read, but the
	// header also marks the request as deliberately local.
	req.Header.Set("X-Torana-Local-Request", "1")

	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the proxy at %s — is it running? (%w)", addr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Conversations []conversation.Record `json:"conversations"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("could not parse the response: %w", err)
	}
	return payload.Conversations, nil
}

func writeTable(w io.Writer, records []conversation.Record) {
	if len(records) == 0 {
		_, _ = fmt.Fprintln(w, "No conversations recorded yet. Send a request through the proxy and try again.")
		return
	}

	rows := make([][5]string, 0, len(records))
	widths := [5]int{len("ID"), len("LAST ACTIVE"), len("TURNS"), len("MODEL"), len("CACHE")}
	header := [5]string{"ID", "LAST ACTIVE", "TURNS", "MODEL", "CACHE"}

	now := time.Now()
	for _, r := range records {
		row := [5]string{
			r.ID,
			humanizeAge(now.Sub(r.LastActive)),
			strconv.Itoa(r.Turns),
			r.Model,
			cacheSummary(r),
		}
		for i := range row {
			if len(row[i]) > widths[i] {
				widths[i] = len(row[i])
			}
		}
		rows = append(rows, row)
	}

	printRow(w, header, widths)
	for _, row := range rows {
		printRow(w, row, widths)
	}
}

func printRow(w io.Writer, row [5]string, widths [5]int) {
	parts := make([]string, len(row))
	for i, cell := range row {
		if i == len(row)-1 {
			parts[i] = cell // no trailing padding on the last column
			continue
		}
		parts[i] = cell + strings.Repeat(" ", widths[i]-len(cell))
	}
	_, _ = fmt.Fprintln(w, strings.Join(parts, "  "))
}

// cacheSummary reports what the provider said about the last turn's cache. This
// is the number that decides whether keeping a conversation warm is worth
// anything: reads mean the cache was live, writes mean it had to be rebuilt.
func cacheSummary(r conversation.Record) string {
	switch {
	case r.LastCacheRead > 0 && r.LastCacheWrite > 0:
		return fmt.Sprintf("%s read, %s written", compactCount(r.LastCacheRead), compactCount(r.LastCacheWrite))
	case r.LastCacheRead > 0:
		return compactCount(r.LastCacheRead) + " read"
	case r.LastCacheWrite > 0:
		return compactCount(r.LastCacheWrite) + " written"
	default:
		return "—"
	}
}

func compactCount(n int) string {
	if n >= 1000 {
		return strconv.FormatFloat(float64(n)/1000, 'f', -1, 64) + "k"
	}
	return strconv.Itoa(n)
}

func humanizeAge(d time.Duration) string {
	switch {
	case d < 0:
		// A clock skew between proxy and CLI should not print a negative age.
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}
