//go:build linux && !relay_ssh_native

package transport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func exerciseSSHCLIAndMaintenance(t *testing.T, ctx context.Context, connection *Connection, binary, config string, port int, bundle string) {
	t.Helper()
	directory := t.TempDir()
	key := filepath.Join(directory, "fixture-key")
	if output, err := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("fixture key: %v %s", err, output)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Run(ctx, `umask 077; mkdir -p "$HOME/.ssh"; cat > "$HOME/.ssh/authorized_keys"`, strings.NewReader(string(public))); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, "\nHost relay-cli\n HostName 127.0.0.1\n User relay\n Port %d\n UserKnownHostsFile %s\n GlobalKnownHostsFile /dev/null\n IdentityFile %s\n IdentitiesOnly yes\n IdentityAgent none\n PreferredAuthentications publickey\n", port, filepath.Join(filepath.Dir(config), "known_hosts"), key)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) ([]byte, error) {
		argv := append([]string{"session"}, args...)
		argv = append(argv, "--ssh", "relay-cli")
		return exec.CommandContext(ctx, binary, argv...).CombinedOutput()
	}
	projectDirectory := "/home/relay/CLI project ' $(printf ignored)"
	if _, err := connection.Run(ctx, "mkdir -p "+quoteShell(projectDirectory), nil); err != nil {
		t.Fatal(err)
	}
	output, err := cli("start", "--harness", "shell", "--cwd", projectDirectory, "--title", "Cross-host 'literal' $() session")
	var session struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	}
	if err != nil || json.Unmarshal(output, &session) != nil || session.ID == "" || session.Cwd != projectDirectory {
		t.Fatalf("SSH CLI start: %v %s", err, output)
	}
	output, err = cli("list")
	if err != nil || !strings.Contains(string(output), session.ID) {
		t.Fatal("SSH CLI list", err, string(output))
	}
	dialer := websocket.Dialer{NetDialContext: connection.DialContext, HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.DialContext(ctx, "ws://relay/api/sessions/"+session.ID+"/terminal", http.Header{"Origin": []string{"http://relay"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	control := func(owner bool) {
		t.Helper()
		typeName := "release"
		if owner {
			typeName = "claim"
		}
		if err := ws.WriteJSON(map[string]string{"type": typeName}); err != nil {
			t.Fatal(err)
		}
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if kind == websocket.TextMessage {
				var state struct {
					Type  string `json:"type"`
					Owner bool   `json:"owner"`
				}
				if json.Unmarshal(data, &state) == nil && state.Type == "control" && state.Owner == owner {
					break
				}
			}
		}
	}
	control(true)
	if _, err := cli("send", "--id", session.ID, "--text", "printf RELAY_DENIED_CLI", "--enter"); err == nil {
		t.Fatal("CLI bypassed another terminal's input ownership")
	}
	control(false)
	if output, err := cli("send", "--id", session.ID, "--text", `printf 'RELAY_%s\n' 'SSH_CLI_OK'`, "--enter"); err != nil {
		t.Fatal("SSH CLI input", err, string(output))
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		output, err = cli("read", "--id", session.ID)
		if err != nil {
			t.Fatal("SSH CLI read", err, string(output))
		}
		if strings.Contains(string(output), "RELAY_DENIED_CLI") {
			t.Fatal("rejected CLI input executed")
		}
		if strings.Contains(string(output), "RELAY_SSH_CLI_OK") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI input did not execute")
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = ws.Close()
	if output, err := cli("stop", "--id", session.ID); err != nil {
		t.Fatal("SSH CLI stop", err, string(output))
	}

	// Exercise controller fallback against the already-running daemon. An
	// intentionally invalid fixture-only maintenance lock makes the new worker
	// exit before any vendor download, while proving it runs in the old PTY.
	if _, err := connection.Run(ctx, `mkdir -p "$HOME/.local/share/relay/tools/harnesses/codex"; ln -s /dev/null "$HOME/.local/share/relay/tools/harnesses/codex/maintenance.lock"`, nil); err != nil {
		t.Fatal(err)
	}
	defer connection.Run(context.Background(), `rm -f "$HOME/.local/share/relay/tools/harnesses/codex/maintenance.lock"`, nil)
	state := filepath.Join(directory, "controller")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "hosts.json"), []byte(`[{"id":"fixture","name":"Fixture","target":"relay-cli","status":"disconnected"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	type controllerLaunch struct {
		URL     string `json:"url"`
		Address string `json:"address"`
		PID     int    `json:"pid"`
	}
	launchController := func() (controllerLaunch, func()) {
		t.Helper()
		launchData, err := exec.CommandContext(ctx, binary, "desktop", "--state-dir", state, "--binaries", bundle, "--local=false").Output()
		if err != nil {
			t.Fatal("fixture controller launch", err)
		}
		var launch controllerLaunch
		if json.Unmarshal(launchData, &launch) != nil || launch.PID < 2 {
			t.Fatal("invalid fixture controller launch response")
		}
		pidfd, err := unix.PidfdOpen(launch.PID, 0)
		if err != nil {
			t.Fatal("pin fixture controller", err)
		}
		t.Cleanup(func() { unix.Close(pidfd) })
		executable, err := os.Readlink("/proc/" + strconv.Itoa(launch.PID) + "/exe")
		if err != nil {
			t.Fatal("read fixture controller identity", err)
		}
		actual, err := os.ReadFile(executable)
		expected, expectedErr := os.ReadFile(binary)
		if err != nil || expectedErr != nil || sha256.Sum256(actual) != sha256.Sum256(expected) || !strings.HasPrefix(executable, state+string(os.PathSeparator)) {
			t.Fatal("fixture controller binary identity differs from launched private CLI")
		}
		stopped := false
		stop := func() {
			t.Helper()
			if stopped {
				return
			}
			if err := unix.PidfdSendSignal(pidfd, syscall.SIGTERM, nil, 0); err != nil && err != syscall.ESRCH {
				t.Error("stop exact fixture controller", err)
				return
			}
			ready, err := unix.Poll([]unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}, 8000)
			if err != nil || ready == 0 {
				t.Error("fixture controller did not stop", err)
				return
			}
			stopped = true
		}
		t.Cleanup(stop)
		return launch, stop
	}
	launch, stopController := launchController()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	response, err := client.Get(launch.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("controller login failed", response.StatusCode)
	}
	get := func(path string) []byte {
		t.Helper()
		response, err := client.Get(launch.Address + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != 200 {
			t.Fatal("controller GET", path, response.StatusCode, err)
		}
		return data
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		data := get("/api/state")
		if strings.Contains(string(data), `"status":"online"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("saved key-authenticated host did not reconnect", string(data))
		}
		time.Sleep(30 * time.Millisecond)
	}
	var bootstrap struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal(get("/api/bootstrap"), &bootstrap) != nil || bootstrap.CSRF == "" {
		t.Fatal("bootstrap missing CSRF")
	}
	type maintenanceJob struct {
		ID               string    `json:"id"`
		HostID           string    `json:"hostId"`
		Status           string    `json:"status"`
		SessionID        string    `json:"sessionId"`
		SessionCreatedAt time.Time `json:"sessionCreatedAt"`
		CleanupStatus    string    `json:"cleanupStatus"`
		Error            string    `json:"error"`
	}
	type runtimeSession struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
		Status    string    `json:"status"`
		ExitCode  *int      `json:"exitCode"`
	}
	sessions := func() []runtimeSession {
		t.Helper()
		var sessions []runtimeSession
		if err := json.Unmarshal(get("/api/hosts/fixture/runtime/sessions"), &sessions); err != nil {
			t.Fatal(err)
		}
		return sessions
	}
	baseline := sessions()
	startJob := func() maintenanceJob {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", launch.Address+"/api/hosts/fixture/harnesses/codex/update", nil)
		req.Header.Set("X-Relay-CSRF", bootstrap.CSRF)
		req.Header.Set("Origin", launch.Address)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		var job maintenanceJob
		if err != nil || response.StatusCode != http.StatusAccepted || json.Unmarshal(data, &job) != nil || job.ID == "" || job.HostID != "fixture" {
			t.Fatal("maintenance background acceptance", response.StatusCode, err, string(data))
		}
		return job
	}
	waitJob := func(id, desired string, timeout time.Duration) maintenanceJob {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			var current struct {
				Jobs []maintenanceJob `json:"maintenanceJobs"`
			}
			data := get("/api/state")
			if err := json.Unmarshal(data, &current); err != nil {
				t.Fatal(err)
			}
			for _, job := range current.Jobs {
				if job.ID != id {
					continue
				}
				if job.Status == desired {
					return job
				}
				if job.Status == "failed" || job.Status == "uncertain" || job.Status == "succeeded" {
					t.Fatalf("maintenance wanted %s, got %+v", desired, job)
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("maintenance job did not reach %s: %s", desired, data)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	failed := waitJob(startJob().ID, "failed", 15*time.Second)
	if failed.SessionID == "" || failed.SessionCreatedAt.IsZero() || failed.CleanupStatus != "retained" {
		t.Fatal("failed worker identity or retained output missing", failed)
	}
	history := get("/api/hosts/fixture/runtime/sessions/" + failed.SessionID + "/history")
	if !strings.Contains(string(history), "maintenance lock") {
		t.Fatal("standalone maintenance worker did not run", string(history))
	}
	foundFailed := false
	for _, item := range sessions() {
		if item.ID == failed.SessionID {
			foundFailed = item.CreatedAt.Equal(failed.SessionCreatedAt) && item.Status == "exited" && item.ExitCode != nil && *item.ExitCode != 0
		}
	}
	if !foundFailed {
		t.Fatal("failed maintenance session was removed or replaced")
	}

	if os.Getenv("RELAY_SSH_HARNESSES") == "1" {
		if _, err := connection.Run(ctx, `rm -f "$HOME/.local/share/relay/tools/harnesses/codex/maintenance.lock"`, nil); err != nil {
			t.Fatal(err)
		}
		running := waitJob(startJob().ID, "running", 15*time.Second)
		if running.SessionID == "" || running.SessionCreatedAt.IsZero() {
			t.Fatal("running job has no durable session identity", running)
		}
		// Stop only this test's pinned controller. The real remote worker and
		// unrelated PTYs continue; no browser remains to perform cleanup.
		stopController()
		time.Sleep(250 * time.Millisecond)
		launch, _ = launchController()
		jar, _ = cookiejar.New(nil)
		client = &http.Client{Jar: jar, Timeout: 30 * time.Second}
		response, err = client.Get(launch.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("restarted fixture controller login", response.StatusCode)
		}
		if json.Unmarshal(get("/api/bootstrap"), &bootstrap) != nil || bootstrap.CSRF == "" {
			t.Fatal("restarted fixture bootstrap missing CSRF")
		}
		succeeded := waitJob(running.ID, "succeeded", 3*time.Minute)
		if succeeded.CleanupStatus != "removed" || succeeded.SessionID != running.SessionID || !succeeded.SessionCreatedAt.Equal(running.SessionCreatedAt) {
			t.Fatal("successful job identity/cleanup changed", succeeded)
		}
		remaining := sessions()
		for _, item := range remaining {
			if item.ID == running.SessionID {
				t.Fatal("successful background worker row survived cleanup")
			}
		}
		for _, before := range baseline {
			found := false
			for _, after := range remaining {
				if before.ID == after.ID && before.CreatedAt.Equal(after.CreatedAt) && before.Status == after.Status {
					found = true
				}
			}
			if !found {
				t.Fatal("background cleanup changed an unrelated session", before.ID)
			}
		}
		failedStillPresent := false
		for _, item := range remaining {
			if item.ID == failed.SessionID && item.CreatedAt.Equal(failed.SessionCreatedAt) {
				failedStillPresent = true
			}
		}
		if !failedStillPresent {
			t.Fatal("successful job cleanup removed failed worker output")
		}
		var health struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(get("/api/hosts/fixture/runtime/health"), &health) != nil || health.Version != "ssh-integration" {
			t.Fatal("controller restart or maintenance replaced live daemon", health.Version)
		}
		t.Log("real Codex background update survived fixture controller restart; successful owned worker removed, failed output and unrelated live PTY preserved")
	}
	if output, err := cli("stop", "--id", failed.SessionID); err != nil {
		t.Fatal("explicit fixture cleanup of failed worker", err, string(output))
	}

	t.Log("verified native OpenSSH cross-host CLI list/start/send/read/stop, exclusive input, saved-host startup reconnect, and durable background maintenance on the existing daemon")
}
