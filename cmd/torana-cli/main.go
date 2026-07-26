// torana-cli is a compatibility wrapper. New documentation uses
// `torana plugin ...` so operators only install one binary.
package main

import (
	"log"
	"os"

	"github.com/torana-edge/torana-edge/internal/plugincmd"
)

func main() {
	if err := plugincmd.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Printf("plugin command: %v", err)
		os.Exit(2)
	}
}
