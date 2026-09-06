package plugincmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/pluginfiles"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func operatorFileStore() (*pluginfiles.Store, error) {
	configPath, err := provider.ManagedStorePath()
	if err != nil {
		return nil, err
	}
	return pluginfiles.New(filepath.Join(filepath.Dir(configPath), "plugin-data"))
}

func listPluginFiles(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: torana plugin files <name>")
	}
	store, err := operatorFileStore()
	if err != nil {
		return err
	}
	paths, err := store.OperatorList(args[0])
	if err != nil {
		return err
	}
	for _, path := range paths {
		if _, err := fmt.Fprintln(stdout, path); err != nil {
			return err
		}
	}
	return nil
}

func pluginFile(args []string, stdout io.Writer) error {
	return pluginFileWithPathResolver(args, stdout, runningPluginFilePath)
}

func pluginFileWithPathResolver(args []string, stdout io.Writer, resolvePath func(string, string) (string, error)) error {
	if len(args) < 2 {
		return errors.New("usage: torana plugin file <path|read|tail|purge> <name> [logical-path]")
	}
	switch args[0] {
	case "path":
		if len(args) != 3 {
			return errors.New("usage: torana plugin file path <name> <logical-path>")
		}
		path, err := resolvePath(args[1], args[2])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, path)
		return err
	case "read":
		store, err := operatorFileStore()
		if err != nil {
			return err
		}
		if len(args) != 3 {
			return errors.New("usage: torana plugin file read <name> <logical-path>")
		}
		data, err := store.OperatorRead(args[1], args[2])
		if err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	case "tail":
		store, err := operatorFileStore()
		if err != nil {
			return err
		}
		follow := len(args) == 4 && args[3] == "--follow"
		if len(args) != 3 && !follow {
			return errors.New("usage: torana plugin file tail <name> <logical-path> [--follow]")
		}
		return tailPluginFile(store, args[1], args[2], follow, stdout)
	case "purge":
		store, err := operatorFileStore()
		if err != nil {
			return err
		}
		if len(args) != 2 {
			return errors.New("usage: torana plugin file purge <name>")
		}
		if err := store.OperatorPurge(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Purged private files for %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown plugin file command %q", args[0])
	}
}

func runningPluginFilePath(plugin, logical string) (string, error) {
	port := strings.TrimSpace(os.Getenv("TORANA_PORT"))
	if port == "" {
		port = "8080"
	}
	return pluginFilePathAt(&http.Client{Timeout: 3 * time.Second}, "http://127.0.0.1:"+port, plugin, logical)
}

func pluginFilePathAt(client *http.Client, baseURL, plugin, logical string) (string, error) {
	endpoint, err := url.Parse(baseURL + "/_torana/api/v1/plugin-files/path")
	if err != nil {
		return "", fmt.Errorf("resolve running Torana URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("plugin", plugin)
	query.Set("logical", logical)
	endpoint.RawQuery = query.Encode()
	response, err := client.Get(endpoint.String())
	if err != nil {
		return "", fmt.Errorf("ask running Torana for plugin file path: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return "", fmt.Errorf("running Torana refused plugin file path (HTTP %d): %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var result struct {
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<10))
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

func tailPluginFile(store *pluginfiles.Store, plugin, logical string, follow bool, stdout io.Writer) error {
	data, err := store.OperatorRead(plugin, logical)
	if err != nil {
		return err
	}
	start := 0
	if len(data) > 64<<10 {
		start = len(data) - (64 << 10)
		if nl := bytes.IndexByte(data[start:], '\n'); nl >= 0 {
			start += nl + 1
		}
	}
	if _, err := stdout.Write(data[start:]); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	offset := len(data)
	for {
		time.Sleep(time.Second)
		current, err := store.OperatorRead(plugin, logical)
		if err != nil {
			return err
		}
		if len(current) < offset {
			offset = 0
		}
		if len(current) > offset {
			if _, err := stdout.Write(current[offset:]); err != nil {
				return err
			}
			offset = len(current)
		}
	}
}
