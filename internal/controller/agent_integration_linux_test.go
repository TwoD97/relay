package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimeapi "github.com/TwoD97/relay/internal/runtime"
	"github.com/gorilla/websocket"
)

// These are disposable real PTYs behind two separate loopback runtime handlers.
// They never consult saved host profiles or launch/authenticate an AI provider.
func TestAgentBridgeTwoRuntimeIsolationAndUnknownCreate(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	s, _ := workspaceFixture(t)
	s.links = map[string]*link{}
	s.store = &hostStore{}
	token := strings.Repeat("a", 32)
	s.mastermind = &mastermindStore{record: mastermindRecord{Token: token, MastermindState: MastermindState{Enabled: true}}}
	if _, err := s.PrepareWorkspaceAgentCapability(token); err != nil {
		t.Fatal(err)
	}
	credential, err := readWorkspaceCapability(s.config.StateDir, token)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.AgentHandler()
	invoke := func(method, path string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer "+credential.Token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	type target struct {
		server   *httptest.Server
		creates  atomic.Int32
		drop     atomic.Bool
		requests atomic.Int32
	}
	targets := []*target{}
	for _, host := range []string{"first", "second"} {
		daemon, err := runtimeapi.New(filepath.Join(t.TempDir(), "runtime"), "bridge-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := daemon.Close(); err != nil {
				t.Error(err)
			}
		})
		target := &target{}
		api := daemon.Handler()
		target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			target.requests.Add(1)
			for _, header := range []string{"Authorization", "Cookie", "X-Relay-CSRF"} {
				if r.Header.Get(header) != "" {
					t.Errorf("agent credential forwarded through %s", header)
				}
			}
			if r.Method == "POST" && r.URL.Path == "/api/sessions" {
				target.creates.Add(1)
				if target.drop.Swap(false) {
					record := httptest.NewRecorder()
					api.ServeHTTP(record, r)
					connection, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						connection.Close()
					}
					return
				}
			}
			api.ServeHTTP(w, r)
		}))
		t.Cleanup(target.server.Close)
		u, _ := url.Parse(target.server.URL)
		l := s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
		})
		s.links[host] = l
		t.Cleanup(l.httpTransport.CloseIdleConnections)
		s.store.hosts = append(s.store.hosts, Host{ID: host, Name: host, Status: "online"})
		targets = append(targets, target)
	}
	create := func(host, title string) runtimeapi.Session {
		w := invoke("POST", "/agent/hosts/"+host+"/sessions", map[string]any{"harness": "shell", "cwd": t.TempDir(), "workspace": "Bridge tests", "title": title})
		if w.Code != 201 {
			t.Fatalf("create %s: %d %s", host, w.Code, w.Body.String())
		}
		var session runtimeapi.Session
		if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
			t.Fatal(err)
		}
		return session
	}
	sessions := []runtimeapi.Session{create("first", "first-owned"), create("second", "second-owned")}
	for i, host := range []string{"first", "second"} {
		w := invoke("GET", "/agent/hosts/"+host+"/sessions", nil)
		var list []runtimeapi.Session
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].ID != sessions[i].ID {
			t.Fatal("session list crossed machine boundary")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			w = invoke("POST", "/agent/hosts/"+host+"/sessions/"+sessions[i].ID+"/input", map[string]string{"data": "printf 'BRIDGE_" + host + "\\n'\r"})
			if w.Code == 200 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("session not writable: %d %s", w.Code, w.Body.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		for {
			w = invoke("GET", "/agent/hosts/"+host+"/sessions/"+sessions[i].ID+"/history", nil)
			if w.Code == 200 && strings.Contains(w.Body.String(), "BRIDGE_"+host+"\r\n") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("real PTY output was not routed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// A controller capability must not be forwarded in a remote process Env.
	count := targets[0].requests.Load()
	w := invoke("POST", "/agent/hosts/first/sessions", map[string]any{"harness": "shell", "cwd": t.TempDir(), "env": map[string]string{"RELAY_AGENT_CAPABILITY_FILE": "private"}})
	if w.Code != 400 || targets[0].requests.Load() != count {
		t.Fatal("agent could forward private launch environment")
	}
	// The real runtime's WebSocket writer lease remains authoritative.
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(targets[0].server.URL, "http")+"/api/sessions/"+sessions[0].ID+"/terminal", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = connection.WriteJSON(map[string]string{"type": "claim"})
	readControl := func(owner bool) {
		for {
			kind, data, err := connection.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var state struct {
				Type      string `json:"type"`
				Owner     bool   `json:"owner"`
				Available bool   `json:"available"`
			}
			if kind == websocket.TextMessage && json.Unmarshal(data, &state) == nil && state.Type == "control" && state.Owner == owner && (owner || state.Available) {
				return
			}
		}
	}
	readControl(true)
	w = invoke("POST", "/agent/hosts/first/sessions/"+sessions[0].ID+"/input", map[string]string{"data": "printf 'REJECTED_INPUT\\n'\r"})
	if w.Code != 409 {
		t.Fatalf("agent bypassed viewer lease: %d", w.Code)
	}
	_ = connection.WriteJSON(map[string]string{"type": "release"})
	readControl(false)
	w = invoke("POST", "/agent/hosts/first/sessions/"+sessions[0].ID+"/input", map[string]string{"data": "printf 'AFTER_RELEASE\\n'\r"})
	if w.Code != 200 {
		t.Fatal("lease release did not restore input")
	}
	// A create which executes but loses its HTTP reply is never retried.
	targets[0].drop.Store(true)
	w = invoke("POST", "/agent/hosts/first/sessions", map[string]any{"harness": "shell", "cwd": t.TempDir(), "workspace": "Bridge tests", "title": "lost-reply"})
	if w.Code != 502 || targets[0].creates.Load() != 2 {
		t.Fatalf("unknown create was replayed: %d, creates %d", w.Code, targets[0].creates.Load())
	}
	w = invoke("GET", "/agent/hosts/first/sessions", nil)
	var recovered []runtimeapi.Session
	if json.Unmarshal(w.Body.Bytes(), &recovered) != nil || len(recovered) != 2 {
		t.Fatal("unknown created session could not be inspected")
	}
	s.store.mu.Lock()
	s.store.hosts[1].Status = "disconnected"
	s.store.mu.Unlock()
	count = targets[1].requests.Load()
	w = invoke("POST", "/agent/hosts/second/sessions", map[string]string{"harness": "shell"})
	if w.Code != 503 || targets[1].requests.Load() != count {
		t.Fatal("disconnected host received a mutation")
	}
	for _, path := range []string{"/launch", "/agent/hosts/first/connect", "/agent/hosts/first/harnesses/claude/install", "/agent/hosts/first/runtime/approval-hook"} {
		w = invoke("POST", path, map[string]string{})
		if w.Code != 404 && w.Code != 405 {
			t.Fatalf("unscoped agent operation %s returned %d", path, w.Code)
		}
	}
	// End only our own second-runtime fixture; the sibling remains observable.
	s.store.mu.Lock()
	s.store.hosts[1].Status = "online"
	s.store.mu.Unlock()
	w = invoke("DELETE", "/agent/hosts/second/sessions/"+sessions[1].ID, nil)
	if w.Code != 200 {
		t.Fatalf("stop fixture: %d %s", w.Code, w.Body.String())
	}
	w = invoke("GET", "/agent/hosts/first/sessions", nil)
	if w.Code != 200 {
		t.Fatal("stopping one host affected another")
	}
}
