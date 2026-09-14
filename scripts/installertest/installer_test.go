// Package installertest exercises the shipped scripts with synthetic release
// archives and a fake curl process. It never downloads releases or installs in
// the developer's home directory. Hashing, extraction, file replacement and the
// native shell/PowerShell interpreter are real.
package installertest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const releaseURL = "https://github.com/torana-edge/torana-edge/releases"

// The test executable doubles as the mocked curl/uname, including on Windows.
// This keeps fixtures independent of Python, Bash, Pester and network access.
func init() {
	if os.Getenv("TORANA_TEST_HELPER") != "1" {
		return
	}
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "uname":
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		switch os.Args[1] {
		case "-s":
			fmt.Println(os.Getenv("TORANA_TEST_UNAME_OS"))
		case "-m":
			fmt.Println(os.Getenv("TORANA_TEST_UNAME_ARCH"))
		default:
			os.Exit(2)
		}
	case "curl":
		if err := mockCurl(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(22)
		}
	case "sha256sum":
		// Prove that even a plausible digest cannot hide a hash command failure.
		data, err := os.ReadFile(os.Args[1])
		if err != nil {
			os.Exit(1)
		}
		fmt.Printf("%x  %s\n", sha256.Sum256(data), os.Args[1])
		os.Exit(1)
	default:
		return
	}
	os.Exit(0)
}

func mockCurl(args []string) error {
	options := map[string]string{}
	flags := map[string]bool{}
	var url string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--proto", "--proto-redir", "--output", "--write-out", "--retry", "--connect-timeout", "--max-time":
			if i+1 == len(args) {
				return fmt.Errorf("missing value: %s", args[i])
			}
			options[args[i]] = args[i+1]
			i++
		case "--tlsv1.2", "--fail", "--silent", "--show-error", "--location":
			flags[args[i]] = true
		default:
			if url != "" {
				return fmt.Errorf("unexpected curl argument: %s", args[i])
			}
			url = args[i]
		}
	}
	if options["--proto"] != "=https" || options["--proto-redir"] != "=https" ||
		!flags["--tlsv1.2"] || !flags["--fail"] || !flags["--location"] || options["--output"] == "" {
		return fmt.Errorf("missing required HTTPS/failure/output controls: %v", args)
	}
	log, err := os.OpenFile(os.Getenv("TORANA_TEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(log, url)
	closeErr := log.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if url == releaseURL+"/latest" {
		if os.Getenv("TORANA_TEST_MISSING") == "latest" {
			return fmt.Errorf("HTTP 404: no published release")
		}
		if options["--write-out"] != "%{url_effective}" {
			return fmt.Errorf("latest resolution must inspect the final URL")
		}
		fmt.Print(os.Getenv("TORANA_TEST_LATEST"))
		return nil
	}
	prefix := releaseURL + "/download/v" + os.Getenv("TORANA_TEST_VERSION") + "/"
	if !strings.HasPrefix(url, prefix) {
		return fmt.Errorf("unexpected release URL: %s", url)
	}
	asset := strings.TrimPrefix(url, prefix)
	if asset == os.Getenv("TORANA_TEST_MISSING") {
		return fmt.Errorf("HTTP 404: missing asset %s", asset)
	}
	if filepath.Base(asset) != asset {
		return fmt.Errorf("unexpected asset path: %s", asset)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("TORANA_TEST_ASSETS"), asset))
	if err != nil {
		return err
	}
	return os.WriteFile(options["--output"], data, 0o600)
}

type installer struct {
	t          *testing.T
	root       string
	assets     string
	temp       string
	home       string
	installDir string
	bin        string
	archive    string
	version    string
	env        map[string]string
	args       []string
}

func newInstaller(t *testing.T, goos, arch string) *installer {
	t.Helper()
	root := t.TempDir()
	f := &installer{
		t: t, root: root, assets: filepath.Join(root, "release assets"),
		temp: filepath.Join(root, "temporary files"), home: filepath.Join(root, "user home"),
		installDir: filepath.Join(root, "install path with spaces"), version: "1.2.3", bin: "torana",
		env: map[string]string{},
	}
	extension := "tar.gz"
	if goos == "windows" {
		extension, f.bin = "zip", "torana.exe"
	}
	f.archive = fmt.Sprintf("torana_%s_%s_%s.%s", f.version, goos, arch, extension)
	mockbin := filepath.Join(root, "mock bin")
	for _, dir := range []string{f.assets, f.temp, f.home, f.installDir, mockbin} {
		must(t, os.MkdirAll(dir, 0o700))
	}
	helper, err := os.Executable()
	must(t, err)
	helperBytes, err := os.ReadFile(helper)
	must(t, err)
	curl := "curl"
	if runtime.GOOS == "windows" {
		curl += ".exe"
	} else {
		must(t, os.WriteFile(filepath.Join(mockbin, "uname"), helperBytes, 0o700))
	}
	must(t, os.WriteFile(filepath.Join(mockbin, curl), helperBytes, 0o700))
	f.env = map[string]string{
		"PATH": mockbin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME": f.home, "LOCALAPPDATA": f.home, "TMPDIR": f.temp, "TEMP": f.temp, "TMP": f.temp,
		"TORANA_VERSION": "", "TORANA_INSTALL_DIR": f.installDir,
		"TORANA_TEST_HELPER": "1", "TORANA_TEST_ASSETS": f.assets,
		"TORANA_TEST_VERSION": f.version, "TORANA_TEST_LATEST": releaseURL + "/tag/v" + f.version,
		"TORANA_TEST_LOG": filepath.Join(root, "downloads.log"), "TORANA_TEST_MISSING": "",
		"TORANA_TEST_UNAME_OS":   map[string]string{"darwin": "Darwin", "linux": "Linux"}[goos],
		"TORANA_TEST_UNAME_ARCH": map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch],
		"PROCESSOR_ARCHITECTURE": strings.ToUpper(arch), "PROCESSOR_ARCHITEW6432": "",
	}
	if runtime.GOOS == "windows" {
		// The workflow starts Go from pwsh. Inheriting its module path into
		// Windows PowerShell makes 5.1 resolve incompatible PowerShell 7
		// modules (notably Get-FileHash). Let each child build its own path.
		// https://learn.microsoft.com/powershell/module/microsoft.powershell.core/about/about_psmodulepath
		f.env["PSModulePath"] = ""
	}
	f.release([]byte("synthetic release binary\n"), "regular")
	return f
}

func (f *installer) release(binary []byte, kind string) {
	f.t.Helper()
	var data bytes.Buffer
	if strings.HasSuffix(f.archive, ".zip") {
		writer := zip.NewWriter(&data)
		name := f.bin
		if kind == "missing" {
			name = "elsewhere/" + f.bin
		}
		file, err := writer.Create(name)
		must(f.t, err)
		_, err = file.Write(binary)
		must(f.t, err)
		if kind == "duplicate" {
			file, err = writer.Create(name)
			must(f.t, err)
			_, err = file.Write(binary)
			must(f.t, err)
		}
		outside, err := writer.Create("../escaped.txt")
		must(f.t, err)
		_, err = outside.Write([]byte("must never be extracted"))
		must(f.t, err)
		must(f.t, writer.Close())
	} else {
		compressed := gzip.NewWriter(&data)
		writer := tar.NewWriter(compressed)
		header := &tar.Header{Name: f.bin, Mode: 0o755, Size: int64(len(binary))}
		switch kind {
		case "missing":
			header.Name = "elsewhere/" + f.bin
		case "symlink":
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, "../outside", 0
		}
		must(f.t, writer.WriteHeader(header))
		if kind != "symlink" {
			_, err := writer.Write(binary)
			must(f.t, err)
		}
		if kind == "duplicate" {
			must(f.t, writer.WriteHeader(header))
			_, err := writer.Write(binary)
			must(f.t, err)
		}
		must(f.t, writer.WriteHeader(&tar.Header{Name: "../escaped.txt", Size: 6, Mode: 0o600}))
		_, err := writer.Write([]byte("unsafe"))
		must(f.t, err)
		must(f.t, writer.Close())
		must(f.t, compressed.Close())
	}
	archiveBytes := data.Bytes()
	if kind == "invalid" {
		archiveBytes = []byte("not an archive")
	}
	must(f.t, os.WriteFile(filepath.Join(f.assets, f.archive), archiveBytes, 0o600))
	f.checksum(fmt.Sprintf("%x  %s\n", sha256.Sum256(archiveBytes), f.archive))
}

func (f *installer) checksum(content string) {
	f.t.Helper()
	must(f.t, os.WriteFile(filepath.Join(f.assets, "checksums.txt"), []byte(content), 0o600))
}

func (f *installer) run() (string, error) {
	f.t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shell := os.Getenv("TORANA_TEST_POWERSHELL")
		if shell == "" {
			shell = "pwsh"
		}
		if _, err := exec.LookPath(shell); err != nil {
			f.t.Fatalf("native PowerShell interpreter is required: %v", err)
		}
		cmd = exec.Command(shell, append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "../install.ps1"}, f.args...)...)
	} else {
		cmd = exec.Command("/bin/sh", append([]string{"../install.sh"}, f.args...)...)
	}
	cmd.Env = overrideEnv(os.Environ(), f.env)
	output, err := cmd.CombinedOutput()
	f.assertCleanup()
	return string(output), err
}

func (f *installer) assertCleanup() {
	f.t.Helper()
	entries, err := os.ReadDir(f.temp)
	must(f.t, err)
	if len(entries) != 0 {
		f.t.Errorf("temporary download files were not cleaned up: %v", entries)
	}
	entries, err = os.ReadDir(f.installDir)
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".torana-install") {
				f.t.Errorf("temporary destination was not cleaned up: %s", entry.Name())
			}
		}
	}
	for _, path := range []string{filepath.Join(f.temp, "escaped.txt"), filepath.Join(f.root, "escaped.txt")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			f.t.Errorf("archive traversal was extracted: %s (%v)", path, err)
		}
	}
}

func (f *installer) wantSuccess() {
	f.t.Helper()
	output, err := f.run()
	if err != nil {
		f.t.Fatalf("installer failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Installed Torana "+f.version) || !strings.Contains(output, "PATH") {
		f.t.Fatalf("missing success/PATH guidance: %s", output)
	}
	data, err := os.ReadFile(filepath.Join(f.installDir, f.bin))
	must(f.t, err)
	if string(data) != "synthetic release binary\n" {
		f.t.Fatalf("installed unexpected bytes: %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(f.installDir, f.bin))
		must(f.t, err)
		if info.Mode().Perm() != 0o755 {
			f.t.Fatalf("binary is not executable: %v", info.Mode())
		}
	}
}

func nativeTarget() (string, string) { return runtime.GOOS, runtime.GOARCH }

func TestInstallerPlatforms(t *testing.T) {
	platforms := []string{"linux", "darwin"}
	if runtime.GOOS == "windows" {
		platforms = []string{"windows"}
	}
	for _, goos := range platforms {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(goos+"/"+arch, func(t *testing.T) {
				f := newInstaller(t, goos, arch)
				f.wantSuccess()
				log, err := os.ReadFile(f.env["TORANA_TEST_LOG"])
				must(t, err)
				want := releaseURL + "/latest\n" + releaseURL + "/download/v1.2.3/" + f.archive + "\n" + releaseURL + "/download/v1.2.3/checksums.txt\n"
				if string(log) != want {
					t.Fatalf("unexpected release requests:\n%s", log)
				}
			})
		}
	}
}

func TestInstallerPinnedVersionAndUpgrade(t *testing.T) {
	goos, arch := nativeTarget()
	for _, version := range []string{"v1.2.3", "1.2.3", "v1.2.3-rc.1", "1.2.3+build.1"} {
		t.Run(version, func(t *testing.T) {
			f := newInstaller(t, goos, arch)
			f.version = strings.TrimPrefix(version, "v")
			f.archive = strings.Replace(f.archive, "1.2.3", f.version, 1)
			f.env["TORANA_VERSION"] = version
			f.env["TORANA_TEST_VERSION"] = f.version
			f.release([]byte("synthetic release binary\n"), "regular")
			must(t, os.WriteFile(filepath.Join(f.installDir, f.bin), []byte("previous version"), 0o700))
			f.wantSuccess()
			log, err := os.ReadFile(f.env["TORANA_TEST_LOG"])
			must(t, err)
			if strings.Contains(string(log), "/latest") {
				t.Fatal("pinned install looked up latest")
			}
		})
	}
}

func TestInstallerDefaultsAndArguments(t *testing.T) {
	goos, arch := nativeTarget()
	t.Run("user-local default", func(t *testing.T) {
		f := newInstaller(t, goos, arch)
		f.env["TORANA_INSTALL_DIR"] = ""
		f.installDir = filepath.Join(f.home, ".local", "bin")
		if runtime.GOOS == "windows" {
			f.installDir = filepath.Join(f.home, "Torana", "bin")
		}
		profile := filepath.Join(f.home, ".profile")
		must(t, os.WriteFile(profile, []byte("untouched profile"), 0o600))
		f.wantSuccess()
		data, err := os.ReadFile(profile)
		must(t, err)
		if string(data) != "untouched profile" {
			t.Fatal("installer edited the shell profile")
		}
	})
	t.Run("arguments override environment", func(t *testing.T) {
		f := newInstaller(t, goos, arch)
		f.env["TORANA_VERSION"] = "invalid"
		f.env["TORANA_INSTALL_DIR"] = "relative"
		f.args = []string{"--version", "v1.2.3", "--install-dir", f.installDir}
		if runtime.GOOS == "windows" {
			f.args = []string{"-Version", "v1.2.3", "-InstallDir", f.installDir}
		}
		f.wantSuccess()
	})
}

func TestInstallerFailsClosed(t *testing.T) {
	goos, arch := nativeTarget()
	cases := []struct {
		name string
		edit func(*installer)
		want string
	}{
		{"no releases", func(f *installer) { f.env["TORANA_TEST_MISSING"] = "latest" }, "release"},
		{"missing archive", func(f *installer) { f.env["TORANA_TEST_MISSING"] = f.archive }, "release"},
		{"missing checksums", func(f *installer) { f.env["TORANA_TEST_MISSING"] = "checksums.txt" }, "release"},
		{"unofficial latest URL", func(f *installer) { f.env["TORANA_TEST_LATEST"] = "https://example.com/tag/v1.2.3" }, "official release tag"},
		{"invalid version", func(f *installer) { f.env["TORANA_VERSION"] = "../../evil" }, "ersion must"},
		{"relative directory", func(f *installer) { f.env["TORANA_INSTALL_DIR"] = "relative" }, "absolute"},
		{"unsupported architecture", func(f *installer) {
			f.env["TORANA_TEST_UNAME_ARCH"], f.env["PROCESSOR_ARCHITECTURE"] = "i686", "x86"
		}, "architectures"},
		{"wrong hash", func(f *installer) { f.checksum(strings.Repeat("0", 64) + "  " + f.archive + "\n") }, "checksum mismatch"},
		{"missing hash", func(f *installer) { f.checksum(strings.Repeat("0", 64) + "  other-archive.tar.gz\n") }, "exactly one valid SHA-256"},
		{"malformed hash", func(f *installer) { f.checksum("not-a-hash  " + f.archive + "\n") }, "exactly one valid SHA-256"},
		{"duplicate hash", func(f *installer) {
			data, err := os.ReadFile(filepath.Join(f.assets, "checksums.txt"))
			must(f.t, err)
			f.checksum(string(data) + string(data))
		}, "exactly one valid SHA-256"},
		{"malformed duplicate hash", func(f *installer) {
			data, err := os.ReadFile(filepath.Join(f.assets, "checksums.txt"))
			must(f.t, err)
			f.checksum(string(data) + "  malformed  " + f.archive + "\n")
		}, "exactly one valid SHA-256"},
		{"extra checksum field", func(f *installer) { f.checksum(strings.Repeat("0", 64) + "  " + f.archive + " extra\n") }, "exactly one valid SHA-256"},
		{"invalid archive", func(f *installer) { f.release(nil, "invalid") }, "archive"},
		{"missing binary", func(f *installer) { f.release([]byte("binary"), "missing") }, "archive must contain exactly one"},
		{"duplicate binary", func(f *installer) { f.release([]byte("binary"), "duplicate") }, "archive must contain exactly one"},
		{"empty binary", func(f *installer) { f.release(nil, "regular") }, "binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstaller(t, goos, arch)
			previous := []byte("previous version")
			must(t, os.WriteFile(filepath.Join(f.installDir, f.bin), previous, 0o700))
			tc.edit(f)
			output, err := f.run()
			if err == nil || !strings.Contains(strings.ToLower(output), strings.ToLower(tc.want)) {
				// Unix's message is plural; Windows includes the exact unsupported architecture.
				if tc.name != "unsupported architecture" || err == nil || !strings.Contains(output, "Unsupported architecture") {
					t.Fatalf("wanted failure containing %q, got %v:\n%s", tc.want, err, output)
				}
			}
			data, err := os.ReadFile(filepath.Join(f.installDir, f.bin))
			must(t, err)
			if !bytes.Equal(data, previous) {
				t.Fatal("failed install changed the previous executable")
			}
		})
	}
}

func TestInstallerRejectsUnsafeDestination(t *testing.T) {
	goos, arch := nativeTarget()
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "symlink" && runtime.GOOS == "windows" {
				t.Skip("Windows symlink creation requires a privilege not assumed by these tests")
			}
			f := newInstaller(t, goos, arch)
			destination := filepath.Join(f.installDir, f.bin)
			if kind == "directory" {
				must(t, os.Mkdir(destination, 0o700))
			} else {
				must(t, os.Symlink(filepath.Join(f.root, "not-present"), destination))
			}
			output, err := f.run()
			if err == nil || !strings.Contains(strings.ToLower(output), "refusing to replace") {
				t.Fatalf("unsafe destination accepted: %v\n%s", err, output)
			}
		})
	}
}

func TestInstallerUnixAliasesAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer test")
	}
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			f := newInstaller(t, "darwin", arch)
			f.env["TORANA_TEST_UNAME_ARCH"] = arch
			f.wantSuccess()
		})
	}
	t.Run("symlink binary", func(t *testing.T) {
		f := newInstaller(t, "linux", "amd64")
		f.release(nil, "symlink")
		output, err := f.run()
		if err == nil || !strings.Contains(output, "empty binary or link") {
			t.Fatalf("symlink archive accepted: %v\n%s", err, output)
		}
	})
	t.Run("unsupported OS", func(t *testing.T) {
		f := newInstaller(t, "linux", "amd64")
		f.env["TORANA_TEST_UNAME_OS"] = "FreeBSD"
		output, err := f.run()
		if err == nil || !strings.Contains(output, "supported operating systems") {
			t.Fatalf("unsupported OS accepted: %v\n%s", err, output)
		}
	})
}

func TestInstallerWindowsWOW64(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows architecture detection")
	}
	f := newInstaller(t, "windows", "arm64")
	f.env["PROCESSOR_ARCHITECTURE"], f.env["PROCESSOR_ARCHITEW6432"] = "x86", "ARM64"
	f.wantSuccess()
}

func TestInstallerUnixHashTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hashing dependencies")
	}
	for _, hash := range []string{"sha256sum", "shasum", "missing", "failed"} {
		t.Run(hash, func(t *testing.T) {
			f := newInstaller(t, runtime.GOOS, runtime.GOARCH)
			mockbin := filepath.Join(f.root, "mock bin")
			// Retain only the installer's other dependencies, so host utilities
			// cannot accidentally satisfy the hash dependency under test.
			for _, name := range []string{"mktemp", "tar", "awk", "grep", "mkdir", "cp", "chmod", "mv", "rm", "gzip"} {
				path, err := exec.LookPath(name)
				must(t, err)
				must(t, os.Symlink(path, filepath.Join(mockbin, name)))
			}
			f.env["PATH"] = mockbin
			switch hash {
			case "sha256sum", "shasum":
				path, err := exec.LookPath(hash)
				if err != nil {
					t.Skipf("%s unavailable on this host", hash)
				}
				must(t, os.Symlink(path, filepath.Join(mockbin, hash)))
				f.wantSuccess()
			case "failed":
				must(t, os.Symlink(filepath.Join(mockbin, "curl"), filepath.Join(mockbin, "sha256sum")))
				output, err := f.run()
				if err == nil || !strings.Contains(output, "SHA-256 calculation failed") {
					t.Fatalf("hash command failure ignored: %v\n%s", err, output)
				}
			case "missing":
				output, err := f.run()
				if err == nil || !strings.Contains(output, "SHA-256 verification requires") {
					t.Fatalf("missing hash tool accepted: %v\n%s", err, output)
				}
			}
		})
	}
}

func TestInstallerNativeBinary(t *testing.T) {
	if os.Getenv("TORANA_INSTALLER_SMOKE") != "1" {
		t.Skip("set TORANA_INSTALLER_SMOKE=1 to build, package, install and run the real native binary")
	}
	goos, arch := nativeTarget()
	f := newInstaller(t, goos, arch)
	compiled := filepath.Join(f.root, f.bin)
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-X main.version=1.2.3", "-o", compiled, "../../cmd/torana")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native binary: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(compiled)
	must(t, err)
	f.release(binary, "regular")
	if output, err := f.run(); err != nil {
		t.Fatalf("install native binary: %v\n%s", err, output)
	}
	output, err := exec.Command(filepath.Join(f.installDir, f.bin), "version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "1.2.3" {
		t.Fatalf("installed native binary version: %v\n%s", err, output)
	}
}

func overrideEnv(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		replaced := false
		for name := range overrides {
			if strings.EqualFold(key, name) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		// Windows PowerShell cannot represent an empty environment variable
		// consistently across versions. Empty fixture overrides mean unset.
		if runtime.GOOS == "windows" && value == "" {
			continue
		}
		result = append(result, key+"="+value)
	}
	return result
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
