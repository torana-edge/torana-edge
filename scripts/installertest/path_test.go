package installertest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallerPersistsDefaultPathWithoutSourcing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX profile test")
	}
	for _, shell := range []string{"zsh", "bash", "fish", "sh"} {
		t.Run(shell, func(t *testing.T) {
			f := newInstaller(t, runtime.GOOS, runtime.GOARCH)
			f.env["TORANA_INSTALL_DIR"] = ""
			f.env["SHELL"] = "/bin/" + shell
			f.env["ZDOTDIR"] = f.home
			f.env["XDG_CONFIG_HOME"] = filepath.Join(f.home, ".config")
			f.installDir = filepath.Join(f.home, ".local", "bin")
			profile := filepath.Join(f.home, ".profile")
			switch shell {
			case "zsh":
				profile = filepath.Join(f.home, ".zshrc")
			case "bash":
				profile = filepath.Join(f.home, ".bashrc")
				if runtime.GOOS == "darwin" {
					profile = filepath.Join(f.home, ".bash_profile")
				}
			case "fish":
				profile = filepath.Join(f.home, ".config", "fish", "config.fish")
			}
			must(t, os.MkdirAll(filepath.Dir(profile), 0o700))
			// This startup command must never be evaluated by the installer.
			original := "exit 17 # preserve existing startup content\n"
			must(t, os.WriteFile(profile, []byte(original), 0o600))
			f.wantSuccess()
			data, err := os.ReadFile(profile)
			must(t, err)
			if !strings.HasPrefix(string(data), original) || !strings.Contains(string(data), "# Torana PATH") {
				t.Fatal("profile content was not preserved and extended")
			}
			if shell == "fish" && !strings.Contains(string(data), "fish_add_path") {
				t.Fatal("fish received POSIX syntax")
			}
			first := string(data)
			f.wantSuccess()
			data, err = os.ReadFile(profile)
			must(t, err)
			if string(data) != first {
				t.Fatal("repeat install duplicated PATH setup")
			}
		})
	}
}
