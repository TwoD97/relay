//go:build linux

package runtime

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type HarnessManagement struct {
	SupportedActions []string `json:"supportedActions"`
	Source           string   `json:"source"`
	Strategy         string   `json:"strategy"`
	Detail           string   `json:"detail"`
}

type harnessRelease struct {
	Harness   string    `json:"harness"`
	Version   string    `json:"version"`
	Strategy  string    `json:"strategy"`
	Source    string    `json:"source"`
	Integrity string    `json:"integrity"`
	CreatedAt time.Time `json:"createdAt"`
}

var releaseVersion = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}$`)

func maintenancePurpose(purpose string) bool {
	return purpose == "install" || purpose == "update" || purpose == "repair"
}

// MaintainHarness operates only on tool releases. It deliberately does not call
// New, open runtime state, or touch sessions/authentication. New CLI workers can
// therefore maintain tools used by an older, compatible, still-running daemon.
func MaintainHarness(ctx context.Context, stateDir, id, action string, out io.Writer) error {
	if _, ok := harnessNames[id]; !ok || !maintenancePurpose(action) {
		return errors.New("harness maintenance requires claude or codex and install, update, or repair")
	}
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	s := &Server{stateDir: dir, home: home, toolsMu: make(chan struct{}, 1)}
	return s.maintainHarness(ctx, id, action, out)
}

func (s *Server) maintainHarness(ctx context.Context, id, action string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if out == nil {
		out = io.Discard
	}
	for _, dir := range []string{s.stateDir, filepath.Join(s.stateDir, "tools"), filepath.Join(s.stateDir, "tools", "harnesses"), filepath.Join(s.stateDir, "tools", "harnesses", id), filepath.Join(s.stateDir, "tools", "harnesses", id, "releases"), filepath.Join(s.stateDir, "bin")} {
		if err := privateDir(dir); err != nil {
			return err
		}
	}
	lock, err := lockHarness(filepath.Join(s.stateDir, "tools", "harnesses", id, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = ctx.Err(); err != nil {
		return err
	}
	if action == "install" {
		if existing, findErr := s.findCommand(id); findErr == nil {
			_, probeErr := s.probe(ctx, existing, "--version")
			if err := ctx.Err(); err != nil {
				return err
			}
			if probeErr == nil || !s.managedHarnessPath(id, existing) {
				fmt.Fprintf(out, "%s is already installed at %s. Existing installation preserved. Use Update or Repair to create a managed replacement.\r\n", harnessNames[id], existing)
				return nil
			}
		}
	}
	fmt.Fprintf(out, "Preparing %s %s. Authentication and running sessions are preserved.\r\n", harnessNames[id], action)
	releases := filepath.Join(s.stateDir, "tools", "harnesses", id, "releases")
	staging, err := os.MkdirTemp(releases, ".preparing-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	client := harnessHTTPClient()
	var release harnessRelease
	if id == "claude" {
		release, err = s.prepareClaude(ctx, client, staging, out)
	} else {
		release, err = s.prepareCodex(ctx, client, staging, out)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = s.activateHarnessRelease(ctx, id, staging, release); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s is ready. New sessions use this release; existing sessions keep their original files.\r\n", harnessNames[id], release.Version)
	return nil
}

func lockHarness(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open harness maintenance lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("harness maintenance lock is not a regular file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("harness maintenance is already running; wait for it to finish")
	}
	return f, nil
}

func harnessHTTPClient() *http.Client {
	return &http.Client{Timeout: 8 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || !trustedHarnessURL(req.URL) || (len(via) > 0 && req.URL.Host != via[0].URL.Host) {
			return errors.New("untrusted harness download redirect")
		}
		return nil
	}}
}
func trustedHarnessURL(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Host == "downloads.claude.ai" || u.Host == "registry.npmjs.org")
}
func fetchHarness(ctx context.Context, client *http.Client, address string, maximum int64, dest io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !trustedHarnessURL(req.URL) {
		return errors.New("untrusted harness download URL")
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download harness release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download harness release: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maximum {
		return errors.New("harness download exceeds size limit")
	}
	n, err := io.Copy(dest, io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return err
	}
	if n > maximum {
		return errors.New("harness download exceeds size limit")
	}
	return nil
}

func (s *Server) prepareCodex(ctx context.Context, client *http.Client, staging string, out io.Writer) (harnessRelease, error) {
	var metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	var data strings.Builder
	if err := fetchHarness(ctx, client, "https://registry.npmjs.org/@openai/codex/latest", 128<<10, &data); err != nil {
		return harnessRelease{}, err
	}
	if err := json.Unmarshal([]byte(data.String()), &metadata); err != nil {
		return harnessRelease{}, errors.New("invalid Codex release metadata")
	}
	expectedURL := "https://registry.npmjs.org/@openai/codex/-/codex-" + metadata.Version + ".tgz"
	expectedDigest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(metadata.Dist.Integrity, "sha512-"))
	if metadata.Name != "@openai/codex" || !releaseVersion.MatchString(metadata.Version) || metadata.Dist.Tarball != expectedURL || !strings.HasPrefix(metadata.Dist.Integrity, "sha512-") || err != nil || len(expectedDigest) != sha512.Size {
		return harnessRelease{}, errors.New("Codex metadata failed package, version, or integrity validation")
	}
	fmt.Fprintf(out, "Downloading official @openai/codex@%s (verified SHA-512).\r\n", metadata.Version)
	archive := filepath.Join(staging, "codex.tgz")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return harnessRelease{}, err
	}
	hash := sha512.New()
	err = fetchHarness(ctx, client, expectedURL, 16<<20, io.MultiWriter(f, hash))
	err = errors.Join(err, f.Close())
	if err != nil {
		return harnessRelease{}, err
	}
	if !equalDigest(hash.Sum(nil), expectedDigest) {
		return harnessRelease{}, errors.New("Codex package checksum mismatch; current installation preserved")
	}
	npm, err := s.ensureNPM(ctx, out)
	if err != nil {
		return harnessRelease{}, err
	}
	cache := filepath.Join(s.stateDir, "tools", "npm-cache")
	if err = privateDir(cache); err != nil {
		return harnessRelease{}, err
	}
	cmd := exec.CommandContext(ctx, npm, "install", "--global", "--prefix", staging, "--registry=https://registry.npmjs.org", "--ignore-scripts", "--include=optional", "--no-audit", "--no-fund", "--cache", cache, archive)
	cmd.Env = s.environment()
	cmd.Dir = staging
	if err = runMaintenanceCommand(ctx, cmd, out); err != nil {
		return harnessRelease{}, fmt.Errorf("Codex installation failed: %w", err)
	}
	if err = s.verifyHarnessVersion(ctx, filepath.Join(staging, "bin", "codex"), metadata.Version); err != nil {
		return harnessRelease{}, err
	}
	if err = os.Remove(archive); err != nil {
		return harnessRelease{}, err
	}
	return harnessRelease{Harness: "codex", Version: metadata.Version, Strategy: "npm", Source: expectedURL, Integrity: metadata.Dist.Integrity, CreatedAt: time.Now().UTC()}, nil
}

func equalDigest(actual, expected []byte) bool {
	if len(actual) != len(expected) {
		return false
	}
	var diff byte
	for i := range actual {
		diff |= actual[i] ^ expected[i]
	}
	return diff == 0
}

func runMaintenanceCommand(ctx context.Context, cmd *exec.Cmd, out io.Writer) error {
	cmd.Stdout = out
	cmd.Stderr = out
	err := runIsolatedCommand(ctx, cmd)
	if cancelErr := ctx.Err(); cancelErr != nil && !errors.Is(err, cancelErr) {
		err = errors.Join(cancelErr, err)
	}
	return err
}

func (s *Server) verifyHarnessVersion(ctx context.Context, path, version string) error {
	output, err := s.probe(ctx, path, "--version")
	if err != nil {
		return fmt.Errorf("downloaded harness could not run: %w", err)
	}
	for _, field := range strings.Fields(output) {
		if field == version {
			return nil
		}
	}
	return errors.New("downloaded harness did not report the expected version; current installation preserved")
}

func (s *Server) activateHarnessRelease(ctx context.Context, id, staging string, release harnessRelease) error {
	if release.Harness != id || !releaseVersion.MatchString(release.Version) {
		return errors.New("invalid prepared harness release")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	suffix, err := newID()
	if err != nil {
		return err
	}
	destination := filepath.Join(s.stateDir, "tools", "harnesses", id, "releases", release.Version+"-"+suffix)
	if id == "claude" {
		if err = privateDir(filepath.Join(staging, "bin")); err != nil {
			return err
		}
		// Keep self-updates out of immutable Relay releases. HOME and every
		// provider authentication/configuration variable remain unchanged.
		launcher := "#!/bin/sh\nexport DISABLE_AUTOUPDATER=1\nexport DISABLE_UPDATES=1\nexec " + shellQuote(filepath.Join(destination, "native", "claude")) + " \"$@\"\n"
		if err = os.WriteFile(filepath.Join(staging, "bin", id), []byte(launcher), 0700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return err
	}
	if err = atomicPrivateWrite(filepath.Join(staging, "release.json"), data); err != nil {
		return err
	}
	if err = os.Rename(staging, destination); err != nil {
		return err
	}
	// Never remove a published release: running processes, including sessions
	// owned by older daemons or another machine, may still read files from it.
	if err = ctx.Err(); err != nil {
		return err
	}
	bin := filepath.Join(s.stateDir, "bin")
	link := filepath.Join(bin, "."+id+"-"+suffix)
	if err = os.Symlink(filepath.Join(destination, "bin", id), link); err != nil {
		return err
	}
	defer os.Remove(link)
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(link, filepath.Join(bin, id)); err != nil {
		return err
	}
	dir, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
