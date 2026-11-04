//go:build linux

package runtime

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func restartCall(s *Server, method, path string, value any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(value)
	r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func preflight(t *testing.T, s *Server) RestartPlan {
	t.Helper()
	w := restartCall(s, "GET", "/api/recovery/preflight", nil)
	var plan RestartPlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
		t.Fatalf("preflight %d %s", w.Code, w.Body.String())
	}
	return plan
}
func TestRestartRestoresNewShellOnceAndPreservesOriginalHistory(t *testing.T) {
	s := testServer(t)
	old := startTest(t, s, "printf 'ORIGINAL HISTORY\\n'; cat")
	ready(t, old)
	eventually(t, func() bool { return strings.Contains(historyOf(old), "ORIGINAL HISTORY") })
	plan := preflight(t, s)
	if len(plan.Blockers) != 0 || len(plan.Entries) != 1 || plan.Entries[0].Mode != "reopen" {
		t.Fatal(plan)
	}
	response := restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("a", 32), "fingerprint": plan.Fingerprint})
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	select {
	case <-s.RestartRequested():
	default:
		t.Fatal("cooperative shutdown was not requested")
	}
	refused := restartCall(s, "POST", "/api/sessions", map[string]string{"cwd": s.stateDir, "harness": "shell"})
	if refused.Code != 409 {
		t.Fatal("new work admitted during quiesce", refused.Code)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := New(s.stateDir, "next-version")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	eventually(t, func() bool {
		next.restart.mu.Lock()
		defer next.restart.mu.Unlock()
		return next.restart.operation.Status == "completed"
	})
	next.restart.mu.Lock()
	entry := next.restart.operation.Entries[0]
	next.restart.mu.Unlock()
	if entry.Status != "started" || entry.Session == nil || entry.Session.ID == old.meta.ID || entry.Session.RestartSource == nil || entry.Session.RestartSource.ID != old.meta.ID {
		t.Fatal("new shell receipt missing", entry)
	}
	previous, _ := next.lookup(old.meta.ID)
	if previous.snapshot().Status != "interrupted" || !strings.Contains(historyOf(previous), "ORIGINAL HISTORY") {
		t.Fatal("old session log was lost")
	}
	if next.runtimeID != s.runtimeID || next.instanceID == s.instanceID {
		t.Fatal("persistent/process identities not separated")
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(s.stateDir, "next-version")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	time.Sleep(30 * time.Millisecond)
	again.mu.Lock()
	count := len(again.sessions)
	again.mu.Unlock()
	if count != 2 {
		t.Fatal("completed restart replayed a launch", count)
	}
}
func TestRestartRejectsStalePlanAndUnprovenAgentBeforeStoppingAnything(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "cat")
	ready(t, p)
	plan := preflight(t, s)
	p.mu.Lock()
	p.meta.Title = "Changed"
	p.mu.Unlock()
	response := restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("b", 32), "fingerprint": plan.Fingerprint})
	if response.Code != 409 {
		t.Fatal(response.Code)
	}
	p.mu.Lock()
	p.meta.Harness = "claude"
	p.mu.Unlock()
	plan = preflight(t, s)
	if len(plan.Blockers) != 1 || plan.Entries[0].Status != "blocked" {
		t.Fatal("unproven provider allowed", plan)
	}
	response = restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("c", 32), "fingerprint": plan.Fingerprint})
	if response.Code != 409 {
		t.Fatal(response.Code)
	}
	if p.snapshot().Status != "running" {
		t.Fatal("rejected restart changed process")
	}
	select {
	case <-s.RestartRequested():
		t.Fatal("rejected restart signaled daemon")
	default:
	}
}
func TestRestartUnknownDispatchIsNeverRepeated(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "cat")
	ready(t, p)
	plan := preflight(t, s)
	response := restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("d", 32), "fingerprint": plan.Fingerprint})
	if response.Code != 202 {
		t.Fatal(response.Code)
	}
	s.restart.mu.Lock()
	s.restart.operation.Status = "restoring"
	s.restart.operation.Entries[0].Status = "dispatching"
	if err := s.saveRestartLocked(s.restart.operation); err != nil {
		t.Fatal(err)
	}
	s.restart.mu.Unlock()
	s.Close()
	next, err := New(s.stateDir, "next")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	eventually(t, func() bool {
		next.restart.mu.Lock()
		defer next.restart.mu.Unlock()
		return next.restart.operation.Status == "attention"
	})
	next.restart.mu.Lock()
	status := next.restart.operation.Entries[0].Status
	next.restart.mu.Unlock()
	if status != "uncertain" {
		t.Fatal(status)
	}
	next.mu.Lock()
	count := len(next.sessions)
	next.mu.Unlock()
	if count != 1 {
		t.Fatal("unknown launch was repeated")
	}
}
func TestRestartPreflightKeepsBoundedSessionCapacity(t *testing.T) {
	s := testServer(t)
	for i := 0; i < 17; i++ {
		p := startTest(t, s, "cat")
		ready(t, p)
	}
	plan := preflight(t, s)
	if len(plan.Blockers) == 0 || !strings.Contains(plan.Blockers[0], "32 slots") {
		t.Fatal("would exceed bounded records", plan.Blockers)
	}
}

func TestRestartPersistenceFailureCannotBeOverwrittenByAnotherIntent(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "cat")
	ready(t, p)
	path := filepath.Join(s.stateDir, "restart.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	plan := preflight(t, s)
	result := restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("e", 32), "fingerprint": plan.Fingerprint})
	if result.Code != 500 {
		t.Fatal(result.Code, result.Body.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result = restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("f", 32), "fingerprint": plan.Fingerprint})
	if result.Code != 409 || p.snapshot().Status != "running" {
		t.Fatal("unconfirmed intent overwritten", result.Code)
	}
	select {
	case <-s.RestartRequested():
		t.Fatal("failed durable intent stopped daemon")
	default:
	}
}
func TestRestartRejectsCorruptEntriesAndChangedOriginalIdentity(t *testing.T) {
	for _, kind := range []string{"duplicate", "mode", "cwd"} {
		t.Run(kind, func(t *testing.T) {
			s := testServer(t)
			p := startTest(t, s, "cat")
			ready(t, p)
			plan := preflight(t, s)
			result := restartCall(s, "POST", "/api/recovery/start", map[string]string{"id": strings.Repeat("e", 32), "fingerprint": plan.Fingerprint})
			if result.Code != 202 {
				t.Fatal(result.Code)
			}
			s.restart.mu.Lock()
			op := s.restart.operation
			switch kind {
			case "duplicate":
				op.Entries = append(op.Entries, op.Entries[0])
			case "mode":
				op.Entries[0].Mode = "resume"
			case "cwd":
				op.Entries[0].Source.Cwd = t.TempDir()
			}
			if err := s.saveRestartLocked(op); err != nil {
				t.Fatal(err)
			}
			s.restart.mu.Unlock()
			s.Close()
			next, err := New(s.stateDir, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if kind == "cwd" {
				eventually(t, func() bool {
					next.restart.mu.Lock()
					defer next.restart.mu.Unlock()
					return next.restart.operation.Status == "attention"
				})
			} else if next.restart.err == "" {
				t.Fatal("corrupt journal accepted")
			}
			next.mu.Lock()
			count := len(next.sessions)
			next.mu.Unlock()
			if count != 1 {
				t.Fatal("unverified source launched another process", count)
			}
		})
	}
}
func TestRestartReceiptMustMatchOperationAndSource(t *testing.T) {
	source := Session{ID: strings.Repeat("a", 32), CreatedAt: time.Now().UTC(), Harness: "shell", Cwd: "/tmp"}
	meta := Session{ID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC(), Harness: "shell", Cwd: "/tmp", RestartReceipt: strings.Repeat("c", 32) + ":" + source.ID, RestartSource: &RestartSource{ID: source.ID, CreatedAt: source.CreatedAt}}
	if !validRestoredReceipt(meta, meta.RestartReceipt, source) {
		t.Fatal("valid receipt rejected")
	}
	meta.RestartSource.CreatedAt = meta.RestartSource.CreatedAt.Add(time.Second)
	if validRestoredReceipt(meta, meta.RestartReceipt, source) {
		t.Fatal("source timestamp mismatch accepted")
	}
	meta.RestartSource = nil
	if validRestartReceipt(meta) {
		t.Fatal("missing source proof accepted")
	}
}
