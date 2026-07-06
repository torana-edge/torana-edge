package provider

import (
	"log"
	"os"
	"time"
)

// WatchConfig polls configPath every interval and calls onChange when the
// file modification time changes. Returns a stop function to cancel watching.
func WatchConfig(configPath string, interval time.Duration, onChange func(Config)) (stop func()) {
	done := make(chan struct{})

	go func() {
		var lastMod time.Time
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Read initial modtime.
		if fi, err := os.Stat(configPath); err == nil {
			lastMod = fi.ModTime()
		}

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fi, err := os.Stat(configPath)
				if err != nil {
					continue
				}
				if fi.ModTime().After(lastMod) {
					lastMod = fi.ModTime()
					cfg, err := Load(configPath)
					if err != nil {
						log.Printf("config hot-reload: failed to load %s: %v", configPath, err)
						continue
					}
					log.Printf("config hot-reload: %s changed, applying", configPath)
					onChange(cfg)
				}
			}
		}
	}()

	return func() { close(done) }
}
