package transport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type health struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$`)

// Bootstrap installs only a checksum-verified local artifact, as the remote
// user. It changes only Relay-managed files and never requires sudo.
func (c *Connection) Bootstrap(ctx context.Context, binaryDir, version string, onStage func(string)) error {
	_, err := c.installRuntime(ctx, binaryDir, version, false, onStage)
	return err
}

type RepairResult struct {
	InstalledVersion string `json:"installedVersion"`
	RunningVersion   string `json:"runningVersion"`
	RestartRequired  bool   `json:"restartRequired"`
}

// Repair updates verified runtime files and the bridge without replacing a live
// daemon. A different running version remains active until its next normal start.
func (c *Connection) Repair(ctx context.Context, binaryDir, version string, onStage func(string)) (RepairResult, error) {
	return c.installRuntime(ctx, binaryDir, version, true, onStage)
}

// RefreshCLI keeps newly registered agent instructions usable even while an
// older compatible daemon continues owning its sessions. A matching bridge
// executable needs no upload; updating it never restarts a healthy daemon.
func (c *Connection) RefreshCLI(ctx context.Context, binaryDir, version string) error {
	if !versionPattern.MatchString(version) {
		return errors.New("invalid build version")
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	output, err := c.Run(probe, `exec "$HOME/.local/share/relay/bin/relay" version`, nil)
	cancel()
	if err == nil && strings.TrimSpace(string(output)) == version {
		return nil
	}
	_, err = c.Repair(ctx, binaryDir, version, nil)
	return err
}

func (c *Connection) installRuntime(ctx context.Context, binaryDir, version string, repair bool, onStage func(string)) (RepairResult, error) {
	if !versionPattern.MatchString(version) {
		return RepairResult{}, errors.New("invalid build version")
	}
	stage := func(message string) {
		if onStage != nil {
			onStage(message)
		}
	}
	stage("Checking the remote Linux host")
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	output, err := c.Run(probeCtx, "uname -s; uname -m", nil)
	cancel()
	if err != nil {
		return RepairResult{}, fmt.Errorf("detect remote platform: %w", err)
	}
	platform := strings.Fields(string(output))
	if len(platform) != 2 || platform[0] != "Linux" {
		return RepairResult{}, errors.New("remote hosts must run Linux (amd64 or arm64)")
	}
	arch := ""
	switch platform[1] {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return RepairResult{}, fmt.Errorf("unsupported Linux architecture %q", platform[1])
	}
	stage("Checking for an existing Relay runtime")
	if existing, err := c.remoteHealth(ctx); err == nil {
		if err := checkHealth(existing, version); err != nil {
			return RepairResult{}, err
		}
		if !repair {
			stage("Connected to the existing Relay runtime")
			return RepairResult{RunningVersion: existing.Version}, nil
		}
	}
	stage("Verifying the runtime release")
	artifact, digest, err := verifiedArtifact(binaryDir, "relay-linux-"+arch)
	if err != nil {
		return RepairResult{}, err
	}
	defer artifact.Close()
	release := version + "/" + digest
	stage("Installing the Relay runtime")
	if _, err := c.Run(ctx, uploadScript(release, digest), artifact); err != nil {
		return RepairResult{}, fmt.Errorf("upload runtime: %w", err)
	}
	// Ensure is intentionally invoked before changing the public bridge symlink.
	// The executable refuses incompatible live daemons instead of replacing them.
	stage("Starting the private Relay runtime")
	ensureScript := `exec "$HOME/.local/share/relay/releases/` + release + `/relay" ensure`
	output, err = c.Run(ctx, ensureScript, nil)
	if err != nil {
		return RepairResult{}, fmt.Errorf("start runtime: %w", err)
	}
	var started health
	if err := json.Unmarshal(output, &started); err != nil {
		return RepairResult{}, fmt.Errorf("runtime returned invalid health information: %w", err)
	}
	if err := checkHealth(started, version); err != nil {
		return RepairResult{}, err
	}
	if _, err := c.Run(ctx, activateScript(release), nil); err != nil {
		return RepairResult{}, fmt.Errorf("activate runtime bridge: %w", err)
	}
	stage("Verifying the encrypted runtime connection")
	connected, err := c.remoteHealth(ctx)
	if err != nil {
		return RepairResult{}, fmt.Errorf("verify runtime connection: %w", err)
	}
	if err := checkHealth(connected, version); err != nil {
		return RepairResult{}, err
	}
	stage("Relay is ready")
	return RepairResult{InstalledVersion: version, RunningVersion: connected.Version, RestartRequired: connected.Version != version}, nil
}

func checkHealth(h health, version string) error {
	// Runtime protocol 1 is a compatibility contract, independent of the client
	// build (which also changes for desktop and browser-only updates).
	if h.Protocol != 1 || !versionPattern.MatchString(h.Version) {
		return fmt.Errorf("the running Relay runtime is incompatible (version %q, protocol %d); client %q requires runtime protocol 1", h.Version, h.Protocol, version)
	}
	return nil
}

func (c *Connection) remoteHealth(ctx context.Context) (health, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: c.DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: 8 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://relay/api/health", nil)
	if err != nil {
		return health{}, err
	}
	response, err := client.Do(req)
	if err != nil {
		return health{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return health{}, fmt.Errorf("runtime health returned HTTP %d", response.StatusCode)
	}
	var value health
	err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&value)
	return value, err
}

func verifiedArtifact(directory, filename string) (*os.File, string, error) {
	manifest, err := os.Open(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return nil, "", fmt.Errorf("open release checksums: %w; build the release bundle first", err)
	}
	defer manifest.Close()
	scanner := bufio.NewScanner(io.LimitReader(manifest, 1<<20))
	expected := ""
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != filename {
			continue
		}
		if expected != "" {
			return nil, "", fmt.Errorf("duplicate checksum for %s", filename)
		}
		if decoded, err := hex.DecodeString(fields[0]); err != nil || len(decoded) != sha256.Size {
			return nil, "", fmt.Errorf("invalid checksum for %s", filename)
		}
		expected = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("read release checksums: %w", err)
	}
	if expected == "" {
		return nil, "", fmt.Errorf("release checksums do not include %s", filename)
	}
	artifact, err := os.Open(filepath.Join(directory, filename))
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", filename, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = artifact.Close()
		}
	}()
	info, err := artifact.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<20 {
		return nil, "", errors.New("runtime artifact must be a non-empty regular file under 256 MiB")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, artifact); err != nil {
		return nil, "", fmt.Errorf("hash runtime artifact: %w", err)
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != expected {
		return nil, "", fmt.Errorf("checksum mismatch for %s", filename)
	}
	if _, err := artifact.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	ok = true
	return artifact, actual, nil
}

func uploadScript(release, digest string) string {
	return `set -eu
umask 077
base="$HOME/.local/share/relay"
release="$base/releases/` + release + `"
mkdir -p "$release" "$base/bin"
chmod 700 "$base" "$base/releases" "$base/bin" "$release"
command -v sha256sum >/dev/null 2>&1 || { echo 'sha256sum is required on the remote host' >&2; exit 1; }
temporary=$(mktemp "$release/.upload.XXXXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
printf '%s  %s\n' '` + digest + `' "$temporary" | sha256sum -c - >/dev/null
chmod 700 "$temporary"
mv -fT "$temporary" "$release/relay"
trap - EXIT HUP INT TERM
`
}

func activateScript(release string) string {
	return `set -eu
umask 077
base="$HOME/.local/share/relay"
temporary=$(mktemp -d "$base/bin/.activate.XXXXXXXX")
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
ln -s "$base/releases/` + release + `/relay" "$temporary/relay"
mv -fT "$temporary/relay" "$base/bin/relay"
rmdir "$temporary"
trap - EXIT HUP INT TERM
`
}
