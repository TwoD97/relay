//go:build !windows && !relay_ssh_native

package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Run explicitly with RELAY_SSH_INTEGRATION=1 go test -race ./internal/transport
// -run TestRealSSH -v. Uses a disposable Docker sshd bound only to loopback and
// isolated known_hosts/config files; it never contacts a configured user host.
func TestRealSSH(t *testing.T) {
	if os.Getenv("RELAY_SSH_INTEGRATION") != "1" {
		t.Skip("set RELAY_SSH_INTEGRATION=1 to run disposable Docker SSH integration")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux/WSL integration")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	run := func(binary string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", filepath.Base(binary), err, out)
		}
		return out
	}
	run(docker, "build", "-q", "-t", "relay-ssh-test:local", "testdata/sshd")
	id := strings.TrimSpace(string(run(docker, "run", "--rm", "-d", "-p", "127.0.0.1::22", "relay-ssh-test:local")))
	t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", id).Run() })
	binding := strings.TrimSpace(string(run(docker, "port", id, "22/tcp")))
	_, portString, err := net.SplitHostPort(binding)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	config := filepath.Join(dir, "ssh_config")
	configData := "Host relay-integration\n HostName 127.0.0.1\n User relay\n UserKnownHostsFile " + knownHosts + "\n GlobalKnownHostsFile /dev/null\n PreferredAuthentications password\n PubkeyAuthentication no\n IdentityAgent none\n"
	if err := os.WriteFile(config, []byte(configData), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "ssh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoteShell(ssh)+" -F "+quoteShell(config)+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bundle := filepath.Join(dir, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	name := "relay-linux-" + runtime.GOARCH
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-trimpath", "-ldflags", "-X main.version=ssh-integration", "-o", filepath.Join(bundle, name), "../../cmd/relay")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runtime: %v\n%s", err, out)
	}
	artifact, err := os.ReadFile(filepath.Join(bundle, name))
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("%x  %s\n", sha256.Sum256(artifact), name)
	if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}

	connect := func(expectHostPrompt bool) *Connection {
		t.Helper()
		conn, err := Start(ctx, Config{Target: "relay-integration", Port: port})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		history, output, unsubscribe := conn.Subscribe()
		defer unsubscribe()
		ready := make(chan error, 1)
		go func() { ready <- conn.WaitReady(ctx) }()
		transcript := string(history)
		accepted, authenticated := false, false
		for {
			if !accepted && strings.Contains(transcript, "(yes/no") {
				if _, err := conn.Write([]byte("yes\n")); err != nil {
					t.Fatal(err)
				}
				accepted = true
			}
			if !authenticated && strings.Contains(transcript, "password:") {
				if _, err := conn.Write([]byte("relay-smoke-only\n")); err != nil {
					t.Fatal(err)
				}
				authenticated = true
			}
			select {
			case err := <-ready:
				if err != nil {
					t.Fatalf("SSH login: %v\n%s", err, transcript)
				}
				if accepted != expectHostPrompt || !authenticated {
					t.Fatalf("unexpected trust/login prompts: accepted=%v authenticated=%v", accepted, authenticated)
				}
				if strings.Contains(transcript, "relay-smoke-only") {
					t.Fatal("password echoed into terminal history")
				}
				return conn
			case chunk, ok := <-output:
				if !ok {
					t.Fatalf("SSH terminal closed: %s", transcript)
				}
				transcript += string(chunk)
			case <-ctx.Done():
				t.Fatalf("SSH login timeout: %s", transcript)
			}
		}
	}

	first := connect(true)
	if err := first.Bootstrap(ctx, bundle, "ssh-integration", func(s string) { t.Log(s) }); err != nil {
		t.Fatal(err)
	}
	httpClient := func(c *Connection) *http.Client {
		tr := &http.Transport{DialContext: c.DialContext, DisableKeepAlives: true}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr, Timeout: 10 * time.Second}
	}
	request := func(client *http.Client, method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://relay"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: status %d, body %s", method, path, response.StatusCode, data)
		}
		return data
	}
	client := httpClient(first)
	data := request(client, "POST", "/api/sessions", `{"title":"SSH survival test","workspace":"Integration","cwd":"/home/relay","harness":"shell"}`, 201)
	var session struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &session); err != nil || session.ID == "" {
		t.Fatalf("session: %s %v", data, err)
	}
	path := "/api/sessions/" + session.ID
	dialer := websocket.Dialer{NetDialContext: first.DialContext, HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, "ws://relay"+path+"/terminal", http.Header{"Origin": []string{"http://relay"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(map[string]any{"type": "claim"}); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(map[string]any{"type": "resize", "cols": 100, "rows": 30}); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(map[string]any{"type": "input", "data": "export RELAY_SMOKE=743291; printf 'BEFORE-%s\\n' \"$RELAY_SMOKE\"\n"}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var terminal bytes.Buffer
	for !strings.Contains(terminal.String(), "BEFORE-743291") {
		kind, chunk, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("terminal before disconnect: %v (%q)", err, terminal.String())
		}
		if kind == websocket.BinaryMessage {
			terminal.Write(chunk)
		}
	}
	_ = ws.Close()
	// A ready master can lose its control socket independently of its process.
	// Slave commands must then fail, never open a new authenticated connection.
	if err := os.Remove(first.cfg.ControlPath); err != nil {
		t.Fatal(err)
	}
	failedCtx, failedCancel := context.WithTimeout(ctx, 3*time.Second)
	_, fallbackErr := first.Run(failedCtx, "true", nil)
	failedCancel()
	if fallbackErr == nil {
		t.Fatal("command succeeded after control socket disappeared")
	}
	_ = first.Close()
	if _, err := first.Run(ctx, "true", nil); err == nil {
		t.Fatal("disconnected transport ran a command")
	}

	second := connect(false)
	if err := second.Bootstrap(ctx, "/does-not-exist", "newer-client-build", func(s string) { t.Log(s) }); err != nil {
		t.Fatalf("reuse without artifact: %v", err)
	}
	folderListing, err := second.ListDirectories(ctx, DirectoryQuery{Path: "~", Hidden: true})
	if err != nil || folderListing.Path != "/home/relay" || folderListing.Home != "/home/relay" {
		t.Fatal("browse with compatible existing runtime and no new artifact", folderListing, err)
	}
	updated := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-trimpath", "-ldflags", "-X main.version=ssh-integration-updated", "-o", filepath.Join(bundle, name), "../../cmd/relay")
	updated.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := updated.CombinedOutput(); err != nil {
		t.Fatalf("build updated runtime: %v\n%s", err, out)
	}
	newArtifact, err := os.ReadFile(filepath.Join(bundle, name))
	if err != nil {
		t.Fatal(err)
	}
	newManifest := fmt.Sprintf("%x  %s\n", sha256.Sum256(newArtifact), name)
	// The desktop controller stages a complete immutable runtime bundle. Supply
	// both real architecture artifacts even though this host executes only one.
	otherArch := "arm64"
	if runtime.GOARCH == otherArch {
		otherArch = "amd64"
	}
	otherName := "relay-linux-" + otherArch
	otherBuild := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-trimpath", "-ldflags", "-X main.version=ssh-integration-updated", "-o", filepath.Join(bundle, otherName), "../../cmd/relay")
	otherBuild.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+otherArch)
	if out, err := otherBuild.CombinedOutput(); err != nil {
		t.Fatalf("build companion runtime: %v\n%s", err, out)
	}
	otherArtifact, err := os.ReadFile(filepath.Join(bundle, otherName))
	if err != nil {
		t.Fatal(err)
	}
	newManifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(otherArtifact), otherName)
	if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), []byte(newManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := second.RefreshCLI(ctx, bundle, "ssh-integration-updated"); err != nil {
		t.Fatal("refresh agent CLI beside an older compatible daemon", err)
	}
	repaired, err := second.Repair(ctx, bundle, "ssh-integration-updated", func(stage string) { t.Log(stage) })
	if err != nil || repaired.InstalledVersion != "ssh-integration-updated" || repaired.RunningVersion != "ssh-integration" || !repaired.RestartRequired {
		t.Fatal("repair replaced compatible live runtime or misreported versions", repaired, err)
	}
	installedVersion, err := second.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" version`, nil)
	if err != nil || strings.TrimSpace(string(installedVersion)) != "ssh-integration-updated" {
		t.Fatal("updated binary not activated", string(installedVersion), err)
	}
	// Repair a missing bridge without touching the daemon or its active PTY.
	if _, err := second.Run(ctx, `rm "$HOME/.local/share/relay/bin/relay"`, nil); err != nil {
		t.Fatal(err)
	}
	if result, err := second.Repair(ctx, bundle, "ssh-integration-updated", nil); err != nil || result.RunningVersion != "ssh-integration" {
		t.Fatal("missing bridge repair failed or restarted daemon", result, err)
	}
	if err := second.RefreshCLI(ctx, "/missing-unused-bundle", "ssh-integration-updated"); err != nil {
		t.Fatal("matching source CLI unnecessarily uploaded a bundle", err)
	}
	exerciseSSHProjectContext(t, ctx, second)
	cliBinary := filepath.Join(dir, "relay-cli")
	if err := os.WriteFile(cliBinary, newArtifact, 0700); err != nil {
		t.Fatal(err)
	}
	exerciseSSHCLIAndMaintenance(t, ctx, second, cliBinary, config, port, bundle)
	if err := os.WriteFile(filepath.Join(bundle, name), []byte("corrupt update"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Repair(ctx, bundle, "ssh-integration-updated", nil); err == nil {
		t.Fatal("corrupt repair artifact accepted")
	}
	client = httpClient(second)
	request(client, "POST", path+"/input", `{"data":"printf 'AFTER-%s\\n' \"$RELAY_SMOKE\"\n"}`, 204)
	deadline := time.Now().Add(10 * time.Second)
	for {
		history := request(client, "GET", path+"/history", "", 200)
		if bytes.Contains(history, []byte("AFTER-743291")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not survive SSH reconnect: %s", history)
		}
		time.Sleep(50 * time.Millisecond)
	}
	request(client, "DELETE", path, "", 204)
	if os.Getenv("RELAY_SSH_HARNESSES") == "1" {
		for _, harness := range []string{"codex", "claude"} {
			data := request(client, "POST", "/api/harnesses/"+harness+"/install", "", 201)
			var install struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(data, &install); err != nil || install.ID == "" {
				t.Fatalf("start %s install: %s %v", harness, data, err)
			}
			installDeadline := time.Now().Add(3 * time.Minute)
			for {
				data := request(client, "GET", "/api/sessions", "", 200)
				var sessions []struct {
					ID       string `json:"id"`
					Status   string `json:"status"`
					ExitCode *int   `json:"exitCode"`
				}
				if err := json.Unmarshal(data, &sessions); err != nil {
					t.Fatal(err)
				}
				finished := false
				for _, item := range sessions {
					if item.ID != install.ID || item.Status == "running" {
						continue
					}
					if item.Status != "exited" || item.ExitCode == nil || *item.ExitCode != 0 {
						history := request(client, "GET", "/api/sessions/"+install.ID+"/history", "", 200)
						t.Fatalf("%s install failed: status=%s exit=%v history=%s", harness, item.Status, item.ExitCode, history)
					}
					finished = true
				}
				if finished {
					break
				}
				if time.Now().After(installDeadline) {
					t.Fatalf("%s install timed out", harness)
				}
				time.Sleep(250 * time.Millisecond)
			}
			data = request(client, "GET", "/api/harnesses", "", 200)
			var installed []struct {
				ID        string `json:"id"`
				Installed bool   `json:"installed"`
				Version   string `json:"version"`
			}
			if err := json.Unmarshal(data, &installed); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range installed {
				if item.ID == harness && item.Installed && item.Version != "" {
					found = true
					t.Logf("verified clean user-space install: %s %s", harness, item.Version)
				}
			}
			if !found {
				t.Fatalf("installed harness did not run: %s", data)
			}
		}
	}
	_ = second.Close()

	// Replace only this test's trust file and prove changed keys fail closed.
	keyPath := filepath.Join(dir, "different_host_key")
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath)
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(string(pub))
	badKnown := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", port, parts[0], parts[1])
	if err := os.WriteFile(knownHosts, []byte(badKnown), 0600); err != nil {
		t.Fatal(err)
	}
	bad, err := Start(ctx, Config{Target: "relay-integration", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bad.Close() })
	keyCtx, keyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer keyCancel()
	if err := bad.WaitReady(keyCtx); err == nil {
		t.Fatal("changed host key was accepted")
	}
	history, _, unsub := bad.Subscribe()
	unsub()
	if !bytes.Contains(history, []byte("REMOTE HOST IDENTIFICATION HAS CHANGED")) {
		t.Fatalf("expected changed-key rejection: %s", history)
	}
	t.Log("verified interactive host trust/password login, checksum deployment, SSH HTTP/WebSocket PTYs, reconnect with a newer compatible client preserving the process, and changed-host-key rejection")
}
