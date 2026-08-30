package plugincmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
	if len(args) < 2 {
		return errors.New("usage: torana plugin file <read|tail|purge> <name> [logical-path]")
	}
	store, err := operatorFileStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "read":
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
		follow := len(args) == 4 && args[3] == "--follow"
		if len(args) != 3 && !follow {
			return errors.New("usage: torana plugin file tail <name> <logical-path> [--follow]")
		}
		return tailPluginFile(store, args[1], args[2], follow, stdout)
	case "purge":
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
