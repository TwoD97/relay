package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var coordinatorIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (c *Connection) DialCoordinatorContext(ctx context.Context, id string) (net.Conn, error) {
	if !coordinatorIDPattern.MatchString(id) {
		return nil, errors.New("Invalid coordinator identity")
	}
	return c.dialCommand(ctx, `exec "$HOME/.local/share/relay/bin/relay" coordinator bridge --id `+id)
}

// StageCoordinatorBundle publishes both architectures under a content-addressed
// private directory. It never starts or replaces a daemon, or changes the
// runtime's active executable. Prepare is a separate explicit lifecycle action.
func (c *Connection) StageCoordinatorBundle(ctx context.Context, directory, version string) (string, error) {
	if !versionPattern.MatchString(version) {
		return "", errors.New("Invalid coordinator build version")
	}
	f, err := os.Open(filepath.Join(directory, "VERSION"))
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	_ = f.Close()
	if err != nil || len(data) > 1024 || strings.TrimSpace(string(data)) != version {
		return "", errors.New("Coordinator bundle version does not match this controller")
	}
	type artifact struct {
		name, digest string
		file         *os.File
	}
	artifacts := []artifact{}
	defer func() {
		for _, a := range artifacts {
			_ = a.file.Close()
		}
	}()
	for _, name := range []string{"relay-linux-amd64", "relay-linux-arm64"} {
		file, digest, err := verifiedArtifact(directory, name)
		if err != nil {
			return "", err
		}
		artifacts = append(artifacts, artifact{name, digest, file})
	}
	optionalManifest := ""
	if file, readErr := os.Open(filepath.Join(directory, "SHA256SUMS.whatsapp")); readErr == nil {
		raw, readErr := io.ReadAll(io.LimitReader(file, 1025))
		_ = file.Close()
		if readErr != nil {
			return "", readErr
		}
		allowed := map[string]bool{"relay-whatsapp-personal-linux-amd64": true, "relay-whatsapp-personal-linux-arm64": true, "relay-whatsapp-personal-windows-amd64.exe": true}
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		seen := map[string]bool{}
		if len(raw) > 1024 {
			return "", errors.New("Optional connector manifest exceeds its limit")
		}
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) != 2 || !allowed[fields[1]] || seen[fields[1]] {
				return "", errors.New("Invalid optional connector manifest")
			}
			seen[fields[1]] = true
		}
		if scanner.Err() != nil || len(seen) != 3 {
			return "", errors.New("Optional connector manifest must contain all three architectures")
		}
		for _, name := range []string{"relay-whatsapp-personal-linux-amd64", "relay-whatsapp-personal-linux-arm64", "relay-whatsapp-personal-windows-amd64.exe"} {
			file, digest, err := verifiedArtifactManifest(directory, name, "SHA256SUMS.whatsapp")
			if err != nil {
				return "", err
			}
			artifacts = append(artifacts, artifact{name, digest, file})
			optionalManifest += digest + "  " + name + "\n"
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, version)
	for _, a := range artifacts {
		_, _ = io.WriteString(hash, "\x00"+a.name+"\x00"+a.digest)
	}
	id := hex.EncodeToString(hash.Sum(nil))
	for _, a := range artifacts {
		if _, err = c.Run(ctx, coordinatorUploadScript(id, a.name, a.digest), a.file); err != nil {
			return "", fmt.Errorf("Stage verified coordinator bundle: %w", err)
		}
	}
	output, err := c.Run(ctx, coordinatorManifestScript(id, version, artifacts[0].digest, artifacts[1].digest, optionalManifest), nil)
	if err != nil {
		return "", fmt.Errorf("Verify coordinator bundle: %w", err)
	}
	path := strings.TrimSpace(string(output))
	if len(path) > 4096 || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n") || !strings.HasSuffix(path, "/coordinator-bundles/"+id) {
		return "", errors.New("Remote coordinator bundle returned an invalid path")
	}
	return path, nil
}

func coordinatorDirectoryScript(id string) string {
	return `set -eu
umask 077
base="$HOME/.local/share/relay"
for directory in "$base" "$base/coordinator-bundles" "$base/coordinator-bundles/` + id + `"; do
  if [ ! -e "$directory" ] && [ ! -L "$directory" ]; then mkdir -p "$directory"; fi
  [ -d "$directory" ] && [ ! -L "$directory" ] && [ "$(stat -c %u "$directory")" = "$(id -u)" ] || { echo 'Coordinator bundle directory ownership is invalid' >&2; exit 1; }
  [ "$(stat -c %a "$directory")" = 700 ] || { echo 'Coordinator bundle directory must be private' >&2; exit 1; }
done
bundle="$base/coordinator-bundles/` + id + `"
command -v sha256sum >/dev/null 2>&1
`
}
func coordinatorUploadScript(id, name, digest string) string {
	return coordinatorDirectoryScript(id) + `
temporary=$(mktemp "$bundle/.upload.XXXXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
printf '%s  %s\n' '` + digest + `' "$temporary" | sha256sum -c - >/dev/null
chmod 700 "$temporary"
target="$bundle/` + name + `"
if [ ! -e "$target" ] && [ ! -L "$target" ]; then ln -T "$temporary" "$target" 2>/dev/null || true; fi
[ -f "$target" ] && [ ! -L "$target" ] && [ "$(stat -c %u "$target")" = "$(id -u)" ] && [ "$(stat -c %a "$target")" = 700 ] || { echo 'Coordinator artifact ownership is invalid' >&2; exit 1; }
printf '%s  %s\n' '` + digest + `' "$target" | sha256sum -c - >/dev/null
`
}
func coordinatorManifestScript(id, version, amd64, arm64, optional string) string {
	optionalPublish := ""
	if optional != "" {
		optionalPublish = "printf '%s' " + quoteShell(optional) + " | publish SHA256SUMS.whatsapp\n"
	}
	return coordinatorDirectoryScript(id) + `
publish() {
  name="$1"
  temporary=$(mktemp "$bundle/.metadata.XXXXXXXX")
  trap 'rm -f "$temporary"' EXIT HUP INT TERM
  cat > "$temporary"
  target="$bundle/$name"
  if [ ! -e "$target" ] && [ ! -L "$target" ]; then ln -T "$temporary" "$target" 2>/dev/null || true; fi
  [ -f "$target" ] && [ ! -L "$target" ] && [ "$(stat -c %u "$target")" = "$(id -u)" ] && [ "$(stat -c %a "$target")" = 600 ] && cmp -s "$temporary" "$target" || { echo 'Coordinator manifest changed' >&2; exit 1; }
  rm -f "$temporary"
  trap - EXIT HUP INT TERM
}
printf '%s\n' '` + version + `' | publish VERSION
printf '%s  %s\n' '` + amd64 + `' relay-linux-amd64 '` + arm64 + `' relay-linux-arm64 | publish SHA256SUMS
` + optionalPublish + `cd "$bundle"
sha256sum -c SHA256SUMS >/dev/null
if [ -e SHA256SUMS.whatsapp ]; then sha256sum -c SHA256SUMS.whatsapp >/dev/null; fi
printf '%s\n' "$bundle"
`
}
