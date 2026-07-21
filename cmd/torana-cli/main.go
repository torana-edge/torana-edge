package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "plugin":
		if len(os.Args) < 3 {
			printUsage()
			os.Exit(1)
		}
		switch os.Args[2] {
		case "build":
			buildPlugin(os.Args[3:])
		case "init":
			initPlugin(os.Args[3:])
		default:
			printUsage()
			os.Exit(1)
		}
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("torana-cli: Torana Edge management tool")
	fmt.Println("\nUsage:")
	fmt.Println("  torana-cli plugin init <name>")
	fmt.Println("  torana-cli plugin build [path/to/plugin/dir] [-o output.wasm]")
	fmt.Println("\nExamples:")
	fmt.Println("  torana-cli plugin init my-plugin")
	fmt.Println("  torana-cli plugin build ./plugins/compactor -o ./plugins/compactor/plugin.wasm")
}

func initPlugin(args []string) {
	if len(args) < 1 || args[0] == "" {
		fmt.Println("Error: plugin name is required")
		fmt.Println("Usage: torana-cli plugin init <name>")
		os.Exit(1)
	}

	pluginDir := args[0]
	pluginName := filepath.Base(pluginDir)

	absDir, err := filepath.Abs(pluginDir)
	if err != nil {
		fmt.Printf("Error resolving directory: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(absDir, 0755); err != nil {
		fmt.Printf("Error creating directory %s: %v\n", absDir, err)
		os.Exit(1)
	}

	goModPath := filepath.Join(absDir, "go.mod")
	if _, err := os.Stat(goModPath); err == nil {
		fmt.Printf("Error: %s already contains a go.mod file\n", absDir)
		os.Exit(1)
	}

	goModContent := fmt.Sprintf(`module %s

go 1.23

require github.com/torana-edge/torana-edge/sdk v0.1.0
`, pluginName)

	pluginGoPath := filepath.Join(absDir, "plugin.wasm.go")
	pluginGoContent := `package main

import (
	"context"

	"github.com/torana-edge/torana-edge/sdk/pb"
	sdk "github.com/torana-edge/torana-edge/sdk/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		return req, nil
	})
}
`

	pluginJsonPath := filepath.Join(absDir, "plugin.json")
	pluginJsonContent := fmt.Sprintf(`{
  "name": "%s",
  "version": "0.1.0",
  "description": "Scaffolded Torana Edge plugin",
  "hooks": [
    {"name": "run_before_request", "priority": 100}
  ],
  "permissions": []
}
`, pluginName)

	if err := os.WriteFile(goModPath, []byte(goModContent), 0644); err != nil {
		fmt.Printf("Error writing go.mod: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(pluginGoPath, []byte(pluginGoContent), 0644); err != nil {
		fmt.Printf("Error writing plugin.wasm.go: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(pluginJsonPath, []byte(pluginJsonContent), 0644); err != nil {
		fmt.Printf("Error writing plugin.json: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully initialized plugin %q in %s\n\n", pluginName, absDir)
	fmt.Println("Note: For local development against an in-tree SDK, you may need to add a replace directive to go.mod:")
	fmt.Println("  replace github.com/torana-edge/torana-edge/sdk => /path/to/torana-edge/sdk")
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Build plugin WASM binary:\n     torana-cli plugin build %s\n", pluginDir)
	fmt.Println("  2. Drop plugin directory into your Torana plugins.dir location.")
	fmt.Println("  3. Enable and configure via the Torana control plane.")
}

func buildPlugin(args []string) {
	dir := "."
	out := ""

	// Very simple flag parsing
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			out = args[i+1]
			i++
		} else if dir == "." {
			dir = args[i]
		}
	}

	if out == "" {
		out = filepath.Join(dir, "plugin.wasm")
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Printf("Error resolving directory: %v\n", err)
		os.Exit(1)
	}

	absOut, err := filepath.Abs(out)
	if err != nil {
		fmt.Printf("Error resolving output path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Building Torana WASM plugin in %s...\n", absDir)

	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", absOut, ".")
	cmd.Dir = absDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Printf("\nBuild failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully built plugin at %s\n", absOut)
}
