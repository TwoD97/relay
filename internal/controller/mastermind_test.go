package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

type mastermindRuntimeFixture struct {
	home          string
	mu            sync.Mutex
	sessions      []MastermindSession
	creates       int
	reads         int
	mutations     []string
	supported     bool
	authenticated bool
	lostResponse  bool
	reject        int
	entered       chan struct{}
	release       chan struct{}
	beforeCreate  func()
	legacyStrict  bool
	workerInput   func(http.ResponseWriter, *http.Request, string)
	inputs        []string
}

func newMastermindRuntime(t *testing.T) (*mastermindRuntimeFixture, *httptest.Server) {
	t.Helper()
	f := &mastermindRuntimeFixture{home: t.TempDir(), supported: true, authenticated: true}
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(server.Close)
	return f, server
}

func (f *mastermindRuntimeFixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != "GET" {
		f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
	}
	switch {
	case r.URL.Path == "/api/health":
		writeJSON(w, 200, map[string]any{"home": f.home, "capabilities": map[string]bool{"mastermindV1": f.supported}})
	case r.URL.Path == "/api/harnesses":
		writeJSON(w, 200, []map[string]any{{"id": "claude", "installed": true, "authenticated": f.authenticated}, {"id": "codex", "installed": true, "authenticated": f.authenticated}})
	case r.URL.Path == "/api/sessions" && r.Method == "GET":
		f.reads++
		writeJSON(w, 200, f.sessions)
	case r.URL.Path == "/api/sessions" && r.Method == "POST":
		f.creates++
		if f.beforeCreate != nil {
			f.beforeCreate()
		}
		var fields map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&fields) != nil {
			writeError(w, 400, "invalid fixture body")
			return
		}
		if f.legacyStrict {
			for key := range fields {
				if key != "title" && key != "workspace" && key != "cwd" && key != "harness" {
					writeError(w, 400, "legacy runtime received unsupported field: "+key)
					return
				}
			}
		}
		data, _ := json.Marshal(fields)
		var request MastermindSession
		if json.Unmarshal(data, &request) != nil {
			writeError(w, 400, "invalid fixture body")
			return
		}
		if f.reject != 0 {
			writeError(w, f.reject, "Fixture rejected before starting any agent")
			return
		}
		request.ID, request.Status, request.CreatedAt, request.UpdatedAt = fmt.Sprintf("%032x", f.creates), "running", time.Now().UTC(), time.Now().UTC()
		f.sessions = append(f.sessions, request)
		if f.entered != nil {
			close(f.entered)
			f.entered = nil
		}
		if f.release != nil {
			release := f.release
			f.mu.Unlock()
			<-release
			f.mu.Lock()
		}
		if f.lostResponse {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		writeJSON(w, 201, request)
	case strings.HasPrefix(r.URL.Path, "/api/sessions/") && strings.HasSuffix(r.URL.Path, "/input") && r.Method == "POST":
		var body struct {
			Data string `json:"data"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			writeError(w, 400, "invalid input fixture body")
			return
		}
		f.inputs = append(f.inputs, body.Data)
		if f.workerInput == nil {
			writeError(w, 500, "unexpected fixture input")
			return
		}
		f.workerInput(w, r, body.Data)
	default:
		writeError(w, 404, "unexpected fixture request")
	}
}

func (f *mastermindRuntimeFixture) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *mastermindRuntimeFixture) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func awaitMastermindRead(t *testing.T, f *mastermindRuntimeFixture, previous int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if f.readCount() > previous {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Mastermind did not observe the remote sessions")
}

func mastermindController(t *testing.T, dir, endpoint string) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "runtime", "run"), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{StateDir: dir, Address: ln.Addr().String(), Version: "fixture", LocalSocket: filepath.Join(dir, "runtime", "run", "daemon.sock"), Assets: fstest.MapFS{"index.html": {Data: []byte("workspace")}}, startConnection: func(context.Context, transport.Config) (*transport.Connection, error) {
		return nil, errors.New("fixture does not open SSH connections")
	}})
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	address := strings.TrimPrefix(endpoint, "http://")
	s.mu.Lock()
	s.links["local"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
	s.mu.Unlock()
	s.wakeMastermind()
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(func() { s.Close(); ts.Close() })
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	login(t, s, client)
	return s, ts, client
}

func mastermindPost(t *testing.T, s *Server, ts *httptest.Server, c *http.Client, route string, value any, status int) MastermindState {
	t.Helper()
	data, _ := json.Marshal(value)
	response := request(t, c, "POST", ts.URL+"/api/mastermind/"+route, string(data), s.csrf, ts.URL)
	defer response.Body.Close()
	if response.StatusCode != status {
		var body any
		_ = json.NewDecoder(response.Body).Decode(&body)
		t.Fatalf("%s returned %d, wanted %d: %v", route, response.StatusCode, status, body)
	}
	var state MastermindState
	if status < 300 {
		if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func mastermindSetup(t *testing.T, s *Server, ts *httptest.Server, c *http.Client, revision uint64, harness string) MastermindState {
	t.Helper()
	return mastermindPost(t, s, ts, c, "config", map[string]any{"revision": revision, "hostId": "local", "harness": harness, "agentMode": "manual", "cwd": "~"}, 202)
}

func requireLinuxMastermindCreation(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Creation integration resolves a real Linux runtime folder; remote recovery and persistence are tested on every platform")
	}
}

func awaitMastermind(t *testing.T, s *Server, status string) mastermindRecord {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		state := s.mastermind.current()
		if state.Status == status {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Mastermind did not reach %s: %+v", status, s.mastermind.current().MastermindState)
	return mastermindRecord{}
}

func TestMastermindStartsOnceReattachesAfterRestartAndPreservesPreviousConfiguration(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	dir := t.TempDir()
	s, ts, c := mastermindController(t, dir, backend.URL)
	if f.createCount() != 0 {
		t.Fatal("opening Relay started an unconfigured provider")
	}
	configured := mastermindSetup(t, s, ts, c, 0, "claude")
	first := awaitMastermind(t, s, "running")
	if first.Session == nil || first.Session.Cwd != f.home || first.Session.MastermindID != first.Token {
		t.Fatal("missing exact canonical identity", first)
	}
	mastermindPost(t, s, ts, c, "config", map[string]any{"revision": configured.Revision, "hostId": "local", "harness": "claude", "agentMode": "manual", "cwd": "~"}, 200)
	mastermindPost(t, s, ts, c, "start", map[string]any{"revision": configured.Revision}, 200)
	if f.createCount() != 1 {
		t.Fatal("repeated setup or start duplicated the session")
	}
	s.Close()
	ts.Close()
	s2, ts2, c2 := mastermindController(t, dir, backend.URL)
	resumed := awaitMastermind(t, s2, "running")
	if resumed.Session.ID != first.Session.ID || !resumed.Session.CreatedAt.Equal(first.Session.CreatedAt) || f.createCount() != 1 {
		t.Fatal("controller restart did not reattach exact session")
	}
	mastermindSetup(t, s2, ts2, c2, resumed.Revision, "codex")
	second := awaitMastermind(t, s2, "running")
	if second.Session.ID == first.Session.ID || second.Config.Harness != "codex" {
		t.Fatal("configuration switch was lost")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 2 || len(f.mutations) != 2 {
		t.Fatal("switch deleted, changed or duplicated a prior session", f.mutations)
	}
}

func TestMastermindLostCreateResponseRecoversByTokenWithoutRepeatingPOST(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	f.lostResponse = true
	dir := t.TempDir()
	s, ts, c := mastermindController(t, dir, backend.URL)
	mastermindSetup(t, s, ts, c, 0, "claude")
	awaitMastermind(t, s, "uncertain")
	s.Close()
	ts.Close()
	s2, _, _ := mastermindController(t, dir, backend.URL)
	recovered := awaitMastermind(t, s2, "running")
	if f.createCount() != 1 || recovered.Session == nil {
		t.Fatal("lost creation response was replayed instead of recovered")
	}
}

func TestMastermindUnknownStartupAndIdentityMismatchNeverRestart(t *testing.T) {
	requireLinuxMastermindCreation(t)
	for _, change := range []string{"missing", "token", "created", "harness", "mode", "cwd", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			f, backend := newMastermindRuntime(t)
			s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
			mastermindSetup(t, s, ts, c, 0, "claude")
			current := awaitMastermind(t, s, "running")
			f.mu.Lock()
			switch change {
			case "missing":
				f.sessions = nil
			case "token":
				f.sessions[0].MastermindID = strings.Repeat("f", 32)
			case "created":
				f.sessions[0].CreatedAt = f.sessions[0].CreatedAt.Add(time.Second)
			case "harness":
				f.sessions[0].Harness = "codex"
			case "mode":
				f.sessions[0].AgentMode = "auto"
			case "cwd":
				f.sessions[0].Cwd = "/different"
			case "duplicate":
				f.sessions = append(f.sessions, f.sessions[0])
			}
			f.mu.Unlock()
			s.wakeMastermind()
			status := "uncertain"
			if change == "missing" {
				status = "missing"
			}
			awaitMastermind(t, s, status)
			if change != "missing" {
				mastermindPost(t, s, ts, c, "start", map[string]any{"revision": current.Revision}, 200)
			}
			if f.createCount() != 1 {
				t.Fatal("identity failure caused another start")
			}
		})
	}
}

func TestMastermindDisableResumeAndExitRequireExplicitNewStart(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
	mastermindSetup(t, s, ts, c, 0, "claude")
	current := awaitMastermind(t, s, "running")
	disabled := mastermindPost(t, s, ts, c, "disable", map[string]any{"revision": current.Revision}, 202)
	if disabled.Enabled || disabled.Session.ID != current.Session.ID {
		t.Fatal("disable lost retained identity")
	}
	mastermindPost(t, s, ts, c, "start", map[string]any{"revision": disabled.Revision}, 202)
	resumed := awaitMastermind(t, s, "running")
	if f.createCount() != 1 || resumed.Session.ID != current.Session.ID {
		t.Fatal("resume created another process")
	}
	f.mu.Lock()
	code := 0
	f.sessions[0].Status = "exited"
	f.sessions[0].ExitCode = &code
	f.mu.Unlock()
	s.wakeMastermind()
	ended := awaitMastermind(t, s, "exited")
	s.wakeMastermind()
	time.Sleep(50 * time.Millisecond)
	if f.createCount() != 1 {
		t.Fatal("exit automatically restarted provider")
	}
	mastermindPost(t, s, ts, c, "start", map[string]any{"revision": ended.Revision}, 202)
	newSession := awaitMastermind(t, s, "running")
	if newSession.Session.ID == ended.Session.ID || f.createCount() != 2 {
		t.Fatal("explicit start did not create one replacement")
	}
}

func TestMastermindPreflightBlocksOldRuntimeAndUnsignedInHarness(t *testing.T) {
	requireLinuxMastermindCreation(t)
	for _, reason := range []string{"capability", "authentication"} {
		t.Run(reason, func(t *testing.T) {
			f, backend := newMastermindRuntime(t)
			if reason == "capability" {
				f.supported = false
			} else {
				f.authenticated = false
			}
			s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
			mastermindSetup(t, s, ts, c, 0, "claude")
			blocked := awaitMastermind(t, s, "blocked")
			if f.createCount() != 0 {
				t.Fatal("failed readiness started an agent")
			}
			f.mu.Lock()
			f.supported, f.authenticated = true, true
			f.mu.Unlock()
			if reason == "capability" {
				mastermindPost(t, s, ts, c, "start", map[string]any{"revision": blocked.Revision}, 200)
			} else {
				s.wakeMastermind()
			}
			awaitMastermind(t, s, "running")
			if f.createCount() != 1 {
				t.Fatal("completed setup did not start exactly once")
			}
		})
	}
}

func TestMastermindDefiniteRejectionRequiresExplicitRetry(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	f.reject = 422
	s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
	mastermindSetup(t, s, ts, c, 0, "claude")
	blocked := awaitMastermind(t, s, "blocked")
	if blocked.Phase != "rejected" {
		t.Fatal("definite rejection treated as unknown", blocked.Phase)
	}
	s.wakeMastermind()
	time.Sleep(50 * time.Millisecond)
	if f.createCount() != 1 {
		t.Fatal("rejected create automatically retried")
	}
	f.mu.Lock()
	f.reject = 0
	f.mu.Unlock()
	mastermindPost(t, s, ts, c, "start", map[string]any{"revision": blocked.Revision}, 202)
	awaitMastermind(t, s, "running")
	if f.createCount() != 2 {
		t.Fatal("explicit retry not dispatched")
	}
}

func TestMastermindStaleRevisionAndBrowserSecurity(t *testing.T) {
	_, backend := newMastermindRuntime(t)
	s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
	input := `{"revision":0,"hostId":"local","harness":"claude","agentMode":"manual","cwd":"~"}`
	if response := request(t, c, "POST", ts.URL+"/api/mastermind/config", input, "", ts.URL); response.StatusCode != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if response := request(t, c, "POST", ts.URL+"/api/mastermind/config", input, s.csrf, "https://foreign.invalid"); response.StatusCode != 403 {
		t.Fatal("foreign origin accepted")
	}
	if response := request(t, c, "POST", ts.URL+"/api/mastermind/config", strings.TrimSuffix(input, "}")+`,"prompt":"injected"}`, s.csrf, ts.URL); response.StatusCode != 400 {
		t.Fatal("unknown field accepted")
	}
	mastermindSetup(t, s, ts, c, 0, "claude")
	mastermindPost(t, s, ts, c, "config", map[string]any{"revision": 0, "hostId": "local", "harness": "codex", "agentMode": "manual", "cwd": "~"}, 409)
	mastermindPost(t, s, ts, c, "disable", map[string]any{"revision": 0}, 409)
}

func TestMastermindCorruptStateDoesNotBlockFleetOrOverwriteEvidence(t *testing.T) {
	_, backend := newMastermindRuntime(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "mastermind.json")
	data := []byte(`{"corrupt":true}`)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, ts, c := mastermindController(t, dir, backend.URL)
	if got := request(t, c, "GET", ts.URL+"/api/state", "", "", ts.URL).StatusCode; got != 200 {
		t.Fatal("optional state blocked fleet", got)
	}
	if s.mastermind.current().Status != "uncertain" {
		t.Fatal("corruption hidden")
	}
	mastermindPost(t, s, ts, c, "config", map[string]any{"revision": 0, "hostId": "local", "harness": "claude", "agentMode": "manual", "cwd": "~"}, 409)
	actual, _ := os.ReadFile(file)
	if string(actual) != string(data) {
		t.Fatal("corrupt state overwritten")
	}
}

func TestMastermindConcurrentConfigurationStartsOnlyOneAgent(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	s, ts, client := mastermindController(t, t.TempDir(), backend.URL)
	body := []byte(`{"revision":0,"hostId":"local","harness":"claude","agentMode":"manual","cwd":"~"}`)
	results := make(chan int, 8)
	for range 8 {
		go func() {
			req, _ := http.NewRequest("POST", ts.URL+"/api/mastermind/config", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", ts.URL)
			req.Header.Set("X-Relay-CSRF", s.csrf)
			response, err := client.Do(req)
			if err != nil {
				results <- 0
				return
			}
			response.Body.Close()
			results <- response.StatusCode
		}()
	}
	accepted := 0
	for range 8 {
		status := <-results
		if status == 202 {
			accepted++
		} else if status != 409 {
			t.Fatal("unexpected concurrent result", status)
		}
	}
	awaitMastermind(t, s, "running")
	if accepted != 1 || f.createCount() != 1 {
		t.Fatal("concurrent configuration duplicated work", accepted, f.createCount())
	}
}

func TestMastermindLateResponseCannotReplaceNewConfiguration(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.entered, f.release = entered, release
	s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
	first := mastermindSetup(t, s, ts, c, 0, "claude")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first start did not reach runtime")
	}
	second := mastermindSetup(t, s, ts, c, first.Revision, "codex")
	f.mu.Lock()
	f.release = nil
	f.mu.Unlock()
	close(release)
	current := awaitMastermind(t, s, "running")
	if current.Revision != second.Revision || current.Session.Harness != "codex" || current.Session.ID != fmt.Sprintf("%032x", 2) {
		t.Fatal("late first response replaced the selected Mastermind", current)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 2 || f.sessions[0].Harness != "claude" || len(f.mutations) != 2 {
		t.Fatal("previous provider session was not preserved", f.mutations)
	}
}

func TestMastermindUnknownStartNeverReplaysAndDisabledResumeCanRecover(t *testing.T) {
	requireLinuxMastermindCreation(t)
	f, backend := newMastermindRuntime(t)
	f.reject = 500
	dir := t.TempDir()
	s, ts, c := mastermindController(t, dir, backend.URL)
	mastermindSetup(t, s, ts, c, 0, "claude")
	unknown := awaitMastermind(t, s, "uncertain")
	mastermindPost(t, s, ts, c, "start", map[string]any{"revision": unknown.Revision}, 409)
	disabled := mastermindPost(t, s, ts, c, "disable", map[string]any{"revision": unknown.Revision}, 202)
	mastermindPost(t, s, ts, c, "start", map[string]any{"revision": disabled.Revision}, 202)
	awaitMastermind(t, s, "uncertain")
	s.Close()
	ts.Close()
	s2, _, _ := mastermindController(t, dir, backend.URL)
	awaitMastermind(t, s2, "uncertain")
	if f.createCount() != 1 {
		t.Fatal("an unknown start was replayed")
	}
	// A later list can reveal the exact committed attempt. Recovery is GET only.
	f.mu.Lock()
	f.sessions = []MastermindSession{{ID: strings.Repeat("a", 32), CreatedAt: time.Now().UTC(), Status: "running", Harness: "claude", AgentMode: "manual", Cwd: unknown.CanonicalCwd, MastermindID: unknown.Token}}
	f.mu.Unlock()
	s2.wakeMastermind()
	recovered := awaitMastermind(t, s2, "running")
	if recovered.Session.ID != strings.Repeat("a", 32) || f.createCount() != 1 {
		t.Fatal("late token discovery did not recover without replay")
	}
}

func TestMastermindFailedStateWritesStopBeforeMutationAndRecoverAfterResponse(t *testing.T) {
	requireLinuxMastermindCreation(t)
	for _, phase := range []string{"before-create", "after-create"} {
		t.Run(phase, func(t *testing.T) {
			f, backend := newMastermindRuntime(t)
			f.authenticated = false
			dir := t.TempDir()
			s, ts, c := mastermindController(t, dir, backend.URL)
			mastermindSetup(t, s, ts, c, 0, "claude")
			awaitMastermind(t, s, "blocked")
			badPath := filepath.Join(t.TempDir(), "cannot-replace-directory")
			if err := os.Mkdir(badPath, 0700); err != nil {
				t.Fatal(err)
			}
			breakStore := func() { s.mastermind.mu.Lock(); s.mastermind.path = badPath; s.mastermind.mu.Unlock() }
			if phase == "before-create" {
				breakStore()
			} else {
				f.mu.Lock()
				f.beforeCreate = breakStore
				f.mu.Unlock()
			}
			f.mu.Lock()
			f.authenticated = true
			f.mu.Unlock()
			s.wakeMastermind()
			awaitMastermind(t, s, "uncertain")
			if phase == "before-create" {
				if f.createCount() != 0 {
					t.Fatal("write failure allowed an uncommitted mutation")
				}
			} else {
				if f.createCount() != 1 {
					t.Fatal("fixture never reached create")
				}
				s.Close()
				ts.Close()
				s2, _, _ := mastermindController(t, dir, backend.URL)
				awaitMastermind(t, s2, "running")
				if f.createCount() != 1 {
					t.Fatal("failed identity persistence replayed create")
				}
			}
		})
	}
}

func TestMastermindHostForgetAndIdentityGuards(t *testing.T) {
	f, backend := newMastermindRuntime(t)
	s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
	host := Host{ID: "remote-fixture", Name: "Fixture", Target: "fixture.invalid", Port: 22, CreatedAt: time.Now().UTC(), Status: "disconnected"}
	if err := s.store.add(host); err != nil {
		t.Fatal(err)
	}
	configured := mastermindPost(t, s, ts, c, "config", map[string]any{"revision": 0, "hostId": host.ID, "harness": "claude", "agentMode": "manual", "cwd": "/fixture/project"}, 202)
	if got := request(t, c, "DELETE", ts.URL+"/api/hosts/"+host.ID, "", s.csrf, ts.URL).StatusCode; got != 409 {
		t.Fatal("configured host could be forgotten", got)
	}
	s.store.mu.Lock()
	s.store.hosts[0].Target = "replacement.invalid"
	s.store.mu.Unlock()
	s.wakeMastermind()
	awaitMastermind(t, s, "blocked")
	if f.createCount() != 0 {
		t.Fatal("host identity change started provider")
	}
	mastermindPost(t, s, ts, c, "disable", map[string]any{"revision": configured.Revision}, 202)
	if got := request(t, c, "DELETE", ts.URL+"/api/hosts/"+host.ID, "", s.csrf, ts.URL).StatusCode; got != 204 {
		t.Fatal("disabled coordinator prevented explicit forget", got)
	}
	if f.createCount() != 0 {
		t.Fatal("forget mutated provider")
	}
}

func attachMastermindRemote(t *testing.T, s *Server, endpoint string, host Host) {
	t.Helper()
	if _, exists := s.store.get(host.ID); !exists {
		if err := s.store.add(host); err != nil {
			t.Fatal(err)
		}
	}
	s.store.update(host.ID, "online", "Fixture connected", "")
	address := strings.TrimPrefix(endpoint, "http://")
	s.mu.Lock()
	s.links[host.ID] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
	s.mu.Unlock()
	s.wakeMastermind()
}

func TestMastermindRemoteRecoveryIsReadOnlyAndPersistent(t *testing.T) {
	// These cases run natively on Windows too. They exercise the actual remote
	// HTTP observation and durable store without inventing Windows local PTYs.
	for _, scenario := range []string{"owned", "absent", "wrong-folder", "wrong-harness", "wrong-mode", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			f, backend := newMastermindRuntime(t)
			dir := t.TempDir()
			s, ts, c := mastermindController(t, dir, backend.URL)
			host := Host{ID: "remote-fixture", Name: "Fixture", Target: "fixture.invalid", Port: 22, CreatedAt: time.Now().UTC(), Status: "online"}
			attachMastermindRemote(t, s, backend.URL, host)
			record := mastermindRecord{Format: 1, MastermindState: MastermindState{Revision: 1, Enabled: true, Config: &MastermindConfig{HostID: host.ID, Harness: "claude", AgentMode: "manual", Cwd: "~/project"}, Status: "starting", Stage: "Verifying previous attempt"}, Phase: "creating", Token: strings.Repeat("a", 32), CanonicalCwd: "/remote/project", HostTarget: host.Target, HostPort: host.Port, HostCreatedAt: host.CreatedAt}
			owned := MastermindSession{ID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Status: "running", Harness: "claude", AgentMode: "manual", MastermindID: record.Token, Cwd: record.CanonicalCwd}
			f.mu.Lock()
			f.sessions = []MastermindSession{owned}
			switch scenario {
			case "absent":
				f.sessions = nil
			case "wrong-folder":
				f.sessions[0].Cwd = "/another/project"
			case "wrong-harness":
				f.sessions[0].Harness = "codex"
			case "wrong-mode":
				f.sessions[0].AgentMode = "auto"
			case "duplicate":
				f.sessions = append(f.sessions, owned)
			}
			f.mu.Unlock()
			s.mastermind.mu.Lock()
			err := s.mastermind.saveLocked(record)
			s.mastermind.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			s.wakeMastermind()
			status := "uncertain"
			if scenario == "owned" {
				status = "running"
			}
			current := awaitMastermind(t, s, status)
			if scenario == "absent" {
				disabled := mastermindPost(t, s, ts, c, "disable", map[string]any{"revision": current.Revision}, 202)
				mastermindPost(t, s, ts, c, "start", map[string]any{"revision": disabled.Revision}, 202)
				awaitMastermind(t, s, "uncertain")
			}
			s.Close()
			ts.Close()
			previousReads := f.readCount()
			s2, _, _ := mastermindController(t, dir, backend.URL)
			attachMastermindRemote(t, s2, backend.URL, host)
			awaitMastermindRead(t, f, previousReads)
			recovered := awaitMastermind(t, s2, status)
			if scenario == "owned" {
				if recovered.Session == nil || recovered.Session.ID != owned.ID || !recovered.Session.CreatedAt.Equal(owned.CreatedAt) {
					t.Fatal("owned identity did not survive controller restart")
				}
				f.mu.Lock()
				f.sessions[0].CreatedAt = f.sessions[0].CreatedAt.Add(time.Second)
				f.mu.Unlock()
				s2.wakeMastermind()
				awaitMastermind(t, s2, "uncertain")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.mutations) != 0 {
				t.Fatal("read-only recovery mutated remote runtime", f.mutations)
			}
		})
	}
}

func TestMastermindRemoteConfigurationSurvivesOfflineRestart(t *testing.T) {
	f, backend := newMastermindRuntime(t)
	dir := t.TempDir()
	s, ts, c := mastermindController(t, dir, backend.URL)
	host := Host{ID: "remote-fixture", Name: "Fixture", Target: "fixture.invalid", Port: 22, CreatedAt: time.Now().UTC(), Status: "disconnected"}
	if err := s.store.add(host); err != nil {
		t.Fatal(err)
	}
	configured := mastermindPost(t, s, ts, c, "config", map[string]any{"revision": 0, "hostId": host.ID, "harness": "codex", "agentMode": "auto", "cwd": "/remote/project"}, 202)
	s.Close()
	ts.Close()
	s2, _, _ := mastermindController(t, dir, backend.URL)
	restored := s2.mastermind.current()
	if restored.Revision != configured.Revision || !restored.Enabled || restored.Config == nil || *restored.Config != *configured.Config || restored.Phase != "queued" || restored.HostTarget != host.Target || !restored.HostCreatedAt.Equal(host.CreatedAt) {
		t.Fatal("configured intent or machine identity changed across restart", restored)
	}
	if f.createCount() != 0 {
		t.Fatal("offline configuration started an agent")
	}
}
