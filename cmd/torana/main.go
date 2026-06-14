// Torana Edge – stateful AI FinOps reverse proxy.
//
// Entry point.  Reads a .env file, wires the proxy server, and blocks until
// the process receives a termination signal.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/projectescape/torana-edge/internal/proxy"
)

// loadEnv reads a bare-bones key=value .env file (no quoting, no escaping)
// and copies every key into the process environment so os.Getenv works.
// For a real deployment you would use a library like joho/godotenv; this
// hand-rolled version keeps the MVP dependency-free.
func loadEnv(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		os.Setenv(key, val)
	}
	return nil
}

func main() {
	// --- configuration --------------------------------------------------
	// Try .env in the current working directory; soft-fail if missing
	// (the user may have set env vars directly).
	if err := loadEnv(".env"); err != nil {
		log.Printf("No .env file found (%v) – using process environment", err)
	}

	cfg := proxy.Config{
		Port:        envOrDefault("TORANA_PORT", "8080"),
		UpstreamURL: os.Getenv("UPSTREAM_URL"),
		Provider:    os.Getenv("UPSTREAM_PROVIDER"),
	}

	if cfg.UpstreamURL == "" {
		log.Fatal("UPSTREAM_URL is required. Set it in .env or the environment.")
	}
	if cfg.Provider == "" {
		// Infer from the URL if possible, otherwise require explicit config.
		switch {
		case strings.Contains(cfg.UpstreamURL, "anthropic"):
			cfg.Provider = "anthropic"
		case strings.Contains(cfg.UpstreamURL, "openai"):
			cfg.Provider = "openai"
		default:
			log.Fatal("UPSTREAM_PROVIDER is required. Set it to 'anthropic' or 'openai' in .env or the environment.")
		}
	}

	// --- server ---------------------------------------------------------
	srv, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create proxy server: %v", err)
	}

	// Graceful shutdown on Ctrl+C / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("Shutting down gracefully…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5e9) // 5 s
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
	log.Println("Torana Edge stopped.")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
