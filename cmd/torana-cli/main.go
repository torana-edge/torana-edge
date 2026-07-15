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
		if len(os.Args) < 3 || os.Args[2] != "build" {
			printUsage()
			os.Exit(1)
		}
		buildPlugin(os.Args[3:])
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("torana-cli: Torana Edge management tool")
	fmt.Println("\nUsage:")
	fmt.Println("  torana-cli plugin build [path/to/plugin/dir] [-o output.wasm]")
	fmt.Println("\nExample:")
	fmt.Println("  torana-cli plugin build ./plugins/compactor -o ./plugins/compactor/plugin.wasm")
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
