package plugincmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

const pluginRegistryURL = "https://torana.sh/registry/v1/index.json"
const maxReleaseBytes = 64 << 20

var releaseName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)
var releaseArchive = regexp.MustCompile(`^([a-z][a-z0-9_]*)-([0-9].*)\.tar\.gz$`)

type pluginRelease struct {
	name, version, url, id string
	digest                 string
}

func isReleaseSource(arg string) bool {
	if _, err := os.Stat(arg); err == nil {
		return false // Preserve existing local-source installation precedence.
	}
	name, _, _ := strings.Cut(arg, "@")
	return releaseName.MatchString(name) || strings.HasSuffix(arg, ".tar.gz")
}

// Downloads use Go's TLS verification, never a shell or external toolchain.
// Apply the same HTTPS rule to every redirect, not just the initial URL.
func releaseClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many release redirects")
		}
		return validateReleaseURL(req.URL)
	}}
}

func validateReleaseURL(u *url.URL) error {
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("plugin downloads require HTTPS URLs without credentials or fragments")
	}
	return nil
}

func downloadRelease(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, errors.New("invalid plugin download URL")
	}
	if err := validateReleaseURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download plugin release: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugin download returned HTTP %d; check that this release is published", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("plugin download exceeds size limit")
	}
	return data, nil
}

func resolveRelease(ctx context.Context, client *http.Client, registryURL, arg string) (pluginRelease, error) {
	if strings.HasSuffix(arg, ".tar.gz") {
		u, err := url.Parse(arg)
		if err != nil {
			return pluginRelease{}, errors.New("invalid plugin release URL")
		}
		if err := validateReleaseURL(u); err != nil {
			return pluginRelease{}, err
		}
		match := releaseArchive.FindStringSubmatch(path.Base(u.Path))
		if len(match) != 3 || !releaseVersion.MatchString(match[2]) || u.RawQuery != "" {
			return pluginRelease{}, errors.New("release URL must end with <plugin>-<version>.tar.gz, without a query")
		}
		return pluginRelease{name: match[1], version: match[2], url: arg}, nil
	}
	name, version, pinned := strings.Cut(arg, "@")
	if !releaseName.MatchString(name) || (pinned && !releaseVersion.MatchString(version)) {
		return pluginRelease{}, errors.New("use a plugin name, name@version, source path, or HTTPS release archive URL")
	}
	data, err := downloadRelease(ctx, client, registryURL, 2<<20)
	if err != nil {
		return pluginRelease{}, fmt.Errorf("read plugin registry: %w", err)
	}
	var index struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			ID            string            `json:"id"`
			Name          string            `json:"name"`
			Latest        string            `json:"latest"`
			BundleDigests map[string]string `json:"bundle_digests"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &index); err != nil || index.SchemaVersion != 1 {
		return pluginRelease{}, errors.New("invalid or unsupported plugin registry")
	}
	var result pluginRelease
	for _, entry := range index.Plugins {
		if entry.Name != name {
			continue
		}
		if result.name != "" || entry.ID != "torana/"+name || !releaseVersion.MatchString(entry.Latest) {
			return pluginRelease{}, errors.New("ambiguous or invalid plugin registry entry")
		}
		if !pinned {
			version = entry.Latest
		}
		digest := entry.BundleDigests[version]
		decoded, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
		if err != nil || len(decoded) != sha256.Size || !strings.HasPrefix(digest, "sha256:") {
			return pluginRelease{}, fmt.Errorf("plugin %s@%s has no published registry digest; use a source path or verified release URL", name, version)
		}
		result = pluginRelease{name: name, version: version, id: entry.ID,
			digest: "sha256:" + hex.EncodeToString(decoded),
			url:    fmt.Sprintf("https://github.com/torana-edge/torana-plugins/releases/download/%s/%s-%s.tar.gz", url.PathEscape(name+"/v"+version), name, version)}
	}
	if result.name == "" {
		return pluginRelease{}, fmt.Errorf("plugin %q is not in the registry; use its source path or release archive URL", name)
	}
	return result, nil
}

func archiveChecksum(data []byte, filename string) (string, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		return "", errors.New("release checksum must contain exactly one entry")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != filename {
		return "", errors.New("release checksum does not identify the exact archive")
	}
	hash, err := hex.DecodeString(fields[0])
	if err != nil || len(hash) != sha256.Size {
		return "", errors.New("invalid release SHA-256 checksum")
	}
	return strings.ToLower(fields[0]), nil
}

// Never extract archive paths. Only allow exact root regular files, and write
// the four runtime files ourselves. Everything else remains bounded metadata.
func unpackRelease(archive []byte, stage string) (string, error) {
	raw := bytes.NewReader(archive)
	gz, err := gzip.NewReader(raw)
	if err != nil {
		return "", fmt.Errorf("read plugin archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	limited := &io.LimitedReader{R: gz, N: 96<<20 + 1}
	tr := tar.NewReader(limited)
	files := map[string][]byte{}
	allowed := map[string]bool{"plugin.json": true, "plugin.wasm": true, "schema.json": true, "agent.json": true,
		"LICENSE": true, "README.md": true, "SHA256SUMS": true, "BUNDLE_DIGEST": true}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read plugin archive: %w", err)
		}
		limit := int64(2 << 20)
		if h.Name == "plugin.wasm" {
			limit = maxReleaseBytes
		}
		if !allowed[h.Name] || h.Typeflag != tar.TypeReg || h.Linkname != "" || len(h.PAXRecords) != 0 || h.Size < 0 || h.Size > limit {
			return "", errors.New("plugin archive contains an unexpected, linked, or oversized entry")
		}
		if _, exists := files[h.Name]; exists {
			return "", errors.New("plugin archive contains a duplicate entry")
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return "", err
		}
		files[h.Name] = data
	}
	// Consume padding to EOF to check the gzip trailer/CRC as well. Concatenated
	// gzip streams or nonzero data after the tar terminator are not bundle files.
	tail, err := io.ReadAll(limited)
	if err != nil || limited.N == 0 || raw.Len() != 0 || len(bytes.Trim(tail, "\x00")) != 0 {
		return "", errors.New("plugin archive is truncated, oversized, or has trailing data")
	}
	for _, name := range []string{"plugin.json", "plugin.wasm", "schema.json", "BUNDLE_DIGEST", "SHA256SUMS"} {
		if len(files[name]) == 0 {
			return "", fmt.Errorf("plugin archive is missing %s", name)
		}
	}
	if !bytes.HasPrefix(files["plugin.wasm"], []byte{0, 'a', 's', 'm', 1, 0, 0, 0}) {
		return "", errors.New("plugin archive has an invalid WASM header")
	}
	checks := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", errors.New("invalid bundle file checksum")
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != "plugin.json" && name != "plugin.wasm" && name != "schema.json" && name != "agent.json" {
			return "", errors.New("unexpected bundle file checksum")
		}
		data, exists := files[name]
		if !exists || checks[name] || !strings.EqualFold(fields[0], fmt.Sprintf("%x", sha256.Sum256(data))) {
			return "", errors.New("bundle file checksum mismatch or duplicate")
		}
		checks[name] = true
	}
	for _, name := range bundleFiles {
		data, exists := files[name]
		if !exists {
			continue
		}
		if !checks[name] {
			return "", fmt.Errorf("bundle checksum missing for %s", name)
		}
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o644); err != nil {
			return "", err
		}
	}
	digest, err := plugin.BundleDigestForDir(stage)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(files["BUNDLE_DIGEST"])) != digest {
		return "", errors.New("published bundle digest does not match the plugin")
	}
	return digest, nil
}

func installRelease(ctx context.Context, client *http.Client, release pluginRelease, dest string, stdout io.Writer) error {
	checksum, err := downloadRelease(ctx, client, release.url+".sha256", 4096)
	if err != nil {
		return err
	}
	u, _ := url.Parse(release.url)
	expected, err := archiveChecksum(checksum, path.Base(u.Path))
	if err != nil {
		return err
	}
	archive, err := downloadRelease(ctx, client, release.url, maxReleaseBytes)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(archive)) != expected {
		return errors.New("plugin archive SHA-256 mismatch; nothing installed")
	}
	stage, err := os.MkdirTemp(dest, "."+release.name+".install-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	digest, err := unpackRelease(archive, stage)
	if err != nil {
		return err
	}
	if release.digest != "" && digest != release.digest {
		return errors.New("prebuilt bundle digest does not match the independently published registry value; nothing installed")
	}
	bundle, err := plugin.ValidateBundleDir(stage)
	if err != nil {
		return fmt.Errorf("validate prebuilt plugin: %w", err)
	}
	if bundle.Manifest.Name != release.name || bundle.Manifest.Version != release.version || (release.id != "" && bundle.Manifest.ID != release.id) {
		return errors.New("prebuilt plugin identity/version does not match the selected release")
	}
	if err := activateBundle(stage, filepath.Join(dest, release.name)); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Installed %s v%s (prebuilt) -> %s\n  digest %s\n", release.name, release.version, filepath.Join(dest, release.name), digest)
	return nil
}
