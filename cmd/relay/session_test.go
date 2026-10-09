//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimeServer "github.com/TwoD97/relay/internal/runtime"
	"github.com/gorilla/websocket"
)

// These CLI tests remain sequential because the command writes to os.Stdout.
// A temporary file avoids a pipe filling when read returns a large history.
func runSessionCLI(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "cli-output-")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = previous }()
	err = sessionCommand(args)
	if closeErr := output.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	data, readErr := os.ReadFile(output.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return data, err
}

func sessionCLISocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hn-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve disposable runtime: %v", err)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

func TestSessionCLIRealShellAndExclusiveBrowserControl(t *testing.T) {
	// Keep the user's HOME intact; a basic POSIX shell avoids interactive
	// startup customizations and never starts a provider session.
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	stateDir := t.TempDir()
	runtime, err := runtimeServer.New(stateDir, "cli-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	socket := sessionCLISocket(t, runtime.Handler())
	t.Setenv("RELAY_SOCKET", socket)

	data, err := runSessionCLI(t, "start", "--cwd", stateDir, "--title", "CLI integration", "--workspace", "Disposable tests")
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		ID, Title, Workspace, Cwd, Harness, Status string
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("invalid start JSON: %v: %s", err, data)
	}
	if len(session.ID) != 32 || session.Title != "CLI integration" || session.Workspace != "Disposable tests" || session.Cwd != stateDir || session.Harness != "shell" || session.Status != "running" {
		t.Fatalf("unexpected session: %+v", session)
	}
	data, err = runSessionCLI(t, "list")
	if err != nil || !strings.Contains(string(data), session.ID) {
		t.Fatalf("list did not include the new session: %v: %s", err, data)
	}

	// The marker is split in the typed command, so only executed output can
	// contain the complete expected value; terminal echo cannot satisfy it.
	if _, err := runSessionCLI(t, "send", "--id", session.ID, "--text", "printf 'RELAY_%s\\n' 'CLI_EXECUTED'", "--enter"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err = runSessionCLI(t, "read", "--id", session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "RELAY_CLI_EXECUTED") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell did not execute the command: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}

	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	browser, _, err := dialer.Dial("ws://runtime/api/sessions/"+session.ID+"/terminal", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	if err := browser.WriteJSON(map[string]string{"type": "claim"}); err != nil {
		t.Fatal(err)
	}
	waitControl := func(owner bool) {
		t.Helper()
		_ = browser.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			kind, data, err := browser.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var control struct {
				Type  string `json:"type"`
				Owner bool   `json:"owner"`
			}
			if kind == websocket.TextMessage && json.Unmarshal(data, &control) == nil && control.Type == "control" && control.Owner == owner {
				return
			}
		}
	}
	waitControl(true)
	if _, err := runSessionCLI(t, "send", "--id", session.ID, "--text", "printf 'MUST_NOT_RUN'", "--enter"); err == nil || !strings.Contains(err.Error(), "another viewer") {
		t.Fatalf("CLI input bypassed the browser lease: %v", err)
	}
	data, err = runSessionCLI(t, "read", "--id", session.ID)
	if err != nil || strings.Contains(string(data), "MUST_NOT_RUN") {
		t.Fatalf("rejected input reached the PTY: %v: %s", err, data)
	}
	if err := browser.WriteJSON(map[string]string{"type": "release"}); err != nil {
		t.Fatal(err)
	}
	waitControl(false)
	if _, err := runSessionCLI(t, "send", "--id", session.ID, "--text", "printf 'after-release\\n'", "--enter"); err != nil {
		t.Fatalf("CLI could not send after release: %v", err)
	}
	if _, err := runSessionCLI(t, "stop", "--id", session.ID); err != nil {
		t.Fatal(err)
	}
	data, err = runSessionCLI(t, "list")
	if err != nil || strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("stop did not remove the session: %v: %s", err, data)
	}
}

func TestSessionCLISendsLiteralTextAndNeverRetriesRejectedInput(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	const literal = "literal $(touch /not-executed) `date` \\\n\"quoted\""
	var calls atomic.Int32
	receivedText := make(chan string, 1)
	socket := sessionCLISocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/sessions/"+id+"/input" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected input request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		receivedText <- body["data"]
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"another viewer has terminal control"}`)
	}))
	_, err := runSessionCLI(t, "send", "--socket", socket, "--id", id, "--text", literal, "--enter")
	if err == nil || !strings.Contains(err.Error(), "another viewer") {
		t.Fatalf("lost API error: %v", err)
	}
	var received string
	select {
	case received = <-receivedText:
	case <-time.After(time.Second):
		t.Fatal("input request did not reach the runtime")
	}
	if received != literal+"\r" || calls.Load() != 1 {
		t.Fatalf("input changed or was retried: %q, calls=%d", received, calls.Load())
	}
}

func TestSessionCLIRejectsInvalidIdentifiersAndInputBeforeConnecting(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{
		{"read", "--id", "../escape"},
		{"stop", "--id", "short"},
		{"send", "--id", id},
		{"send", "--id", id, "--text", strings.Repeat("x", 64<<10), "--enter"},
		{"send", "--id", id, "unexpected-positional-text"},
		{"unknown"},
	} {
		_, err := runSessionCLI(t, append(args, "--socket", "/tmp/nonexistent-relay-cli-validation.sock")...)
		if err == nil || strings.Contains(err.Error(), "runtime request failed") {
			t.Fatalf("invalid arguments reached transport: %v: %v", args[:1], err)
		}
	}
}
