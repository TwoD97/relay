//go:build linux

package runtime

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"
)

// Anthropic's documented release key, retrieved from the official fixed URL.
// https://code.claude.com/docs/en/setup#verify-the-manifest-signature
// The fingerprint is also checked before every detached signature verification.
//
//go:embed keys/claude-release.asc
var claudeReleaseKey []byte

const claudeReleaseFingerprint = "31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"
const claudeReleaseBase = "https://downloads.claude.ai/claude-code-releases/"
const maxNativeHarness = 512 << 20

type claudeManifest struct {
	Version   string `json:"version"`
	Platforms map[string]struct {
		Binary   string `json:"binary"`
		Checksum string `json:"checksum"`
		Size     int64  `json:"size"`
	} `json:"platforms"`
}

func verifyClaudeManifest(manifest, signature, key []byte, fingerprint string) error {
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil || len(keys) != 1 || strings.ToUpper(hex.EncodeToString(keys[0].PrimaryKey.Fingerprint[:])) != fingerprint {
		return errors.New("Claude release signing key fingerprint mismatch")
	}
	if bytes.HasPrefix(signature, []byte("-----BEGIN")) {
		block, decodeErr := armor.Decode(bytes.NewReader(signature))
		if decodeErr != nil || block.Type != "PGP SIGNATURE" {
			return errors.New("invalid Claude manifest signature")
		}
		signature, err = io.ReadAll(io.LimitReader(block.Body, 64<<10))
		if err != nil {
			return err
		}
	}
	// This legacy package is used only for this pinned RSA public key and a
	// SHA-2 detached signature; no encryption, key discovery, or key import.
	parsed, err := packet.Read(bytes.NewReader(signature))
	if err != nil {
		return errors.New("invalid Claude manifest signature")
	}
	sig, ok := parsed.(*packet.Signature)
	if !ok || (sig.Hash != crypto.SHA256 && sig.Hash != crypto.SHA384 && sig.Hash != crypto.SHA512) {
		return errors.New("Claude manifest requires a SHA-2 signature")
	}
	if _, err = openpgp.CheckDetachedSignature(keys, bytes.NewReader(manifest), bytes.NewReader(signature)); err != nil {
		return errors.New("Claude manifest signature verification failed; installation aborted")
	}
	return nil
}

func claudePlatform() (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
	if arch == "" {
		return "", errors.New("Claude native releases support Linux amd64 and arm64")
	}
	platform := "linux-" + arch
	if matches, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(matches) > 0 {
		platform += "-musl"
	}
	return platform, nil
}

func (s *Server) prepareClaude(ctx context.Context, client *http.Client, staging string, out io.Writer) (harnessRelease, error) {
	var latest bytes.Buffer
	if err := fetchHarness(ctx, client, claudeReleaseBase+"latest", 128, &latest); err != nil {
		return harnessRelease{}, err
	}
	version := strings.TrimSpace(latest.String())
	if !releaseVersion.MatchString(version) {
		return harnessRelease{}, errors.New("invalid Claude release version")
	}
	base := claudeReleaseBase + version + "/"
	var manifest, signature bytes.Buffer
	if err := fetchHarness(ctx, client, base+"manifest.json", 128<<10, &manifest); err != nil {
		return harnessRelease{}, err
	}
	if err := fetchHarness(ctx, client, base+"manifest.json.sig", 64<<10, &signature); err != nil {
		return harnessRelease{}, err
	}
	if err := verifyClaudeManifest(manifest.Bytes(), signature.Bytes(), claudeReleaseKey, claudeReleaseFingerprint); err != nil {
		return harnessRelease{}, err
	}
	var metadata claudeManifest
	if err := json.Unmarshal(manifest.Bytes(), &metadata); err != nil || metadata.Version != version {
		return harnessRelease{}, errors.New("Claude signed manifest has an unexpected version")
	}
	platform, err := claudePlatform()
	if err != nil {
		return harnessRelease{}, err
	}
	entry, ok := metadata.Platforms[platform]
	digest, err := hex.DecodeString(entry.Checksum)
	if !ok || entry.Binary != "claude" || len(digest) != sha256.Size || err != nil || entry.Size <= 0 || entry.Size > maxNativeHarness {
		return harnessRelease{}, errors.New("Claude signed manifest is missing a valid binary for this platform")
	}
	if err = privateDir(filepath.Join(staging, "native")); err != nil {
		return harnessRelease{}, err
	}
	binary := filepath.Join(staging, "native", "claude")
	f, err := os.OpenFile(binary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return harnessRelease{}, err
	}
	fmt.Fprintf(out, "Downloading Claude Code %s for %s. Anthropic's manifest signature is verified.\r\n", version, platform)
	hash := sha256.New()
	progress := &harnessProgress{out: out, total: entry.Size}
	err = fetchHarness(ctx, client, base+platform+"/claude", entry.Size, io.MultiWriter(f, hash, progress))
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return harnessRelease{}, err
	}
	if progress.downloaded != entry.Size || !equalDigest(hash.Sum(nil), digest) {
		return harnessRelease{}, errors.New("Claude binary checksum or size mismatch; current installation preserved")
	}
	if err = os.Chmod(binary, 0700); err != nil {
		return harnessRelease{}, err
	}
	if err = s.verifyHarnessVersion(ctx, binary, version); err != nil {
		return harnessRelease{}, err
	}
	return harnessRelease{Harness: "claude", Version: version, Strategy: "native", Source: base + platform + "/claude", Integrity: "sha256-" + entry.Checksum, CreatedAt: time.Now().UTC()}, nil
}

type harnessProgress struct {
	out                     io.Writer
	downloaded, total, last int64
}

func (p *harnessProgress) Write(data []byte) (int, error) {
	p.downloaded += int64(len(data))
	if p.downloaded-p.last >= 32<<20 || p.downloaded == p.total {
		fmt.Fprintf(p.out, "Downloaded %d of %d MiB.\r\n", p.downloaded>>20, p.total>>20)
		p.last = p.downloaded
	}
	return len(data), nil
}
