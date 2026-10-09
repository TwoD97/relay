//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func approvalFixture(t *testing.T) (*Server, *liveSession) {
	t.Helper()
	id, _ := newID()
	p := newLiveSession(Session{ID: id, CreatedAt: time.Now().UTC(), Status: "running", Harness: "claude", Cwd: "/workspace", Permissions: &Permissions{Support: "configured"}})
	p.eventToken = "test-capability"
	return &Server{sessions: map[string]*liveSession{id: p}}, p
}
func approvalRequest(s *Server, p *liveSession, ctx context.Context, payload string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/sessions/"+p.meta.ID+"/approval-hook", strings.NewReader(payload)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+p.eventToken)
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, req)
		done <- response
	}()
	return done
}

const validApproval = `{"provider":"claude","event":"PermissionRequest","toolName":"Bash","input":{"command":"printf sensitive-test-marker"},"cwd":"/workspace","permissionMode":"default"}`

func approvalRecords(s *Server) []Approval {
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/approvals", nil))
	var result struct {
		Requests []Approval `json:"requests"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	return result.Requests
}
func awaitApproval(t *testing.T, s *Server, count int) []Approval {
	t.Helper()
	var records []Approval
	eventually(t, func() bool { records = approvalRecords(s); return len(records) == count })
	return records
}
func approvalDecision(s *Server, a Approval, decision string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"sessionId": a.SessionID, "sessionCreatedAt": a.SessionCreatedAt, "decision": decision})
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/api/approvals/"+a.ID+"/decision", bytes.NewReader(body)))
	return response
}
func TestApprovalConcurrentDecisionsAndObservedPermissions(t *testing.T) {
	s, p := approvalFixture(t)
	result := approvalRequest(s, p, context.Background(), validApproval)
	a := awaitApproval(t, s, 1)[0]
	wrong := a
	wrong.SessionCreatedAt = wrong.SessionCreatedAt.Add(time.Second)
	if code := approvalDecision(s, wrong, "allow").Code; code != 409 {
		t.Fatalf("stale identity: %d", code)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, decision := range []string{"allow", "deny"} {
		wg.Add(1)
		go func(d string) { defer wg.Done(); codes <- approvalDecision(s, a, d).Code }(decision)
	}
	wg.Wait()
	close(codes)
	ok, conflict := 0, 0
	for code := range codes {
		if code == 200 {
			ok++
		}
		if code == 409 {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("CAS: %d success, %d conflicts", ok, conflict)
	}
	response := <-result
	records := approvalRecords(s)
	if response.Code != 200 || records[0].Status != "submitted" || !strings.Contains(response.Body.String(), records[0].Decision) {
		t.Fatal("decision did not reach waiting hook")
	}
	permissions := p.snapshot().Permissions
	if permissions.Support != "active" || permissions.Mode != "default" || permissions.ModeObservedAt == nil {
		t.Fatalf("permissions not observed: %+v", permissions)
	}
	if p.snapshot().Attention == nil {
		t.Fatal("permission attention not recorded")
	}
}
func TestApprovalExpiryCancellationAndSessionFinish(t *testing.T) {
	for _, scenario := range []string{"expiry", "disconnect", "finish", "terminal"} {
		t.Run(scenario, func(t *testing.T) {
			s, p := approvalFixture(t)
			s.approvalTTL = 80 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := approvalRequest(s, p, ctx, validApproval)
			a := awaitApproval(t, s, 1)[0]
			expected := "cancelled"
			switch scenario {
			case "expiry":
				expected = "expired"
			case "disconnect":
				cancel()
			case "finish":
				close(p.done)
			case "terminal":
				expected = "submitted"
				if approvalDecision(s, a, "terminal").Code != 200 {
					t.Fatal("terminal decision failed")
				}
			}
			select {
			case <-result:
			case <-time.After(time.Second):
				t.Fatal("hook did not release")
			}
			record := approvalRecords(s)[0]
			if record.Status != expected {
				t.Fatalf("%+v", record)
			}
			if approvalDecision(s, a, "allow").Code != 409 {
				t.Fatal("resolved request resurrected")
			}
		})
	}
}
func TestApprovalRejectsUnauthorizedUnconfiguredAndOversized(t *testing.T) {
	cases := []struct {
		name, payload, token, purpose, provider, support string
		code                                             int
	}{
		{name: "missing capability", payload: validApproval, code: 401},
		{name: "wrong capability", payload: validApproval, token: "wrong", code: 401},
		{name: "login", payload: validApproval, token: "test-capability", purpose: "login", code: 409},
		{name: "wrong provider", payload: validApproval, token: "test-capability", provider: "codex", code: 409},
		{name: "unsupported", payload: validApproval, token: "test-capability", support: "terminal-only", code: 409},
		{name: "oversized", payload: `{"provider":"claude","event":"PermissionRequest","toolName":"Bash","input":{"command":"` + strings.Repeat("x", maxApprovalInput) + `"}}`, token: "test-capability", code: 400},
		{name: "invalid input", payload: `{"provider":"claude","event":"PermissionRequest","toolName":"Bash","input":[]}`, token: "test-capability", code: 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, p := approvalFixture(t)
			p.meta.Purpose = c.purpose
			if c.provider != "" {
				p.meta.Harness = c.provider
			}
			if c.support != "" {
				p.meta.Permissions.Support = c.support
			}
			r := httptest.NewRequest("POST", "/api/sessions/"+p.meta.ID+"/approval-hook", strings.NewReader(c.payload))
			r.Header.Set("Authorization", "Bearer "+c.token)
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, r)
			if response.Code != c.code {
				t.Fatalf("got %d", response.Code)
			}
			if len(approvalRecords(s)) != 0 {
				t.Fatal("invalid request admitted")
			}
		})
	}
}
func TestApprovalBoundsAndStoppedSession(t *testing.T) {
	s, p := approvalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make([]<-chan *httptest.ResponseRecorder, 0, 4)
	for i := 0; i < 4; i++ {
		results = append(results, approvalRequest(s, p, ctx, validApproval))
		awaitApproval(t, s, i+1)
	}
	if response := <-approvalRequest(s, p, ctx, validApproval); response.Code != 429 {
		t.Fatalf("per-session limit: %d", response.Code)
	}
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	if approvalDecision(s, approvalRecords(s)[0], "allow").Code != 409 {
		t.Fatal("stopped session accepted decision")
	}
	cancel()
	for _, result := range results {
		<-result
	}
	for i := 0; i < 180; i++ {
		s.approvalMu.Lock()
		s.pruneApprovalsLocked()
		id := fmt.Sprint(i)
		s.approvals[id] = &approvalWaiter{record: Approval{ID: id, Status: "submitted", CreatedAt: time.Now()}}
		s.approvalMu.Unlock()
	}
	if len(approvalRecords(s)) != 128 {
		t.Fatal("retained history is not bounded")
	}
}
func TestPermissionConfigurationNoPolicyOverride(t *testing.T) {
	for _, id := range []string{"codex", "claude"} {
		args := permissionHarnessArguments(id, "/opt/relay's runtime/bin/relay", true)
		data := strings.Join(args, " ")
		for _, forbidden := range []string{"dangerously", "bypassPermissions", "updatedPermissions", "updatedInput", "approval_policy", "sandbox_mode", "features.hooks"} {
			if strings.Contains(data, forbidden) {
				t.Fatalf("policy override %s", forbidden)
			}
		}
		if !strings.Contains(data, "PermissionRequest") || !strings.Contains(data, "hook --provider "+id) {
			t.Fatalf("missing hooks: %v", args)
		}
		if id == "claude" {
			var settings map[string]any
			if json.Unmarshal([]byte(args[1]), &settings) != nil {
				t.Fatal("invalid settings")
			}
		}
	}
	for _, c := range []struct {
		id, version string
		want        bool
	}{{"codex", "codex-cli 0.153.4", true}, {"codex", "codex-cli 0.153.3", false}, {"codex", "codex-cli 0.161.0", true}, {"claude", "2.1.209 (Claude Code)", true}, {"claude", "2.1.208", false}, {"claude", "unknown", false}} {
		if got := verifiedPermissionVersion(c.id, c.version); got != c.want {
			t.Fatalf("%+v: %v", c, got)
		}
	}
}

func TestApprovalGlobalPendingLimit(t *testing.T) {
	s, p := approvalFixture(t)
	sessions := []*liveSession{p}
	for i := 1; i < 17; i++ {
		_, next := approvalFixture(t)
		sessions = append(sessions, next)
		s.sessions[next.meta.ID] = next
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waiting []<-chan *httptest.ResponseRecorder
	for i := 0; i < 64; i++ {
		waiting = append(waiting, approvalRequest(s, sessions[i/4], ctx, validApproval))
		awaitApproval(t, s, i+1)
	}
	if response := <-approvalRequest(s, sessions[16], ctx, validApproval); response.Code != 429 {
		t.Fatalf("global limit: %d", response.Code)
	}
	cancel()
	for _, done := range waiting {
		<-done
	}
}

func TestApprovalPayloadNeverEntersPersistentStateAndRestartDoesNotResume(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "sleep 60")
	ready(t, p)
	p.mu.Lock()
	p.meta.Harness = "claude"
	p.meta.Permissions = &Permissions{Support: "configured"}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := approvalRequest(s, p, ctx, validApproval)
	awaitApproval(t, s, 1)
	s.mu.Lock()
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.stateDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sensitive-test-marker") || strings.Contains(string(data), "toolName") {
		t.Fatal("approval payload was persisted")
	}
	cancel()
	<-done
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.stateDir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(approvalRecords(restarted)) != 0 {
		t.Fatal("approval resurrected after daemon restart")
	}
}

// This opt-in check invokes only help/features commands with isolated provider
// configuration. It performs no authentication or model request.
func TestInstalledPermissionHookConfiguration(t *testing.T) {
	if os.Getenv("RELAY_HOOK_CONFIG_SMOKE") != "1" {
		t.Skip("set RELAY_HOOK_CONFIG_SMOKE=1 for installed CLI configuration parsing")
	}
	s := testServer(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, id := range []string{"codex", "claude"} {
		t.Run(id, func(t *testing.T) {
			path, err := s.findCommand(id)
			if err != nil {
				t.Skip("CLI unavailable")
			}
			args := permissionHarnessArguments(id, "/opt/relay's test release/bin/relay", true)
			if id == "codex" {
				args = append(args, "features", "list")
			} else {
				args = append(args, "--help")
			}
			if output, err := s.probe(context.Background(), path, args...); err != nil {
				t.Fatalf("CLI rejected hook configuration: %v: %s", err, output)
			}
			if id == "codex" {
				if _, err := s.probe(context.Background(), path, "-c", `hooks.PermissionRequest="invalid-hook-array"`, "features", "list"); err == nil {
					t.Fatal("configuration probe did not reject an invalid hook definition")
				}
			}
		})
	}
}
