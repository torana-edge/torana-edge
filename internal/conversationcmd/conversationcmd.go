// Package conversationcmd implements `torana conversations`.
//
// Unlike `torana plugin`, which works on files on disk, conversations exist
// only in a running proxy's memory. This command therefore queries the live
// control plane over HTTP rather than reading anything local — if the proxy is
// not running there is nothing to list, and saying so plainly is more useful
// than printing an empty table.
package conversationcmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/conversation"
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
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conversations accepts options, not positional arguments")
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

func fetch(addr string) ([]conversation.Record, error) {
	client, err := controlclient.New(addr, requestTimeout)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	body, _, err := client.JSON(context.Background(), http.MethodGet, controlclient.BasePath+"/conversations", nil, "")
	if err != nil {
		return nil, err
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
