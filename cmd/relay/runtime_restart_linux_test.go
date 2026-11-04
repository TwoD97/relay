package main

import (
	"bytes"
	"context"
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
	"syscall"
	"testing"
	"time"

	runtimeServer "github.com/TwoD97/relay/internal/runtime"
	"golang.org/x/sys/unix"
)

// This uses the actual daemon command, cooperative shutdown signal and detached
// ensure launcher. No providers or personal host/configuration files are used.
func TestRuntimeCLIExplicitRestartRestoresOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two isolated runtime versions")
	}
	dir := shortDesktopDirectory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	build := func(name, version string) string {
		t.Helper()
		binary := filepath.Join(dir, name)
		cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-ldflags", "-X main.version="+version, "-o", binary, ".")
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build runtime: %v %s", err, data)
		}
		return binary
	}
	oldBinary := build("old-runtime", "0.4.0-restart-old")
	newBinary := build("new-runtime", "0.4.0-restart-new")
	state := filepath.Join(dir, "state")
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "RELAY_") || key == "SHELL" || key == "ENV" || key == "PS1" || key == "CLAUDE_CONFIG_DIR" || key == "CODEX_HOME" {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "SHELL=/bin/sh", "ENV=", "PS1=RESTART_FIXTURE_READY ", "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "claude"), "CODEX_HOME="+filepath.Join(dir, "codex"))
	log, err := os.Create(filepath.Join(dir, "old-runtime.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	old := exec.Command(oldBinary, "daemon", "--state-dir", state)
	old.Env, old.Stdout, old.Stderr = env, log, log
	if err = old.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = old.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = old.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = old.Process.Kill()
			<-done
		}
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath(state))
	}, DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, body any) (int, []byte, error) {
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, "http://runtime"+path, bytes.NewReader(data))
		req.GetBody = nil
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		data, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if len(data) > 1<<20 {
			return 0, nil, fmt.Errorf("test response exceeds bound")
		}
		return response.StatusCode, data, err
	}
	call := func(method, path string, body, out any) {
		t.Helper()
		status, data, err := request(method, path, body)
		if err != nil || status < 200 || status >= 300 {
			t.Fatalf("%s %s: HTTP%d %v %s", method, path, status, err, data)
		}
		if out != nil && json.Unmarshal(data, out) != nil {
			t.Fatalf("invalid fixture response %s", data)
		}
	}
	await := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if condition() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("isolated runtime condition did not become true")
	}
	type healthIdentity struct {
		Version    string `json:"version"`
		RuntimeID  string `json:"runtimeId"`
		InstanceID string `json:"instanceId"`
	}
	var before healthIdentity
	await(func() bool {
		status, data, err := request("GET", "/api/health", nil)
		return err == nil && status == 200 && json.Unmarshal(data, &before) == nil
	})
	if before.Version != "0.4.0-restart-old" || before.RuntimeID == "" || before.InstanceID == "" {
		t.Fatal("missing initial runtime identity", before)
	}
	var original runtimeServer.Session
	call("POST", "/api/sessions", map[string]any{"title": "Restart sentinel", "workspace": "Fixture", "cwd": dir, "harness": "shell"}, &original)
	history := func(id string) string {
		_, data, err := request("GET", "/api/sessions/"+id+"/history", nil)
		if err != nil {
			return ""
		}
		return string(data)
	}
	await(func() bool { return strings.Contains(history(original.ID), "RESTART_FIXTURE_READY") })
	call("POST", "/api/sessions/"+original.ID+"/input", map[string]string{"data": "printf 'BEFORE_%s\\n' RESTART_MARKER\r"}, nil)
	await(func() bool { return strings.Contains(history(original.ID), "BEFORE_RESTART_MARKER") })
	var plan runtimeServer.RestartPlan
	call("GET", "/api/recovery/preflight", nil, &plan)
	if len(plan.Blockers) != 0 || len(plan.Entries) != 1 || plan.Entries[0].Mode != "reopen" {
		t.Fatal("unexpected restart plan", plan)
	}
	opID := strings.Repeat("c", 32)
	call("POST", "/api/recovery/start", map[string]string{"id": opID, "fingerprint": plan.Fingerprint}, nil)
	// The test sends no signal here: the accepted durable restart must reach the
	// daemon command's cooperative shutdown branch and exit successfully itself.
	select {
	case <-done:
		if waitErr != nil {
			t.Fatal("cooperative daemon exit", waitErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("daemon did not exit after accepted restart")
	}
	ensure := func() {
		t.Helper()
		cmd := exec.CommandContext(ctx, newBinary, "ensure", "--state-dir", state)
		cmd.Env = env
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("new runtime ensure: %v %s", err, data)
		}
		var health healthIdentity
		if json.Unmarshal(data, &health) != nil || health.Version != "0.4.0-restart-new" {
			t.Fatal("ensure launched wrong runtime", string(data))
		}
	}
	ensure()
	connection, err := net.Dial("unix", socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := pinControllerProcess(connection)
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint lives only in this test's new temporary directory. Pin its
	// process and verify exact executable/argv before retaining a cleanup handle.
	proc := filepath.Join("/proc", strconv.Itoa(pinned.PID))
	args, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		pinned.Close()
		t.Fatal(err)
	}
	executable, err := os.Readlink(filepath.Join(proc, "exe"))
	expected := []byte(strings.Join([]string{newBinary, "daemon", "--state-dir", state, ""}, "\x00"))
	if err != nil || executable != newBinary || !bytes.Equal(args, expected) {
		pinned.Close()
		t.Fatal("cleanup peer is not the exact test-owned daemon")
	}
	t.Cleanup(func() {
		defer pinned.Close()
		_ = unix.PidfdSendSignal(pinned.handle, unix.SIGTERM, nil, 0)
		if err := pinned.Wait(context.Background()); err != nil {
			_ = unix.PidfdSendSignal(pinned.handle, unix.SIGKILL, nil, 0)
			if err = pinned.Wait(context.Background()); err != nil {
				t.Error("test-owned daemon cleanup", err)
			}
		}
	})
	var after healthIdentity
	call("GET", "/api/health", nil, &after)
	if after.RuntimeID != before.RuntimeID || after.InstanceID == before.InstanceID || after.InstanceID == "" {
		t.Fatal("runtime identity did not survive replacement", before, after)
	}
	var status struct {
		Operation *runtimeServer.RestartOperation `json:"operation"`
	}
	await(func() bool {
		code, data, err := request("GET", "/api/recovery", nil)
		return err == nil && code == 200 && json.Unmarshal(data, &status) == nil && status.Operation != nil && status.Operation.Status == "completed"
	})
	var sessions []runtimeServer.Session
	call("GET", "/api/sessions", nil, &sessions)
	if len(sessions) != 2 {
		t.Fatal("restart did not retain exactly old+new records", len(sessions))
	}
	var replacement runtimeServer.Session
	for _, session := range sessions {
		if session.ID == original.ID {
			if session.Status != "interrupted" || !session.CreatedAt.Equal(original.CreatedAt) {
				t.Fatal("old identity/status changed", session)
			}
		} else {
			replacement = session
		}
	}
	if replacement.ID == "" || replacement.Status != "running" || replacement.RestartReceipt != opID+":"+original.ID || replacement.RestartSource == nil || replacement.RestartSource.ID != original.ID || !replacement.RestartSource.CreatedAt.Equal(original.CreatedAt) {
		t.Fatal("missing exact restoration receipt", replacement)
	}
	if !strings.Contains(history(original.ID), "BEFORE_RESTART_MARKER") {
		t.Fatal("original terminal log lost")
	}
	await(func() bool { return strings.Contains(history(replacement.ID), "RESTART_FIXTURE_READY") })
	ensure()
	var unchanged healthIdentity
	call("GET", "/api/health", nil, &unchanged)
	call("GET", "/api/sessions", nil, &sessions)
	if unchanged.InstanceID != after.InstanceID || len(sessions) != 2 {
		t.Fatal("repeat ensure restarted or duplicated restoration", unchanged, len(sessions))
	}
}
