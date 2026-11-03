package localagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHarnessLiteralArgumentsAndScopedEnvironment(t *testing.T) {
	prompt := "User project ; $(literal) & \"quoted\" 日本語"
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []string{"manual", "auto"} {
			args := providerArguments(provider, mode, prompt)
			if !strings.HasSuffix(args[len(args)-1], prompt) {
				t.Fatal("prompt modified or evaluated")
			}
			dangerous := strings.Contains(strings.Join(args[:len(args)-1], " "), "--dangerously-")
			if dangerous != (mode == "auto") {
				t.Fatal("permission mode changed")
			}
			if provider == "claude" && args[len(args)-2] != "--append-system-prompt" {
				t.Fatal("project instructions replaced")
			}
		}
	}
	t.Setenv("RELAY_SESSION_ID", "old-session")
	t.Setenv("RELAY_EVENT_TOKEN", "old-hook")
	t.Setenv("CODEX_HOME", "configured-provider-home")
	t.Setenv("CLAUDE_CONFIG_DIR", "configured-claude-home")
	s := &Server{stateDir: t.TempDir(), executable: "relay-test"}
	env := s.environment(map[string]string{"RELAY_AGENT_CAPABILITY_FILE": "new-capability"})
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	if values["RELAY_EVENT_TOKEN"] != "" || values["RELAY_SESSION_ID"] != "" || values["RELAY_AGENT_CAPABILITY_FILE"] != "new-capability" {
		t.Fatal("Relay capability leaked between sessions")
	}
	if values["CODEX_HOME"] != "configured-provider-home" || values["CLAUDE_CONFIG_DIR"] != "configured-claude-home" {
		t.Fatal("provider authentication location changed")
	}
	if advertisedFlag("--append-system-prompt-file FOO", "--append-system-prompt") {
		t.Fatal("unsupported lookalike flag accepted")
	}
}

func TestHarnessProviderPreflightUsesActualHelp(t *testing.T) {
	t.Setenv("LOCALAGENT_PROVIDER_FIXTURE", "1")
	exe, _ := os.Executable()
	s := &Server{stateDir: t.TempDir(), home: t.TempDir(), executable: exe}
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []string{"manual", "auto"} {
			if err := s.validateProvider(context.Background(), provider, exe, mode); err != nil {
				t.Fatalf("%s %s: %v", provider, mode, err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.validateProvider(ctx, "claude", exe, "auto") == nil {
		t.Fatal("cancelled preflight started a session")
	}
}

type harnessRoundTripper func(*http.Request) (*http.Response, error)

func (f harnessRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureHarnessClient(t *testing.T, fixtures map[string][]byte) *http.Client {
	t.Helper()
	return &http.Client{Transport: harnessRoundTripper(func(r *http.Request) (*http.Response, error) {
		data, ok := fixtures[r.URL.String()]
		if !ok {
			return nil, errors.New("unrecognized fixture URL")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Header: http.Header{}}, nil
	})}
}
func codexArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	writer := tar.NewWriter(gz)
	for name, value := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0700, Size: int64(len(value))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestCodexVerifiedPlatformArchiveKeepsResources(t *testing.T) {
	platform, triple, err := codexPlatform()
	if err != nil {
		t.Fatal(err)
	}
	version := "1.2.3"
	platformVersion := version + "-" + platform
	relative := "package/vendor/" + triple + "/bin/" + executableName("codex")
	helper := "package/vendor/" + triple + "/runtime/resource.dat"
	archive := codexArchive(t, map[string]string{relative: "fixture binary", helper: "required adjacent resource"})
	hash := sha512.Sum512(archive)
	latest := codexMetadata{Name: "@openai/codex", Version: version, OptionalDependencies: map[string]string{"@openai/codex-" + platform: "npm:@openai/codex@" + platformVersion}}
	metadata := codexMetadata{Name: "@openai/codex", Version: platformVersion}
	metadata.Dist.Tarball = "https://registry.npmjs.org/@openai/codex/-/codex-" + platformVersion + ".tgz"
	metadata.Dist.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(hash[:])
	mainJSON, _ := json.Marshal(latest)
	platformJSON, _ := json.Marshal(metadata)
	fixtures := map[string][]byte{"https://registry.npmjs.org/@openai/codex/latest": mainJSON, "https://registry.npmjs.org/@openai/codex/" + platformVersion: platformJSON, metadata.Dist.Tarball: archive}
	dest := t.TempDir()
	gotVersion, gotPath, err := prepareCodex(context.Background(), fixtureHarnessClient(t, fixtures), dest, io.Discard)
	if err != nil || gotVersion != version || gotPath != relative {
		t.Fatalf("verified preparation: %s %s %v", gotVersion, gotPath, err)
	}
	data, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(helper)))
	if err != nil || string(data) != "required adjacent resource" {
		t.Fatal("adjacent runtime files lost")
	}
	fixtures[metadata.Dist.Tarball] = append(append([]byte{}, archive...), 1)
	failed := t.TempDir()
	if _, _, err = prepareCodex(context.Background(), fixtureHarnessClient(t, fixtures), failed, io.Discard); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err = os.Stat(filepath.Join(failed, filepath.FromSlash(relative))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unverified binary was extracted")
	}
}

func TestHarnessArchiveRejectsUnsafeEntriesAndLinks(t *testing.T) {
	for _, name := range []string{"../outside", "package/../../outside", "/package/absolute", "package/C:stream", "package/a\\b", "package/NUL.txt", "package/folder.", "package/a/../b"} {
		if safeArchivePath(name) {
			t.Fatalf("unsafe path accepted: %q", name)
		}
		if err := extractCodex(bytes.NewReader(codexArchive(t, map[string]string{name: "unsafe"})), t.TempDir()); err == nil {
			t.Fatal("unsafe archive extracted")
		}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "package/link", Typeflag: tar.TypeSymlink, Linkname: "outside"})
	_ = tw.Close()
	_ = gz.Close()
	if err := extractCodex(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("archive symlink accepted")
	}
}

func TestHarnessTransportAndSignatureFailClosed(t *testing.T) {
	for _, address := range []string{"http://registry.npmjs.org/package", "https://registry.npmjs.org.evil.test/x", "https://user@registry.npmjs.org/x", "https://registry.npmjs.org:443/x"} {
		if err := fetchHarness(context.Background(), fixtureHarnessClient(t, nil), address, 10, io.Discard); err == nil {
			t.Fatal("untrusted download accepted")
		}
	}
	fixtures := map[string][]byte{claudeReleaseBase + "latest": []byte("1.2.3"), claudeReleaseBase + "1.2.3/manifest.json": []byte(`{"version":"1.2.3"}`), claudeReleaseBase + "1.2.3/manifest.json.sig": []byte("forged")}
	dir := t.TempDir()
	if _, _, err := prepareClaude(context.Background(), fixtureHarnessClient(t, fixtures), dir, io.Discard); err == nil {
		t.Fatal("unsigned vendor manifest accepted")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("unverified Claude binary created")
	}
}

func TestHarnessInstallLockAndCurrentPointer(t *testing.T) {
	dir := t.TempDir()
	lock, err := lockHarnessInstall(filepath.Join(dir, "install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockHarnessInstall(filepath.Join(dir, "install.lock")); err == nil {
		second.Close()
		t.Fatal("concurrent maintenance entered")
	}
	_ = lock.Close()
	again, err := lockHarnessInstall(filepath.Join(dir, "install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
	release := "1.2.3-" + strings.Repeat("a", 32)
	root := filepath.Join(dir, "tools", "harnesses", "claude")
	destination := filepath.Join(root, "releases", release)
	if err := privateDir(destination); err != nil {
		t.Fatal(err)
	}
	executable := executableName("claude")
	if err := os.WriteFile(filepath.Join(destination, executable), []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	pointer := harnessPointer{1, "claude", release, executable}
	data, _ := json.Marshal(pointer)
	if err := atomicWrite(filepath.Join(root, "current.json"), data); err != nil {
		t.Fatal(err)
	}
	got, err := currentHarness(dir, "claude")
	if err != nil || got != filepath.Join(destination, executable) {
		t.Fatal("current release was not resolved")
	}
	before, _ := os.ReadFile(got)
	pointer.Executable = "../../outside"
	data, _ = json.Marshal(pointer)
	if err := atomicWrite(filepath.Join(root, "current.json"), data); err != nil {
		t.Fatal(err)
	}
	if _, err := currentHarness(dir, "claude"); err == nil {
		t.Fatal("unsafe managed pointer accepted")
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, executableName("claude")), []byte("external fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", external)
	s := &Server{stateDir: dir, home: t.TempDir()}
	if path, err := s.discover("claude"); err == nil || path != "" {
		t.Fatal("corrupt managed pointer silently switched to an external provider")
	}
	after, _ := os.ReadFile(got)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("previous release was modified")
	}
}

func TestHarnessFailedRepairKeepsPublishedRelease(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	release := "1.2.3-" + strings.Repeat("b", 32)
	root := filepath.Join(state, "tools", "harnesses", "codex")
	executable := executableName("codex")
	oldPath := filepath.Join(root, "releases", release, executable)
	if err := privateDir(filepath.Dir(oldPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("preserve old executable"), 0700); err != nil {
		t.Fatal(err)
	}
	pointer, _ := json.Marshal(harnessPointer{1, "codex", release, executable})
	if err := atomicWrite(filepath.Join(root, "current.json"), pointer); err != nil {
		t.Fatal(err)
	}
	client := fixtureHarnessClient(t, map[string][]byte{"https://registry.npmjs.org/@openai/codex/latest": []byte(`{"name":"wrong publisher","version":"1.2.3"}`)})
	if err := installWithClient(context.Background(), state, "codex", "repair", io.Discard, client); err == nil {
		t.Fatal("invalid publisher was installed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := installWithClient(ctx, state, "codex", "repair", io.Discard, client); err == nil {
		t.Fatal("cancelled repair reported success")
	}
	after, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil || !bytes.Equal(after, pointer) {
		t.Fatal("failed repair changed active release")
	}
	data, _ := os.ReadFile(oldPath)
	if string(data) != "preserve old executable" {
		t.Fatal("failed repair changed running binary")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "releases"))
	if len(entries) != 1 {
		t.Fatal("failed preparation left unfinished published releases")
	}
}

func TestHarnessRealVendorInstall(t *testing.T) {
	if os.Getenv("RELAY_LOCALAGENT_VENDOR_SMOKE") != "1" {
		t.Skip("set RELAY_LOCALAGENT_VENDOR_SMOKE=1 for verified vendor download/version smoke")
	}
	if runtime.GOOS != "windows" {
		t.Skip("native Windows release gate")
	}
	state := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			if err := Install(ctx, state, provider, "repair", io.Discard); err != nil {
				t.Fatal(err)
			}
			path, err := currentHarness(state, provider)
			if err != nil {
				t.Fatal(err)
			}
			exe, _ := os.Executable()
			s := &Server{stateDir: state, home: t.TempDir(), executable: exe}
			version, err := s.probe(ctx, path, "--version")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Verified native %s version: %s", provider, strings.TrimSpace(version))
			if err := s.validateProvider(ctx, provider, path, "auto"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
