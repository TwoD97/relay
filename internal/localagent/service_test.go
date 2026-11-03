package localagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("LOCALAGENT_CRASH_HOST") == "1" {
		os.Exit(crashHostFixture())
	}
	if os.Getenv("LOCALAGENT_PROVIDER_FIXTURE") == "1" {
		os.Exit(providerFixture())
	}
	os.Exit(m.Run())
}
func providerFixture() int {
	for _, arg := range os.Args[1:] {
		if arg == "--help" {
			fmt.Println("[PROMPT] --append-system-prompt --dangerously-skip-permissions --dangerously-bypass-approvals-and-sandbox")
			return 0
		}
		if arg == "--version" {
			fmt.Println("fixture 1.0.0")
			return 0
		}
		if arg == "--child" {
			fmt.Println("CHILD", os.Getpid())
			time.Sleep(2 * time.Minute)
			return 0
		}
	}
	if len(os.Args) > 2 && (os.Args[1] == "auth" || os.Args[1] == "login") && os.Args[2] == "status" {
		return 0
	}
	fmt.Println("READY", os.Getpid())
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "exit "):
			code, _ := strconv.Atoi(strings.TrimPrefix(line, "exit "))
			return code
		case line == "size":
			cols, rows := fixtureConsoleSize()
			fmt.Printf("SIZE %d %d\n", cols, rows)
		case line == "spawn":
			exe, _ := os.Executable()
			child := exec.Command(exe, "--child")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				fmt.Println("SPAWN ERROR", err)
			}
		case line == "inspect":
			cwd, _ := os.Getwd()
			b, _ := json.Marshal(map[string]any{"args": os.Args[1:], "cwd": cwd, "mode": os.Getenv("RELAY_AGENT_MODE"), "controller": os.Getenv("RELAY_CONTROLLER_STATE")})
			fmt.Println("INSPECT", string(b))
		default:
			fmt.Println("RESULT", line)
		}
	}
	return 0
}
func testService(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("LOCALAGENT_PROVIDER_FIXTURE", "1")
	s, err := New(filepath.Join(t.TempDir(), "state"), "test")
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	s.findHarness = func(string) (string, error) { return exe, nil }
	httpServer := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, httpServer
}
func requestJSON(t *testing.T, url, method string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	request, _ := http.NewRequest(method, url, bytes.NewReader(data))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}
func createFixture(t *testing.T, s *Server, server *httptest.Server, mode string) Session {
	t.Helper()
	q := CreateRequest{MastermindID: id(), Harness: "claude", AgentMode: mode, Cwd: t.TempDir(), Prompt: "Literal instruction: spaces & ; \"quoted\" 日本語", Env: map[string]string{"RELAY_CONTROLLER_STATE": s.stateDir}}
	status, data := requestJSON(t, server.URL+"/api/sessions", "POST", q)
	if status != 201 {
		t.Fatalf("create %d: %s", status, data)
	}
	var result Session
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	awaitHistory(t, s, result.ID, "READY")
	return result
}
func awaitHistory(t *testing.T, s *Server, id, contains string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		p, err := s.lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		p.mu.Lock()
		data := string(p.history)
		p.mu.Unlock()
		if strings.Contains(data, contains) {
			return data
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := s.lookup(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("history missing %q: %q", contains, string(p.history))
	return ""
}
func sendFixture(t *testing.T, s *Server, session Session, input string) {
	t.Helper()
	p, err := s.lookup(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.input(nil, input+"\r"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalMastermindIdentityLiteralLaunchAndPersistence(t *testing.T) {
	s, httpServer := testService(t)
	cwd := filepath.Join(t.TempDir(), "space & literal")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	q := CreateRequest{MastermindID: id(), Harness: "claude", AgentMode: "auto", Cwd: cwd, Prompt: "literal 'prompt' & 日本語", Env: map[string]string{"RELAY_CONTROLLER_STATE": s.stateDir}}
	status, data := requestJSON(t, httpServer.URL+"/api/sessions", "POST", q)
	if status != 201 {
		t.Fatalf("create %d: %s", status, data)
	}
	var session Session
	json.Unmarshal(data, &session)
	awaitHistory(t, s, session.ID, "READY")
	status, data = requestJSON(t, httpServer.URL+"/api/sessions", "POST", q)
	if status != 200 {
		t.Fatalf("repeat %d: %s", status, data)
	}
	var repeated Session
	json.Unmarshal(data, &repeated)
	if repeated.ID != session.ID {
		t.Fatal("duplicate process created")
	}
	q.Prompt = "different"
	status, _ = requestJSON(t, httpServer.URL+"/api/sessions", "POST", q)
	if status != 409 {
		t.Fatalf("identity reuse %d", status)
	}
	sendFixture(t, s, session, "inspect")
	history := awaitHistory(t, s, session.ID, "INSPECT")
	for _, want := range []string{"--dangerously-skip-permissions", "literal 'prompt'", "日本語", `"mode":"auto"`} {
		if !strings.Contains(history, want) {
			t.Fatalf("literal fixture lost %q in %q", want, history)
		}
	}
	sendFixture(t, s, session, "exit 7")
	p, _ := s.lookup(session.ID)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("exit timeout")
	}
	if code := p.snapshot().ExitCode; code == nil || *code != 7 {
		t.Fatalf("exit code %v", code)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(s.stateDir, "test-new")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	old, _ := restored.lookup(session.ID)
	if old.snapshot().Status != "exited" {
		t.Fatal("completed session lost")
	}
	awaitHistory(t, restored, session.ID, "INSPECT")
}
func TestLocalRejectsUnscopedSessionsAndEnvironment(t *testing.T) {
	_, server := testService(t)
	for _, q := range []CreateRequest{{Harness: "shell", MastermindID: id()}, {Harness: "claude"}, {Harness: "claude", MastermindID: id(), Env: map[string]string{"ANTHROPIC_API_KEY": "secret"}}, {Harness: "claude", MastermindID: id(), Env: map[string]string{"RELAY_BIN": "relative"}}} {
		status, _ := requestJSON(t, server.URL+"/api/sessions", "POST", q)
		if status != 400 {
			t.Fatalf("rejected request %d", status)
		}
	}
}
func TestLocalTerminalLeaseResizeAndReconnect(t *testing.T) {
	s, server := testService(t)
	session := createFixture(t, s, server, "manual")
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/sessions/" + session.ID + "/terminal"
	owner, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	viewer, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	p, _ := s.lookup(session.ID)
	if err = owner.WriteJSON(map[string]string{"type": "claim"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		hasOwner := p.owner != nil
		p.mu.Unlock()
		if hasOwner {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no owner")
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, _ := requestJSON(t, server.URL+"/api/sessions/"+session.ID+"/input", "POST", map[string]string{"data": "DENIED\r"})
	if status != 409 {
		t.Fatalf("HTTP lease %d", status)
	}
	owner.WriteJSON(map[string]any{"type": "resize", "cols": 132, "rows": 41})
	owner.WriteJSON(map[string]string{"type": "input", "data": "size\r"})
	awaitHistory(t, s, session.ID, "SIZE 132 41")
	owner.Close()
	deadline = time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		available := p.owner == nil
		p.mu.Unlock()
		if available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sendFixture(t, s, session, "after reconnect")
	awaitHistory(t, s, session.ID, "RESULT after reconnect")
}
func TestLocalLoginHistoryNeverPersists(t *testing.T) {
	s, _ := testService(t)
	exe, _ := os.Executable()
	meta, err := s.start(Session{Title: "Sign in", Workspace: "Setup", Cwd: t.TempDir(), Harness: "claude", Purpose: "login"}, "", 80, 24, command{Path: exe, Dir: t.TempDir(), Env: s.environment(nil)})
	if err != nil {
		t.Fatal(err)
	}
	awaitHistory(t, s, meta.ID, "READY")
	sendFixture(t, s, meta, "ephemeral secret fixture")
	awaitHistory(t, s, meta.ID, "RESULT ephemeral")
	s.mu.Lock()
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.stateDir, "history", meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("login persisted: %v", err)
	}
	sendFixture(t, s, meta, "exit 0")
	p, _ := s.lookup(meta.ID)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.history) != 0 || p.screen != nil {
		t.Fatal("completed login output retained")
	}
}
func TestLocalProbeCancellationOwnsChild(t *testing.T) {
	t.Setenv("LOCALAGENT_PROVIDER_FIXTURE", "1")
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runProbe(ctx, exe, nil, os.Environ())
	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("probe cancellation %v after %v", err, time.Since(started))
	}
}

func TestLocalProcessTreeCleanupPreservesUnrelatedProcess(t *testing.T) {
	t.Setenv("LOCALAGENT_PROVIDER_FIXTURE", "1")
	exe, _ := os.Executable()
	sentinel := exec.Command(exe, "--child")
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() }()
	aliveSentinel, closeSentinel, err := pinFixtureProcess(sentinel.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSentinel()
	s, server := testService(t)
	session := createFixture(t, s, server, "manual")
	sendFixture(t, s, session, "spawn")
	history := awaitHistory(t, s, session.ID, "CHILD ")
	childID := 0
	for _, line := range strings.Split(history, "\n") {
		if start := strings.Index(line, "CHILD "); start >= 0 {
			fmt.Sscanf(line[start:], "CHILD %d", &childID)
		}
	}
	if childID <= 0 {
		t.Fatalf("no child ID: %q", history)
	}
	aliveChild, closeChild, err := pinFixtureProcess(childID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeChild()
	p, _ := s.lookup(session.ID)
	p.stop(false)
	until := time.Now().Add(3 * time.Second)
	for aliveChild() && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
	}
	if aliveChild() {
		t.Fatal("owned child survived session cleanup")
	}
	if !aliveSentinel() {
		t.Fatal("unrelated sentinel was terminated")
	}
}

func TestLocalProviderProbe(t *testing.T) {
	t.Setenv("LOCALAGENT_PROVIDER_FIXTURE", "1")
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := runProbe(ctx, exe, []string{"--help"}, os.Environ())
	if err != nil {
		t.Fatalf("native probe: %v output %q", err, data)
	}
	if !strings.Contains(string(data), "--append-system-prompt") {
		t.Fatalf("truncated probe output %q", data)
	}
}
