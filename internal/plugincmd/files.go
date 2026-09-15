package plugincmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
)

func pluginFile(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "path" {
		return errors.New("use torana plugin file path [--addr origin] <name> <logical-path>; use your shell's cat, tail, or Get-Content to read the returned path")
	}
	fs := flag.NewFlagSet("plugin file path", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "loopback control-plane origin")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(stdout, "Usage: torana plugin file path [--addr origin] <name> <logical-path>\nPrints only the running plugin's absolute file path. Use cat, tail, or Get-Content to read it.")
		}
		return err
	}
	return pluginFileWithPathResolver(append([]string{"path"}, fs.Args()...), stdout, func(plugin, logical string) (string, error) {
		client, err := controlclient.New(*addr, 3*time.Second)
		if err != nil {
			return "", err
		}
		defer client.Close()
		return pluginFilePathAt(client, plugin, logical)
	})
}

func pluginFileWithPathResolver(args []string, stdout io.Writer, resolvePath func(string, string) (string, error)) error {
	if len(args) != 3 || args[0] != "path" || args[1] == "" || args[2] == "" {
		return errors.New("usage: torana plugin file path [--addr origin] <name> <logical-path>")
	}
	path, err := resolvePath(args[1], args[2])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, path)
	return err
}

func pluginFilePathAt(client *controlclient.Client, plugin, logical string) (string, error) {
	query := url.Values{"plugin": {plugin}, "logical": {logical}}
	raw, _, err := client.JSON(context.Background(), http.MethodGet, controlclient.BasePath+"/plugin-files/path?"+query.Encode(), nil, "")
	if err != nil {
		return "", err
	}
	var result struct {
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return "", fmt.Errorf("decode running Torana plugin file path: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("decode running Torana plugin file path: trailing JSON")
	}
	if result.Path == "" || !filepath.IsAbs(result.Path) {
		return "", fmt.Errorf("running Torana returned a non-absolute plugin file path")
	}
	return filepath.Clean(result.Path), nil
}
