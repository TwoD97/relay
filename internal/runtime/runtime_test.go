//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/gorilla/websocket"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func startTest(t *testing.T, s *Server, script string) *liveSession {
	t.Helper()
	meta, err := s.start(Session{Title: "Test", Workspace: "Tests", Cwd: s.stateDir, Harness: "shell"}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) { return exec.Command("/bin/sh", "-c", script), nil })
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.lookup(meta.ID)
	return p
}
func historyOf(p *liveSession) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.history.bytes())
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func ready(t *testing.T, p *liveSession) {
	t.Helper()
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.inputReady || p.finished })
	if p.snapshot().Status != "running" {
		t.Fatalf("PTY failed: %s", historyOf(p))
	}
}
func TestPTYDisconnectAndReconnect(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "printf 'ready-marker\\n'; IFS= read -r value; printf 'reply:%s\\n' \"$value\"")
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/sessions/" + p.meta.ID + "/terminal"
	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(historyOf(p), "ready-marker") })
	conn.Close()
	if p.snapshot().Status != "running" {
		t.Fatal("disconnect stopped PTY")
	}
	conn, _, err = websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.TextMessage {
		t.Fatal("geometry must precede terminal snapshot")
	}
	typ, data, err = conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.BinaryMessage || !strings.Contains(string(data), "ready-marker") {
		t.Fatalf("missing replay: %s", data)
	}
	if err = conn.WriteJSON(map[string]any{"type": "claim"}); err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteJSON(map[string]any{"type": "input", "data": "survived\n"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(historyOf(p), "reply:survived") })
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit")
	}
	if p.snapshot().Status != "exited" || *p.snapshot().ExitCode != 0 {
		t.Fatalf("wrong process status: %+v", p.snapshot())
	}
	if err = p.writeInput("must-not-replay"); err == nil {
		t.Fatal("accepted input for exited process")
	}
}
func TestHistoryBoundAndBackpressure(t *testing.T) {
	p := newLiveSession(Session{})
	_, slow, _ := p.subscribe()
	_, fast, _ := p.subscribe()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	var all []byte
	for i := 0; i < 50; i++ {
		payload[0] = byte(i)
		_, _ = p.Write(payload)
		all = append(all, payload...)
		select {
		case <-fast.frames:
		default:
			t.Fatal("fast subscriber stalled")
		}
	}
	select {
	case <-slow.done:
	default:
		t.Fatal("slow subscriber was not disconnected")
	}
	if got := p.history.bytes(); !bytes.Equal(got, all[len(all)-maxHistory:]) {
		t.Fatal("ring does not preserve exact bounded suffix")
	}
	if p.history.size != maxHistory {
		t.Fatal("unbounded history")
	}
}
func TestRestartMarksRunningInterruptedAndKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := startTest(t, s, "printf 'durable-marker\\n'; sleep 30")
	eventually(t, func() bool { return strings.Contains(historyOf(p), "durable-marker") })
	// Model an abrupt daemon loss without running a second owner concurrently.
	saved := p.snapshot()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(storedState{Protocol: 1, Sessions: []Session{saved}})
	if err = atomicPrivateWrite(filepath.Join(dir, "sessions.json"), state); err != nil {
		t.Fatal(err)
	}
	restored, err := New(dir, "new")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	old, _ := restored.lookup(saved.ID)
	if old.snapshot().Status != "interrupted" {
		t.Fatal("claimed restarted process still running")
	}
	if !strings.Contains(historyOf(old), "durable-marker") {
		t.Fatal("history lost")
	}
	if err = old.writeInput("x"); err == nil {
		t.Fatal("interrupted process accepted input")
	}
	info, err := os.Stat(filepath.Join(dir, "sessions.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("metadata file is not private")
	}
}
func TestCloseTerminatesSessionChildren(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "trap '' TERM; sleep 60 & printf 'child:%s\\n' \"$!\"; wait")
	eventually(t, func() bool { return strings.Contains(historyOf(p), "child:") })
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(historyOf(p)), "child:%d", &pid)
	if pid <= 1 {
		t.Fatal("missing child PID")
	}
	start := time.Now()
	p.stop(true)
	if time.Since(start) > 4*time.Second {
		t.Fatal("shutdown exceeded escalation bound")
	}
	eventually(t, func() bool {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return true
		}
		end := strings.LastIndexByte(string(data), ')')
		return end >= 0 && strings.HasPrefix(string(data[end+1:]), " Z ")
	})
	if p.snapshot().Status != "interrupted" {
		t.Fatal("shutdown not recorded")
	}
}
func TestInputHasDeadline(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "stty -icanon -echo; printf ready; sleep 30")
	eventually(t, func() bool { return strings.Contains(historyOf(p), "ready") })
	start := time.Now()
	err := p.writeInput(strings.Repeat("x", maxInput))
	if err == nil {
		t.Fatal("expected saturated terminal write to time out")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("input write hung")
	}
}
func request(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}
func TestAPIRejectsUnsafeAndOversizedInput(t *testing.T) {
	s := testServer(t)
	h := s.Handler()
	cases := []string{`{"harness":"sh; touch /tmp/pwn"}`, `{"cwd":"relative"}`, `{"title":"line\ncontrol"}`, `{"workspace":"bad\u001b"}`, `{"harness":"shell","executable":"rm"}`, `{"cols":10000}`, `{} {}`}
	for _, body := range cases {
		w := request(t, h, "POST", "/api/sessions", body)
		if w.Code < 400 {
			t.Errorf("accepted invalid body %s: %d", body, w.Code)
		}
	}
	w := request(t, h, "POST", "/api/sessions", `{"title":"`+strings.Repeat("x", 100<<10)+`"}`)
	if w.Code < 400 {
		t.Fatal("accepted oversized request")
	}
	for _, id := range []string{"shell", "other", "codex;id"} {
		if w := request(t, h, "POST", "/api/harnesses/"+id+"/login", ""); w.Code != 404 {
			t.Errorf("harness %q: %d", id, w.Code)
		}
	}
}
func TestWebsocketOriginRejected(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "sleep 30")
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/sessions/"+p.meta.ID+"/terminal", http.Header{"Origin": []string{"https://evil.example"}})
	if conn != nil {
		conn.Close()
	}
	if err == nil || response.StatusCode != 403 {
		t.Fatal("cross origin terminal allowed")
	}
}
func TestResourceCapAndDeletion(t *testing.T) {
	s := testServer(t)
	for i := 0; i < maxSessions; i++ {
		p := startTest(t, s, "true")
		<-p.done
	}
	_, err := s.start(Session{Title: "over", Workspace: "x", Cwd: s.stateDir, Harness: "shell"}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) { return exec.Command("true"), nil })
	if err == nil {
		t.Fatal("resource cap not enforced")
	}
	var id string
	for key := range s.sessions {
		id = key
		break
	}
	w := request(t, s.Handler(), "DELETE", "/api/sessions/"+id, "")
	if w.Code != 204 {
		t.Fatalf("delete: %s", w.Body.String())
	}
	if _, err = s.lookup(id); !os.IsNotExist(err) {
		t.Fatal("deleted session retained")
	}
	p := startTest(t, s, "true")
	<-p.done
}
func TestSymlinkStateRejected(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	_ = os.Mkdir(target, 0700)
	link := filepath.Join(parent, "link")
	_ = os.Symlink(target, link)
	if s, err := New(link, "test"); err == nil {
		s.Close()
		t.Fatal("accepted symlink state dir")
	}
}
func archive(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}
func TestNodeArchiveRejectsEscape(t *testing.T) {
	cases := []*tar.Header{
		{Name: "../escaped", Typeflag: tar.TypeReg, Size: 1},
		{Name: "node/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../../../outside"},
		{Name: "node/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	}
	for _, h := range cases {
		if err := extractNode(bytes.NewReader(archive(t, []*tar.Header{h})), t.TempDir(), "node"); err == nil {
			t.Errorf("accepted dangerous header %+v", h)
		}
	}
}
func TestNodeArchiveExtractsPrivateExecutablesAndSafeLinks(t *testing.T) {
	headers := []*tar.Header{{Name: "node/bin/node", Typeflag: tar.TypeReg, Size: 1, Mode: 0755}, {Name: "node/lib/npm.js", Typeflag: tar.TypeReg, Size: 1, Mode: 0755}, {Name: "node/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../lib/npm.js"}}
	dir := t.TempDir()
	if err := extractNode(bytes.NewReader(archive(t, headers)), dir, "node"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "node/bin/node"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("unsafe executable permissions")
	}
	if _, err = os.ReadFile(filepath.Join(dir, "node/bin/npm")); err != nil {
		t.Fatal(err)
	}
}
func TestAuthenticationOutputNotReturned(t *testing.T) {
	s := testServer(t)
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex 0.1'; else echo 'sensitive-auth-value'; exit 0; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	h := s.inspectHarness(context.Background(), "codex")
	data, _ := json.Marshal(h)
	if strings.Contains(string(data), "sensitive") {
		t.Fatal("auth output disclosed")
	}
	if h.Authenticated == nil || !*h.Authenticated {
		t.Fatal("successful status not detected")
	}
}

func TestResizePreservesInputDeadline(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "stty -icanon -echo; printf ready; sleep 30")
	eventually(t, func() bool { return strings.Contains(historyOf(p), "ready") })
	if err := p.resize(110, 40); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.writeInput(strings.Repeat("x", maxInput)); err == nil {
		t.Fatal("expected saturated terminal write to time out")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("resize disabled nonblocking input")
	}
}
func TestExclusiveControlLifecycle(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "printf ready; IFS= read -r value; printf 'got:%s' \"$value\"; sleep 30")
	ready(t, p)
	_, a, _ := p.subscribe()
	_, b, _ := p.subscribe()
	initial := latestControl(t, a)
	if initial.Owner || !initial.Available {
		t.Fatal("attach automatically took control")
	}
	if err := p.writeInputControlled(a, "BAD"); err == nil {
		t.Fatal("observer wrote input")
	}
	if err := p.resizeControlled(a, 200, 40); err == nil {
		t.Fatal("observer resized shared terminal")
	}
	p.claimControl(a)
	if state := latestControl(t, a); !state.Owner || state.Available {
		t.Fatal("claim failed")
	}
	p.claimControl(b)
	if state := latestControl(t, b); state.Owner || state.Available {
		t.Fatal("claim stole control")
	}
	if err := p.writeInput("BAD"); err == nil {
		t.Fatal("HTTP-style input bypassed owner")
	}
	if err := p.writeInputControlled(a, "good\n"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(historyOf(p), "got:good") })
	p.mu.Lock()
	p.lastInput = time.Now().Add(-61 * time.Second)
	p.mu.Unlock()
	p.expireControl()
	p.claimControl(b)
	if state := latestControl(t, b); !state.Owner {
		t.Fatal("expired lease still blocked claim")
	}
	p.unsubscribe(b)
	p.claimControl(a)
	if state := latestControl(t, a); !state.Owner {
		t.Fatal("disconnect did not release lease")
	}
	p.releaseControl(a)
	if state := latestControl(t, a); state.Owner || !state.Available {
		t.Fatal("release failed")
	}
	if strings.Contains(historyOf(p), "BAD") {
		t.Fatal("denied input reached PTY")
	}
}
func TestHookEventCapabilityAndAttention(t *testing.T) {
	s := testServer(t)
	meta, err := s.start(Session{Title: "Agent", Workspace: "Tests", Cwd: s.stateDir, Harness: "claude"}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", "sleep 30"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.lookup(meta.ID)
	ready(t, p)
	event := func(token, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/sessions/"+meta.ID+"/events", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := event("wrong", `{"kind":"permission","source":"claude-hook"}`); w.Code != 401 {
		t.Fatalf("unauthorized event: %d", w.Code)
	}
	if w := event(p.eventToken, `{"kind":"permission","source":"codex-notify"}`); w.Code != 409 {
		t.Fatal("wrong provider event accepted")
	}
	if w := event(p.eventToken, `{"kind":"permission","source":"claude-hook"}`); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if p.snapshot().Attention == nil || p.snapshot().Attention.Kind != "permission" {
		t.Fatal("hook signal not represented")
	}
	encoded, _ := json.Marshal(p.snapshot())
	if strings.Contains(string(encoded), p.eventToken) {
		t.Fatal("event token leaked into metadata")
	}
	if w := event(p.eventToken, `{"kind":"working","source":"claude-hook"}`); w.Code != 204 || p.snapshot().Attention != nil {
		t.Fatal("working hook did not clear attention")
	}
	if w := event(p.eventToken, `{"kind":"completed","source":"claude-hook"}`); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if w := request(t, s.Handler(), "POST", "/api/sessions/"+meta.ID+"/attention/ack", ""); w.Code != 204 || p.snapshot().Attention != nil {
		t.Fatal("ack did not clear attention")
	}
	data, err := os.ReadFile(filepath.Join(s.stateDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(p.eventToken)) {
		t.Fatal("capability persisted")
	}
}
func TestNodeArchiveRejectsSymlinkParentChain(t *testing.T) {
	headers := []*tar.Header{{Name: "node", Typeflag: tar.TypeDir}, {Name: "node/a", Typeflag: tar.TypeSymlink, Linkname: "."}, {Name: "node/a/b", Typeflag: tar.TypeSymlink, Linkname: ".."}}
	if err := extractNode(bytes.NewReader(archive(t, headers)), t.TempDir(), "node"); err == nil {
		t.Fatal("accepted symlink parent chain")
	}
}
func TestProbeOutputIsBoundedThroughIOCopy(t *testing.T) {
	b := &boundedOutput{limit: 100}
	data := strings.Repeat("x", 1<<20)
	if _, err := io.Copy(b, io.LimitReader(strings.NewReader(data), int64(len(data)))); err != nil {
		t.Fatal(err)
	}
	if b.buffer.Len() != 100 {
		t.Fatal("probe output bypassed bound")
	}
}
func TestCancelledNodePreparationDoesNotWaitForOtherInstall(t *testing.T) {
	s := testServer(t)
	s.toolsMu <- struct{}{}
	defer func() { <-s.toolsMu }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ensureNPM(ctx, io.Discard); err == nil {
		t.Fatal("cancelled installer continued")
	}
}

func TestDisconnectedViewerCannotReclaimControl(t *testing.T) {
	p := newLiveSession(Session{})
	_, sub, _ := p.subscribe()
	p.unsubscribe(sub)
	p.claimControl(sub)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner != nil {
		t.Fatal("detached reader acquired orphaned control lease")
	}
}

// The inherited environment intentionally remains real in this opt-in smoke.
// Only provider login status is requested; no inference or new login occurs.
func TestInstalledHarnessConfiguration(t *testing.T) {
	if os.Getenv("RELAY_LIVE_HARNESS_SMOKE") != "1" {
		t.Skip("set RELAY_LIVE_HARNESS_SMOKE=1 to verify installed provider CLIs")
	}
	s := testServer(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude", "codex"} {
		t.Run(id, func(t *testing.T) {
			path, err := s.findCommand(id)
			if err != nil {
				t.Skip("provider is not installed")
			}
			args := harnessArguments(id, executable)
			if id == "claude" {
				args = append(args, "auth", "status")
			} else {
				args = append(args, "login", "status")
			}
			if _, err = s.probe(context.Background(), path, args...); err != nil {
				t.Fatalf("provider rejected configuration or has no current login: %T", err)
			}
		})
	}
}

func BenchmarkFanout(b *testing.B) {
	for _, panes := range []int{1, 15} {
		for _, viewers := range []int{0, 1, 8} {
			b.Run(fmt.Sprintf("panes_%d/viewers_%d", panes, viewers), func(b *testing.B) {
				sessions := make([]*liveSession, panes)
				subscriptions := make([][]*subscriber, panes)
				chunk := bytes.Repeat([]byte("terminal output "), 2048)
				for i := range sessions {
					p := newLiveSession(Session{})
					p.history.append(bytes.Repeat([]byte("x"), maxHistory))
					sessions[i] = p
					p.screen = newTerminalModel(100, 30)
					for j := 0; j < viewers; j++ {
						_, sub, _ := p.subscribe()
						subscriptions[i] = append(subscriptions[i], sub)
					}
				}
				b.ReportAllocs()
				b.SetBytes(int64(panes * len(chunk)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for j, p := range sessions {
						_, _ = p.Write(chunk)
						for _, sub := range subscriptions[j] {
							<-sub.frames
						}
					}
				}
			})
		}
	}
}

func latestControl(t *testing.T, sub *subscriber) controlMessage {
	t.Helper()
	var result *controlMessage
	for {
		select {
		case f := <-sub.frames:
			if f.Control != nil {
				result = f.Control
			}
		default:
			if result == nil {
				t.Fatal("no control frame")
			}
			return *result
		}
	}
}

func TestTerminalSnapshotSurvivesTruncatedAlternateScreenHistory(t *testing.T) {
	p := newLiveSession(Session{})
	p.cols = 80
	p.rows = 24
	_, _ = p.Write([]byte("main-buffer-marker\x1b[?1049h\x1b[2J\x1b[1;1H\x1b[1;31magent-header\x1b[0m"))
	_, _ = p.Write(bytes.Repeat([]byte("\x1b[2;2H."), 150000))
	_, _ = p.Write([]byte("\x1b[?2004h\x1b[?1006h\x1b[?1000h\x1b[?25l\x1b[6 q\x1b[38;2;120;150;200m\x1b[7;9H界"))
	if bytes.Contains(p.history.bytes(), []byte("agent-header")) {
		t.Fatal("fixture did not evict initial screen state")
	}
	replay, sub, err := p.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer p.unsubscribe(sub)
	restored := newTerminalModel(80, 24)
	restored.write(replay)
	if !restored.emulator.IsAltScreen() {
		t.Fatal("alternate screen mode was lost")
	}
	compareScreens(t, p.screen, restored)
	if restored.cursorVisible || restored.cursorStyle != 6 || !restored.modes[ansi.DECMode(2004)] || !restored.modes[ansi.DECMode(1006)] {
		t.Fatal("cursor or input mode was lost")
	}
	// Future raw output must continue with the prior RGB pen and exact cursor.
	p.screen.write([]byte("Q"))
	restored.write([]byte("Q"))
	compareScreens(t, p.screen, restored)
	p.screen.write([]byte("\x1b[?1049l"))
	restored.write([]byte("\x1b[?1049l"))
	compareScreens(t, p.screen, restored)
}
func compareScreens(t *testing.T, a, b *terminalModel) {
	t.Helper()
	if a.emulator.String() != b.emulator.String() {
		t.Fatalf("screen mismatch\nexpected=%q\nactual=%q", a.emulator.String(), b.emulator.String())
	}
	if a.emulator.CursorPosition() != b.emulator.CursorPosition() {
		t.Fatalf("cursor mismatch expected=%v actual=%v", a.emulator.CursorPosition(), b.emulator.CursorPosition())
	}
	for y := 0; y < a.emulator.Height(); y++ {
		for x := 0; x < a.emulator.Width(); x++ {
			ca, cb := a.emulator.CellAt(x, y), b.emulator.CellAt(x, y)
			if !ca.Equal(cb) {
				t.Fatalf("styled cell mismatch at %d,%d: %#v != %#v", x, y, ca, cb)
			}
		}
	}
}
func TestTerminalQueriesDoNotBlockOrInjectInput(t *testing.T) {
	m := newTerminalModel(80, 24)
	done := make(chan struct{})
	go func() { m.write([]byte("\x1b[6n\x1b[c\x1b]10;?\x07\x1b[?2026$p")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("query response blocked output parser")
	}
}
func TestGeometryAndOutputShareOneOrderedQueue(t *testing.T) {
	p := newLiveSession(Session{})
	_, sub, _ := p.subscribe()
	<-sub.frames
	_, _ = p.Write([]byte("before"))
	if err := p.resize(120, 40); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Write([]byte("after"))
	first, second, third := <-sub.frames, <-sub.frames, <-sub.frames
	if string(first.Data) != "before" || second.Control == nil || second.Control.Cols != 120 || second.Control.Rows != 40 || string(third.Data) != "after" {
		t.Fatal("geometry was not ordered between old/new output")
	}
}
func TestLoginHistoryIsEphemeral(t *testing.T) {
	s := testServer(t)
	meta, err := s.start(Session{Title: "Login", Workspace: "Setup", Cwd: s.stateDir, Harness: "codex", Purpose: "login"}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", "printf ephemeral-device-code; read value"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.lookup(meta.ID)
	eventually(t, func() bool { return strings.Contains(historyOf(p), "ephemeral-device-code") })
	s.mu.Lock()
	err = s.flushHistoryLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.historyPath(meta.ID)); !os.IsNotExist(err) {
		t.Fatal("login transcript was written to disk")
	}
	if err = p.writeInput("done\n"); err != nil {
		t.Fatal(err)
	}
	<-p.done
	if historyOf(p) != "" {
		t.Fatal("completed login retained device code")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(s.stateDir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	old, _ := restored.lookup(meta.ID)
	if historyOf(old) != "" {
		t.Fatal("login history survived restart")
	}
}

func TestAlternateSnapshotAfterShrinkGrowDropsClippedCells(t *testing.T) {
	m := newTerminalModel(80, 24)
	m.write([]byte("\x1b[1;60Hclipped-main-text\x1b[20;1Hclipped-main-row\x1b[1;1Hkept\x1b[?1049h\x1b[3;2Hagent"))
	m.resize(40, 12)
	m.resize(80, 24)
	restored := newTerminalModel(80, 24)
	restored.write(m.snapshot(nil))
	compareScreens(t, m, restored)
	m.write([]byte("\x1b[?1049l"))
	restored.write([]byte("\x1b[?1049l"))
	compareScreens(t, m, restored)
	if strings.Contains(restored.emulator.String(), "clipped") {
		t.Fatal("clipped primary buffer cells were resurrected")
	}
}
func TestMixedUnknownModesPreserveKnownMode(t *testing.T) {
	m := newTerminalModel(80, 24)
	m.write([]byte("\x1b[?25;9999l\x1b[?9998;2004h"))
	if m.cursorVisible || !m.modes[ansi.DECMode(2004)] {
		t.Fatal("unknown mode suppressed supported mode")
	}
	if _, ok := m.modes[ansi.DECMode(9999)]; ok {
		t.Fatal("unbounded unknown mode retained")
	}
}

func TestUnattendedTerminalReceivesCursorQueryReply(t *testing.T) {
	s := testServer(t)
	// Raw mode makes the cursor report readable immediately, without an Enter.
	p := startTest(t, s, "stty raw -echo; printf '\\033[6n'; answer=$(dd bs=1 count=6 2>/dev/null); if [ \"$answer\" = \"$(printf '\\033[1;1R')\" ]; then printf QUERY-ANSWERED; else printf WRONG-ANSWER; fi")
	eventually(t, func() bool { return strings.Contains(historyOf(p), "QUERY-ANSWERED") })
	<-p.done
	if *p.snapshot().ExitCode != 0 {
		t.Fatal("unattended query probe failed")
	}
}
func TestOwnerQueryIsNotAnsweredTwice(t *testing.T) {
	p := newLiveSession(Session{})
	_, sub, _ := p.subscribe()
	p.claimControl(sub)
	_, _ = p.Write([]byte("\x1b[6n"))
	select {
	case <-p.replies:
		t.Fatal("server replied while browser owned terminal")
	default:
	}
	p.releaseControl(sub)
	_, _ = p.Write([]byte("\x1b[6n"))
	select {
	case reply := <-p.replies:
		if string(reply) != "\x1b[1;1R" {
			t.Fatalf("unexpected cursor reply: %q", reply)
		}
	default:
		t.Fatal("unowned query had no reply")
	}
	before := len(p.replies)
	_, _, _ = p.subscribe()
	if len(p.replies) != before {
		t.Fatal("replay injected terminal input")
	}
}

func TestSnapshotPreservesIncompleteTerminalSequences(t *testing.T) {
	for _, tc := range []struct{ name, prefix, suffix string }{
		{"CSI", "\x1b[31", "mRED"},
		{"UTF8", string([]byte{0xe7}), string([]byte{0x95, 0x8c}) + "wide"},
		{"OSC", "\x1b]8;;https://example.", "org\x1b\\link\x1b]8;;\x1b\\"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLiveSession(Session{})
			_, _ = p.Write([]byte("before" + tc.prefix))
			snapshot, sub, err := p.subscribe()
			if err != nil {
				t.Fatal(err)
			}
			defer p.unsubscribe(sub)
			restored := newTerminalModel(int(p.cols), int(p.rows))
			restored.write(snapshot)
			_, _ = p.Write([]byte(tc.suffix))
			restored.write([]byte(tc.suffix))
			compareScreens(t, p.screen, restored)
		})
	}
}
func TestOversizedPendingSequenceHasBoundedRetry(t *testing.T) {
	p := newLiveSession(Session{})
	_, _ = p.Write([]byte("\x1b]0;" + strings.Repeat("x", 70<<10)))
	if len(p.screen.pending) > 64<<10 {
		t.Fatal("unbounded pending control prefix")
	}
	if _, _, err := p.subscribe(); err == nil {
		t.Fatal("attached with unrecoverable incomplete prefix")
	}
	_, _ = p.Write([]byte("\x07ready"))
	if _, sub, err := p.subscribe(); err != nil {
		t.Fatal(err)
	} else {
		p.unsubscribe(sub)
	}
}

func TestCursorReportUsesScrollOrigin(t *testing.T) {
	p := newLiveSession(Session{})
	_, _ = p.Write([]byte("\x1b[5;10r\x1b[?6h\x1b[2;3H\x1b[6n\x1b[?6n"))
	for _, expected := range []string{"\x1b[2;3R", "\x1b[?2;3;1R"} {
		select {
		case response := <-p.replies:
			if string(response) != expected {
				t.Fatalf("cursor report %q; want %q", response, expected)
			}
		default:
			t.Fatal("missing cursor report")
		}
	}
}

func TestInstallLeaseBlocksHarnessLaunch(t *testing.T) {
	s := testServer(t)
	s.mu.Lock()
	s.installing = map[string]bool{"codex": true}
	s.mu.Unlock()
	_, err := s.start(Session{Harness: "codex", Cwd: s.home}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) {
		t.Error("launched a harness during its installation")
		return exec.Command("/bin/true"), nil
	})
	if err == nil || !strings.Contains(err.Error(), "installation is in progress") {
		t.Fatalf("launch did not reject installation lease: %v", err)
	}
}

func TestManagedBrokenHarnessOffersInstallAgain(t *testing.T) {
	s := testServer(t)
	s.home = t.TempDir()
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(s.stateDir, "tools", "harnesses", "codex", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := s.inspectHarness(context.Background(), "codex")
	if h.Installed || !strings.Contains(h.AuthDetail, "Install again") {
		t.Fatalf("broken managed installation cannot be retried: %+v", h)
	}
}

func TestInstallEndpointRepairsInterruptedPackage(t *testing.T) {
	s := testServer(t)
	codexRegistryFixture(t, false, nil)
	s.home = t.TempDir()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	prefix := filepath.Join(s.stateDir, "tools", "harnesses", "codex")
	managed := filepath.Join(prefix, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(managed), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"node": "#!/bin/sh\nprintf 'v24.21.0\\n'\n",
		"npm": `#!/bin/sh
set -eu
if [ "$1" = --version ]; then printf '11.0.0\n'; exit 0; fi
[ "$1" = install ]
[ "$3" = --prefix ]
[ ! -e "$4/bin/codex" ]
/bin/mkdir -p "$4/bin"
printf '#!/bin/sh\nprintf "codex-cli 0.161.0\\n"\n' > "$4/bin/codex"
/bin/chmod 700 "$4/bin/codex"
`,
	}
	for name, script := range fixtures {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/harnesses/codex/install", nil))
	if w.Code != 201 {
		t.Fatalf("install status %d: %s", w.Code, w.Body.String())
	}
	var meta Session
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	p, err := s.lookup(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("repair did not finish")
	}
	if meta := p.snapshot(); meta.ExitCode == nil || *meta.ExitCode != 0 {
		t.Fatalf("repair failed: %s", historyOf(p))
	}
	if data, err := os.ReadFile(managed); err != nil || string(data) != "#!/bin/sh\nexit 1\n" {
		t.Fatal("repair modified the legacy release instead of preserving it")
	}
	h := s.inspectHarness(context.Background(), "codex")
	if !h.Installed || h.Version != "codex-cli 0.161.0" {
		t.Fatalf("repaired harness is not runnable: %+v", h)
	}
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.installing["codex"] })
}

func TestManagedShellReceivesRelayConnectionEnvironment(t *testing.T) {
	s := testServer(t)
	// Inspect connection identity, not the event capability value itself.
	p := startTest(t, s, `test -n "$RELAY_EVENT_TOKEN" && test -x "$RELAY_BIN" && printf 'socket:%s\nsession:%s\nrelay-environment-ready\n' "$RELAY_SOCKET" "$RELAY_SESSION_ID"`)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("environment probe did not finish")
	}
	meta := p.snapshot()
	output := historyOf(p)
	if meta.ExitCode == nil || *meta.ExitCode != 0 || !strings.Contains(output, "socket:"+filepath.Join(s.stateDir, "run", "daemon.sock")) || !strings.Contains(output, "session:"+meta.ID) || !strings.Contains(output, "relay-environment-ready") {
		t.Fatalf("managed shell did not receive its Relay connection identity: status=%s output=%q", meta.Status, output)
	}
}
