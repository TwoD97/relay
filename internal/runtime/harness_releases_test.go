//go:build linux

package runtime

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harnessFixtureTransport func(*http.Request) (*http.Response, error)

func (f harnessFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func codexRegistryFixture(t *testing.T, damaged bool, wait <-chan struct{}) {
	t.Helper()
	archive := []byte("fixture package; fake npm is used by this test")
	digest := sha512.Sum512(archive)
	metadata := map[string]any{"name": "@openai/codex", "version": "0.161.0", "dist": map[string]string{"tarball": "https://registry.npmjs.org/@openai/codex/-/codex-0.161.0.tgz", "integrity": "sha512-" + base64.StdEncoding.EncodeToString(digest[:])}}
	data, _ := json.Marshal(metadata)
	original := http.DefaultTransport
	http.DefaultTransport = harnessFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "registry.npmjs.org" {
			return nil, errors.New("unexpected network request")
		}
		body := data
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			if wait != nil {
				select {
				case <-wait:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			body = archive
			if damaged {
				body = []byte("tampered")
			}
		} else if r.URL.Path != "/@openai/codex/latest" {
			return nil, errors.New("unexpected registry path")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), ContentLength: int64(len(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
}

func fakeHarnessNPM(t *testing.T, s *Server, script string) string {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '11.0.0\\n'; exit 0; fi\n", 1)
	for name, body := range map[string]string{"node": "#!/bin/sh\nprintf 'v24.21.0\\n'\n", "npm": script} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

const successfulNPM = `#!/bin/sh
set -eu
[ "$1" = install ]
[ "$3" = --prefix ]
[ ! -e "$4/bin/codex" ]
/bin/mkdir -p "$4/bin"
printf '#!/bin/sh\nif [ "$1" = --version ]; then printf "codex-cli 0.161.0\\n"; else exit 1; fi\n' > "$4/bin/codex"
/bin/chmod 700 "$4/bin/codex"
`

func TestClaudeManifestUsesPinnedPublisherSignature(t *testing.T) {
	manifest, err := os.ReadFile("testdata/claude-2.1.294-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := os.ReadFile("testdata/claude-2.1.294-manifest.json.sig")
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyClaudeManifest(manifest, signature, claudeReleaseKey, claudeReleaseFingerprint); err != nil {
		t.Fatal(err)
	}
	if err = verifyClaudeManifest(append(manifest, ' '), signature, claudeReleaseKey, claudeReleaseFingerprint); err == nil {
		t.Fatal("accepted modified signed manifest")
	}
	if err = verifyClaudeManifest(manifest, []byte("invalid"), claudeReleaseKey, claudeReleaseFingerprint); err == nil {
		t.Fatal("accepted invalid signature")
	}
	if err = verifyClaudeManifest(manifest, signature, claudeReleaseKey, strings.Repeat("0", 40)); err == nil {
		t.Fatal("accepted different signing key fingerprint")
	}
}

func TestHarnessDownloadRejectsUntrustedSourcesAndOversizedResponses(t *testing.T) {
	for _, address := range []string{"http://downloads.claude.ai/file", "https://downloads.claude.ai.evil.test/file", "https://user@downloads.claude.ai/file", "https://registry.npmjs.org:444/file"} {
		if err := fetchHarness(context.Background(), harnessHTTPClient(), address, 10, io.Discard); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	client := harnessHTTPClient()
	first, _ := http.NewRequest(http.MethodGet, "https://downloads.claude.ai/file", nil)
	redirect, _ := http.NewRequest(http.MethodGet, "https://registry.npmjs.org/file", nil)
	if err := client.CheckRedirect(redirect, []*http.Request{first}); err == nil {
		t.Fatal("accepted cross-publisher redirect")
	}
	client.Transport = harnessFixtureTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("too much data")), Header: make(http.Header), ContentLength: -1, Request: r}, nil
	})
	if err := fetchHarness(context.Background(), client, first.URL.String(), 3, io.Discard); err == nil {
		t.Fatal("accepted unbounded chunked data")
	}
}

func TestHarnessMaintenancePreservesAuthenticationAndRunningSession(t *testing.T) {
	s := testServer(t)
	s.home = t.TempDir()
	codexRegistryFixture(t, false, nil)
	bin := fakeHarnessNPM(t, s, successfulNPM)
	external := filepath.Join(bin, "codex")
	old := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'codex-cli 0.150.0\\n'; else while read line; do printf 'old-session:%s\\n' \"$line\"; done; fi\n")
	if err := os.WriteFile(external, old, 0700); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(s.home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(auth), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte("fixture-auth-do-not-touch"), 0600); err != nil {
		t.Fatal(err)
	}
	meta, err := s.start(Session{Harness: "codex", Cwd: s.home, Title: "Existing session"}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) { return exec.Command(external), nil })
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.lookup(meta.ID)
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.pty != nil })
	p.mu.Lock()
	oldPID := p.cmd.Process.Pid
	p.mu.Unlock()
	for _, action := range []string{"update", "repair"} {
		if err = s.maintainHarness(context.Background(), "codex", action, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(external); !bytes.Equal(data, old) {
		t.Fatal("external installation changed")
	}
	if data, _ := os.ReadFile(auth); string(data) != "fixture-auth-do-not-touch" {
		t.Fatal("provider authentication changed")
	}
	p.mu.Lock()
	same := p.cmd.Process.Pid == oldPID && !p.finished
	p.mu.Unlock()
	if !same {
		t.Fatal("maintenance restarted existing session")
	}
	if err = p.writeInput("still-running\r"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(historyOf(p), "old-session:still-running") })
	path, err := s.findCommand("codex")
	if err != nil || path != filepath.Join(s.stateDir, "bin", "codex") {
		t.Fatalf("managed launcher not selected: %s %v", path, err)
	}
	h := s.inspectHarness(context.Background(), "codex")
	if h.Version != "codex-cli 0.161.0" || h.Management.Source != "relay" {
		t.Fatalf("unexpected managed discovery: %+v", h)
	}
	entries, _ := os.ReadDir(filepath.Join(s.stateDir, "tools", "harnesses", "codex", "releases"))
	if len(entries) != 2 {
		t.Fatalf("old release was removed: %d releases", len(entries))
	}
}

func TestInstallPreservesExistingHarnessWithoutDownloading(t *testing.T) {
	for _, source := range []string{"external", "managed"} {
		t.Run(source, func(t *testing.T) {
			s := testServer(t)
			s.home = t.TempDir()
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			if source == "managed" {
				bin = filepath.Join(s.stateDir, "tools", "harnesses", "codex", "bin")
				if err := privateDir(bin); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(bin, "codex")
			original := []byte("#!/bin/sh\nprintf 'codex-cli existing\\n'\n")
			if err := os.WriteFile(path, original, 0700); err != nil {
				t.Fatal(err)
			}
			oldTransport := http.DefaultTransport
			http.DefaultTransport = harnessFixtureTransport(func(*http.Request) (*http.Response, error) {
				t.Error("install downloaded over a healthy existing harness")
				return nil, errors.New("unexpected download")
			})
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			if err := s.maintainHarness(context.Background(), "codex", "install", io.Discard); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(path); !bytes.Equal(data, original) {
				t.Fatal("install changed the existing harness")
			}
		})
	}
}

func TestHarnessMaintenanceFailureAndCancellationKeepLauncher(t *testing.T) {
	for _, scenario := range []string{"checksum", "npm", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t)
			s.home = t.TempDir()
			var wait <-chan struct{}
			if scenario == "cancel" {
				wait = make(chan struct{})
			}
			codexRegistryFixture(t, scenario == "checksum", wait)
			script := successfulNPM
			if scenario == "npm" {
				script = "#!/bin/sh\nexit 23\n"
			}
			fakeHarnessNPM(t, s, script)
			bin := filepath.Join(s.stateDir, "bin")
			if err := privateDir(bin); err != nil {
				t.Fatal(err)
			}
			current := filepath.Join(bin, "codex")
			original := []byte("#!/bin/sh\nprintf 'codex-cli 0.150.0\\n'\n")
			if err := os.WriteFile(current, original, 0700); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if scenario == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			if err := s.maintainHarness(ctx, "codex", "update", io.Discard); err == nil {
				t.Fatal("failed maintenance reported success")
			}
			if data, _ := os.ReadFile(current); !bytes.Equal(data, original) {
				t.Fatal("failed maintenance replaced current launcher")
			}
			entries, _ := os.ReadDir(filepath.Join(s.stateDir, "tools", "harnesses", "codex", "releases"))
			if len(entries) != 0 {
				t.Fatal("failed preparation left partial releases")
			}
		})
	}
}

func TestStandaloneMaintenanceDoesNotReadRuntimeState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	s := &Server{stateDir: state, home: os.Getenv("HOME"), toolsMu: make(chan struct{}, 1)}
	fakeHarnessNPM(t, s, successfulNPM)
	codexRegistryFixture(t, false, nil)
	metadata := filepath.Join(state, "sessions.json")
	original := []byte("not runtime metadata: worker must not load or change this")
	if err := os.WriteFile(metadata, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := MaintainHarness(context.Background(), state, "codex", "update", io.Discard); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(metadata); !bytes.Equal(data, original) {
		t.Fatal("worker changed runtime state")
	}
	if err := MaintainHarness(context.Background(), state, "../bad", "update", io.Discard); err == nil {
		t.Fatal("accepted unknown harness")
	}
}

func TestHarnessMaintenanceLockAndEndpointContract(t *testing.T) {
	s := testServer(t)
	s.home = t.TempDir()
	fakeHarnessNPM(t, s, successfulNPM)
	gate := make(chan struct{})
	codexRegistryFixture(t, false, gate)
	var first Session
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/harnesses/codex/update", nil))
	if w.Code != 201 {
		t.Fatalf("update status %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil || first.Purpose != "update" {
		t.Fatalf("wrong session: %+v %v", first, err)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/harnesses/codex/repair", nil))
	if w.Code != 409 {
		t.Fatalf("concurrent maintenance status %d", w.Code)
	}
	lockPath := filepath.Join(s.stateDir, "tools", "harnesses", "codex", "maintenance.lock")
	eventually(t, func() bool { _, err := os.Stat(lockPath); return err == nil })
	if lock, err := lockHarness(lockPath); err == nil {
		lock.Close()
		close(gate)
		t.Fatal("second process acquired maintenance lock")
	}
	close(gate)
	p, _ := s.lookup(first.ID)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not finish")
	}
	if result := p.snapshot(); result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("maintenance failed: %s", historyOf(p))
	}
}

func TestNativeActivationDisablesOnlyManagedSelfUpdates(t *testing.T) {
	s := testServer(t)
	s.home = t.TempDir()
	root := filepath.Join(s.stateDir, "tools", "harnesses", "claude", "releases")
	for _, dir := range []string{root, filepath.Join(s.stateDir, "bin")} {
		if err := privateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	staging, err := os.MkdirTemp(root, ".prepare-")
	if err != nil {
		t.Fatal(err)
	}
	if err = privateDir(filepath.Join(staging, "native")); err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$DISABLE_UPDATES\" \"$DISABLE_AUTOUPDATER\" \"$HOME\" \"$1\"\n")
	if err = os.WriteFile(filepath.Join(staging, "native", "claude"), script, 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.activateHarnessRelease(context.Background(), "claude", staging, harnessRelease{Harness: "claude", Version: "2.1.294", Strategy: "native"}); err != nil {
		t.Fatal(err)
	}
	output, err := s.probe(context.Background(), filepath.Join(s.stateDir, "bin", "claude"), "literal argument")
	if err != nil || output != "1|1|"+os.Getenv("HOME")+"|literal argument\n" {
		t.Fatalf("managed launcher environment/arguments changed: %q %v", output, err)
	}
}

// Optional read-only upstream check: this downloads only signed metadata, never
// installs provider tools or reads provider authentication.
func TestLiveClaudeManifest(t *testing.T) {
	if os.Getenv("RELAY_LIVE_RELEASE_METADATA") != "1" {
		t.Skip("set RELAY_LIVE_RELEASE_METADATA=1 for upstream signed metadata check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := harnessHTTPClient()
	var latest, manifest, signature bytes.Buffer
	if err := fetchHarness(ctx, client, claudeReleaseBase+"latest", 128, &latest); err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(latest.String())
	if !releaseVersion.MatchString(version) {
		t.Fatal("invalid upstream version")
	}
	for suffix, dest := range map[string]*bytes.Buffer{"manifest.json": &manifest, "manifest.json.sig": &signature} {
		if err := fetchHarness(ctx, client, claudeReleaseBase+version+"/"+suffix, 128<<10, dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyClaudeManifest(manifest.Bytes(), signature.Bytes(), claudeReleaseKey, claudeReleaseFingerprint); err != nil {
		t.Fatal(err)
	}
}
