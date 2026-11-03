package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/controller"
)

func captureAgentCLI(t *testing.T, command func([]string) error, args ...string) ([]byte, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "output-")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = previous }()
	err = command(args)
	if closeErr := f.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	data, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return data, err
}

func agentCLIController(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "relay-agent-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := prepareControllerDirectory(dir); err != nil {
		t.Fatal(err)
	}
	launch := strings.Repeat("a", 32)
	// A rejected start gives the fixture a durable active capability, without
	// starting any real provider or reading any personal host/account state.
	record := map[string]any{"format": 1, "revision": 1, "enabled": true, "config": map[string]string{"hostId": "local", "harness": "codex", "agentMode": "manual", "cwd": dir}, "status": "blocked", "phase": "rejected", "token": launch, "canonicalCwd": dir, "hostTarget": "fixture", "execution": "native"}
	data, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(dir, "mastermind.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	app, err := controller.New(controller.Config{StateDir: dir, Address: "127.0.0.1:43210", Version: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	path, err := app.PrepareWorkspaceAgentCapability(launch)
	if err != nil {
		t.Fatal(err)
	}
	listener, cleanup, err := listenControllerControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: controllerControlHandler(app, controllerInfo{Address: "http://127.0.0.1:43210", Version: "fixture", Protocol: controllerProtocol, PID: os.Getpid()}, uiOptions{StateDir: dir}), ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); cleanup() })
	t.Setenv("RELAY_CONTROLLER_STATE", dir)
	t.Setenv("RELAY_AGENT_CAPABILITY_FILE", path)
	return dir
}

func TestWorkspaceCLIUsesPrivateControllerAndPreservesRevisionConflicts(t *testing.T) {
	dir := agentCLIController(t)
	call := func(args ...string) map[string]any {
		t.Helper()
		data, err := captureAgentCLI(t, workspaceCommand, args...)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := call("show"); got["revision"] != float64(0) {
		t.Fatal("unexpected initial revision")
	}
	file := filepath.Join(dir, "plan with spaces.html")
	html := "<h1>Plan & evidence</h1><style>body{color:green}</style>"
	if err := os.WriteFile(file, []byte(html), 0600); err != nil {
		t.Fatal(err)
	}
	created := call("pages", "create", "--revision", "0", "--title", "Plan", "--html-file", file)
	pageID := created["page"].(map[string]any)["id"].(string)
	if !agentID(pageID) || call("pages", "get", "--id", pageID)["html"] != html {
		t.Fatal("page content changed")
	}
	if _, err := captureAgentCLI(t, workspaceCommand, "tasks", "create", "--revision", "0", "--title", "stale"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal("stale CLI write was not rejected", err)
	}
	task := call("tasks", "create", "--revision", "1", "--title", "Inspect", "--status", "doing", "--page", pageID)["task"].(map[string]any)
	taskID := task["id"].(string)
	if task["status"] != "doing" || task["pageId"] != pageID {
		t.Fatal("task links/status changed")
	}
	call("tasks", "update", "--revision", "2", "--id", taskID, "--title", "Inspected", "--status", "done", "--detail", "Verified test result")
	if call("tasks", "get", "--id", taskID)["status"] != "done" {
		t.Fatal("task update missing")
	}
	call("pages", "delete", "--revision", "3", "--id", pageID)
	call("tasks", "delete", "--revision", "4", "--id", taskID)
	if got := call("show"); got["revision"] != float64(5) {
		t.Fatal("write sequence did not persist")
	}
	// The capability authorizes the agent allowlist, not browser bootstrap.
	if _, err := agentRequest("GET", "/agent/launch", nil); err == nil {
		t.Fatal("agent obtained a browser login")
	}
	for _, args := range [][]string{{"pages", "create", "--title", "Missing revision", "--html-file", file}, {"tasks", "update", "--revision", "5", "--id", "../escape", "--title", "x"}, {"pages", "create", "--revision", "5", "--title", "x", "--html-file", file, "--status", "done"}} {
		if _, err := captureAgentCLI(t, workspaceCommand, args...); err == nil {
			t.Fatal("invalid CLI options accepted", args)
		}
	}
}

func TestAgentCLIDoesNotReplayUnknownCreatesOrExposeCapability(t *testing.T) {
	dir := agentCLIController(t)
	// Switch to a separately owned listener that loses the reply after dispatch.
	other, err := os.MkdirTemp("", "relay-lost-agent-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(other)
	if err := prepareControllerDirectory(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(other, "agent-capabilities"), 0700); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("RELAY_AGENT_CAPABILITY_FILE")
	secret, _ := os.ReadFile(oldPath)
	path := filepath.Join(other, "agent-capabilities", filepath.Base(oldPath))
	if err := os.WriteFile(path, secret, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_CONTROLLER_STATE", other)
	t.Setenv("RELAY_AGENT_CAPABILITY_FILE", path)
	listener, cleanup, err := listenControllerControl(other)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}), ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	defer server.Close()
	id := strings.Repeat("b", 32)
	_, err = captureAgentCLI(t, workspaceCommand, "tasks", "create", "--revision", "0", "--id", id, "--title", "once")
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "not retried") || calls.Load() != 1 {
		t.Fatal("unknown create lost identity or repeated", err, calls.Load())
	}
	var cap struct {
		Token string `json:"token"`
	}
	json.Unmarshal(secret, &cap)
	if strings.Contains(err.Error(), cap.Token) || strings.Contains(err.Error(), dir) {
		t.Fatal("request error leaked private credentials/state")
	}
}

func TestAgentCLIFleetUsesControllerConnectionsAndRequiresExactNames(t *testing.T) {
	dir := agentCLIController(t)
	// No saved personal hosts in this fixture. Exact-name resolution must fail,
	// never silently fall through to SSH or the default local runtime.
	_, err := captureAgentCLI(t, sessionCommand, "start", "--host", "Missing", "--harness", "codex", "--cwd", dir)
	if err == nil || !strings.Contains(err.Error(), "unambiguous") {
		t.Fatal("missing host was not rejected", err)
	}
	_, err = captureAgentCLI(t, sessionCommand, "list")
	if err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatal("missing host defaulted to a different runtime", err)
	}
	data, err := captureAgentCLI(t, fleetCommand, "list")
	if err != nil {
		t.Fatal(err)
	}
	var fleet struct {
		Hosts []any `json:"hosts"`
	}
	if json.Unmarshal(data, &fleet) != nil || len(fleet.Hosts) != 0 {
		t.Fatal("unexpected fleet", strconv.Quote(string(data)))
	}
}
