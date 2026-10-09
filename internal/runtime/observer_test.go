//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func observerFixture(t *testing.T) (*Server, *observerManager, *liveSession) {
	t.Helper()
	id, _ := newID()
	p := newLiveSession(Session{ID: id, CreatedAt: time.Now().UTC(), Harness: "codex", Status: "running", Title: "Example", Workspace: "Tests", Cwd: "/example"})
	p.history.append([]byte("Read two files; tests still pending."))
	s := &Server{stateDir: t.TempDir(), sessions: map[string]*liveSession{id: p}}
	o := newObserver(s)
	s.observer = o
	o.validate = func(context.Context, ObserverConfig) error { return nil }
	o.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
		return ObserverContent{Summary: "Read two files.", Steps: []string{"Read source."}, NextSteps: []string{"Run tests."}, Blockers: []string{}}, nil
	}
	return s, o, p
}

func observerHTTP(s *Server, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func observerEnable(t *testing.T, s *Server, enabled bool) {
	t.Helper()
	cfg := defaultObserverConfig()
	cfg.Enabled = enabled
	data, _ := json.Marshal(cfg)
	w := observerHTTP(s, "POST", "/api/observer/config", string(data))
	if w.Code != 200 {
		t.Fatalf("config: %d %s", w.Code, w.Body.String())
	}
}

func observerRefresh(s *Server, p *liveSession) *httptest.ResponseRecorder {
	meta := p.snapshot()
	data, _ := json.Marshal(map[string]any{"sessionId": meta.ID, "sessionCreatedAt": meta.CreatedAt})
	return observerHTTP(s, "POST", "/api/observer/refresh", string(data))
}

func TestObserverRequiresOptInAndExcludesNonAgentSessions(t *testing.T) {
	s, o, p := observerFixture(t)
	var calls int
	o.run = func(_ context.Context, _ ObserverConfig, prompt []byte) (ObserverContent, error) {
		calls++
		if strings.Contains(string(prompt), "secret-login-marker") {
			t.Fatal("login session observed")
		}
		return ObserverContent{Summary: "Review pending."}, nil
	}
	for _, kind := range []string{"login", "maintenance", "install", "shell"} {
		id, _ := newID()
		meta := Session{ID: id, Harness: "claude", Purpose: kind, CreatedAt: time.Now(), Status: "running"}
		if kind == "shell" {
			meta.Harness = "shell"
			meta.Purpose = ""
		}
		other := newLiveSession(meta)
		other.history.append([]byte("secret-login-marker"))
		s.sessions[id] = other
	}
	if observerHTTP(s, "GET", "/api/observer", "").Code != 200 || o.runNext(context.Background(), time.Now()) || calls != 0 {
		t.Fatal("read/default caused model use")
	}
	if observerRefresh(s, p).Code != 409 {
		t.Fatal("refresh allowed without opt in")
	}
	observerEnable(t, s, true)
	if !o.runNext(context.Background(), time.Now()) || calls != 1 {
		t.Fatal("normal Codex session not summarized")
	}
	if o.runNext(context.Background(), time.Now().Add(time.Hour)) || calls != 1 {
		t.Fatal("unchanged or excluded context summarized")
	}
	state := o.snapshot()
	if len(state.Summaries) != 1 || state.Summaries[0].Stale || state.Summaries[0].Status != "ready" {
		t.Fatalf("bad state: %+v", state)
	}
	data, err := os.ReadFile(o.path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Read two files; tests still pending") || strings.Contains(string(data), "secret-login-marker") {
		t.Fatal("raw observation persisted")
	}
	info, _ := os.Stat(o.path())
	if info.Mode().Perm() != 0600 {
		t.Fatal("summary state must be private")
	}
}

func TestObserverChangedContextCadenceAndFailureKeepsProvenance(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	now := time.Now().UTC()
	if !o.runNext(context.Background(), now) {
		t.Fatal("initial missing")
	}
	first := o.snapshot().Summaries[0]
	p.history.append([]byte("\nTests failed."))
	if !o.snapshot().Summaries[0].Stale {
		t.Fatal("changed source marked fresh")
	}
	if o.runNext(context.Background(), now.Add(time.Second)) {
		t.Fatal("cadence ignored")
	}
	o.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
		return ObserverContent{}, errors.New("secret provider stderr")
	}
	if !o.runNext(context.Background(), now.Add(301*time.Second)) {
		t.Fatal("changed source not scheduled")
	}
	failed := o.snapshot().Summaries[0]
	if failed.Status != "error" || !failed.Stale || failed.Summary != first.Summary || !failed.GeneratedAt.Equal(*first.GeneratedAt) || strings.Contains(failed.Error, "secret") {
		t.Fatalf("lost content/provenance or raw error: %+v", failed)
	}
	if o.runNext(context.Background(), now.Add(time.Hour)) {
		t.Fatal("failed model invocation automatically retried")
	}
	if observerRefresh(s, p).Code != 202 {
		t.Fatal("explicit refresh rejected")
	}
	if observerRefresh(s, p).Code != 409 {
		t.Fatal("duplicate refresh accepted")
	}
}

func TestObserverExactSessionIdentityAndSingleWorker(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	wrong := p.snapshot()
	wrong.CreatedAt = wrong.CreatedAt.Add(time.Second)
	data, _ := json.Marshal(map[string]any{"sessionId": wrong.ID, "sessionCreatedAt": wrong.CreatedAt})
	if observerHTTP(s, "POST", "/api/observer/refresh", string(data)).Code != 409 {
		t.Fatal("wrong identity accepted")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan bool, 1)
	var calls atomic.Int32
	o.run = func(ctx context.Context, _ ObserverConfig, _ []byte) (ObserverContent, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ObserverContent{Summary: "Done."}, ctx.Err()
	}
	go func() { done <- o.runNext(context.Background(), time.Now()) }()
	<-started
	if o.runNext(context.Background(), time.Now()) {
		t.Fatal("second worker started")
	}
	if observerRefresh(s, p).Code != 409 {
		t.Fatal("active refresh replayed")
	}
	if !o.snapshot().Running {
		t.Fatal("worker not exposed")
	}
	observerEnable(t, s, false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable failed to cancel worker")
	}
	close(release)
	if calls.Load() != 1 || o.snapshot().Summaries[0].Status != "error" {
		t.Fatal("cancelled output published")
	}
}

func TestObserverRestartDoesNotReplayUncertainInvocation(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	if observerRefresh(s, p).Code != 202 {
		t.Fatal("queue failed")
	}
	for _, status := range []string{"queued", "running"} {
		rec := o.state.Records[p.meta.ID]
		rec.Status = status
		o.state.Records[p.meta.ID] = rec
		if err := o.saveLocked(); err != nil {
			t.Fatal(err)
		}
		restored := newObserver(s)
		restored.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
			t.Fatal("uncertain invocation replayed")
			return ObserverContent{}, nil
		}
		if restored.runNext(context.Background(), time.Now().Add(time.Hour)) {
			t.Fatal("restart replayed queued/running")
		}
		if restored.snapshot().Summaries[0].Status != "error" {
			t.Fatal("interruption not visible")
		}
	}
}

func TestObserverHourlyBudgetAndPersistenceFailureBlockModel(t *testing.T) {
	s, o, _ := observerFixture(t)
	observerEnable(t, s, true)
	now := time.Now().UTC()
	for range observerHourlyLimit {
		o.state.Attempts = append(o.state.Attempts, now)
	}
	o.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
		t.Fatal("model invoked across budget/storage boundary")
		return ObserverContent{}, nil
	}
	if o.runNext(context.Background(), now) || o.snapshot().Limits.Remaining != 0 {
		t.Fatal("budget ignored")
	}
	if err := o.saveLocked(); err != nil {
		t.Fatal(err)
	}
	restored := newObserver(s)
	restored.run = o.run
	if restored.runNext(context.Background(), now) {
		t.Fatal("restart resets budget")
	}
	o.state.Attempts = nil
	if err := os.Remove(o.path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(o.path(), 0700); err != nil {
		t.Fatal(err)
	}
	if o.runNext(context.Background(), now) || o.snapshot().Error == "" {
		t.Fatal("storage failure not surfaced")
	}
}

func TestObserverCorruptStateFailsClosedAndExplicitSaveRepairs(t *testing.T) {
	s, _, _ := observerFixture(t)
	path := filepath.Join(s.stateDir, "observer.json")
	if err := os.WriteFile(path, []byte("invalid state"), 0600); err != nil {
		t.Fatal(err)
	}
	o := newObserver(s)
	s.observer = o
	if o.snapshot().Config.Enabled || o.snapshot().Error == "" {
		t.Fatal("corrupt state enabled observer")
	}
	if o.runNext(context.Background(), time.Now()) {
		t.Fatal("corrupt state scheduled")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "invalid state" {
		t.Fatal("corrupt state silently overwritten")
	}
	observerEnable(t, s, false)
	if o.snapshot().Error != "" {
		t.Fatal("explicit repair failed")
	}
}

func TestObserverDeletedSessionCancelsWithoutResurrection(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	started := make(chan struct{})
	done := make(chan bool, 1)
	o.run = func(ctx context.Context, _ ObserverConfig, _ []byte) (ObserverContent, error) {
		close(started)
		<-ctx.Done()
		return ObserverContent{}, ctx.Err()
	}
	go func() { done <- o.runNext(context.Background(), time.Now()) }()
	<-started
	s.mu.Lock()
	delete(s.sessions, p.meta.ID)
	s.mu.Unlock()
	o.removeSession(p.meta.ID)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deleted session worker not cancelled")
	}
	if len(o.snapshot().Summaries) != 0 || len(o.state.Records) != 0 {
		t.Fatal("deleted session summary resurrected")
	}
}

func TestObserverBoundedTerminalPromptAndEscapeFiltering(t *testing.T) {
	data := strings.Repeat("x", 40<<10) + "\x1b[31mred\x1b[0m\x1b]52;c;clipboard-secret\x07\x1bPcontrol-secret\x1b\\\nvisible"
	text := observerPlainText([]byte(data))
	if len(text) > 10<<10 {
		t.Fatal("plain historical context exceeds input limit")
	}
	if !strings.HasSuffix(text, "red\nvisible") || strings.Contains(text, "secret") || strings.Contains(text, "\x1b") {
		t.Fatal("terminal control strings retained")
	}
	source := observerSource{Session: Session{Title: "untrusted <tag>", Harness: "claude", Status: "running"}, Text: strings.Repeat("\"", 10<<10)}
	prompt, err := observerPrompt(source, strings.Repeat("\"", 4000))
	if err != nil || len(prompt) > observerPromptLimit || !json.Valid(prompt) {
		t.Fatal("worst-case valid prompt exceeds bound")
	}
}

func TestObserverStorageRepairAllowsOnlyExplicitRetry(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	if err := os.Remove(o.path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(o.path(), 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
		calls++
		return ObserverContent{Summary: "After repair."}, nil
	}
	if o.runNext(context.Background(), time.Now()) || calls != 0 {
		t.Fatal("provider started without durable attempt")
	}
	if status := o.snapshot().Summaries[0].Status; status != "error" {
		t.Fatalf("phantom worker left %s", status)
	}
	if err := os.Remove(o.path()); err != nil {
		t.Fatal(err)
	}
	observerEnable(t, s, true)
	if o.runNext(context.Background(), time.Now().Add(time.Hour)) || calls != 0 {
		t.Fatal("repair replayed uncertain work")
	}
	if got := observerRefresh(s, p).Code; got != 202 {
		t.Fatalf("explicit recovery refresh blocked: %d", got)
	}
	if !o.runNext(context.Background(), time.Now()) || calls != 1 {
		t.Fatal("explicit recovery did not start one worker")
	}
}

func TestObserverCompletionCannotRestoreRecordAfterRemoval(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	if !o.runNext(context.Background(), time.Now()) {
		t.Fatal("initial record absent")
	}
	source := s.observerSources()[p.meta.ID]
	staleSnapshot := s.observerSources()
	s.mu.Lock()
	delete(s.sessions, p.meta.ID)
	s.mu.Unlock()
	o.removeSession(p.meta.ID)
	// Reproduce deletion after the worker's source capture but before it acquires
	// the observer lock to publish completion.
	o.finishRun(source, o.state.Config, ObserverContent{Summary: "Late result."}, nil, false, time.Now(), staleSnapshot)
	if len(o.state.Records) != 0 {
		t.Fatal("late worker recreated a removed record")
	}
	restored := newObserver(s)
	if restored.err != "" || len(restored.state.Records) != 0 {
		t.Fatal("late result corrupted persisted state")
	}
}

func TestObserverSettingsAndQueueStorageFailuresBlockScheduling(t *testing.T) {
	for _, action := range []string{"settings", "refresh"} {
		t.Run(action, func(t *testing.T) {
			s, o, p := observerFixture(t)
			observerEnable(t, s, true)
			if err := os.Remove(o.path()); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(o.path(), 0700); err != nil {
				t.Fatal(err)
			}
			if action == "refresh" {
				if observerRefresh(s, p).Code != 500 {
					t.Fatal("refresh unexpectedly succeeded")
				}
			} else {
				cfg := defaultObserverConfig()
				data, _ := json.Marshal(cfg)
				if observerHTTP(s, "POST", "/api/observer/config", string(data)).Code != 500 {
					t.Fatal("settings unexpectedly saved")
				}
			}
			if o.snapshot().Error == "" {
				t.Fatal("storage failure not latched")
			}
			if err := os.Remove(o.path()); err != nil {
				t.Fatal(err)
			}
			o.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
				t.Fatal("auto-retried after transient storage failure")
				return ObserverContent{}, nil
			}
			if o.runNext(context.Background(), time.Now()) {
				t.Fatal("storage failure did not block requests until explicit settings save")
			}
		})
	}
}

func TestObserverSourceOmitsInvisibleStringsAcrossTailBoundary(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	// The start of this clipboard string is outside the old 32KiB tail; a
	// stateless parser of that tail would expose its hidden payload.
	hidden := strings.Repeat("clipboard-secret", 3000)
	_, _ = p.Write([]byte("\r\nVisible task.\x1b]52;c;" + hidden + "\x07\r\nNext visible step."))
	source := s.observerSources()[p.meta.ID]
	if strings.Contains(source.Text, "clipboard-secret") || !strings.Contains(source.Text, "Next visible step.") {
		t.Fatalf("screen source was not visible text: %.200s", source.Text)
	}
	p.mu.Lock()
	p.screen = nil
	p.mu.Unlock()
	source = s.observerSources()[p.meta.ID]
	if strings.Contains(source.Text, "clipboard-secret") || !strings.Contains(source.Text, "Next visible step.") {
		t.Fatal("complete historical stream leaked control payload")
	}
	// Include the 8-bit OSC form handled by the shared ANSI parser too.
	p.mu.Lock()
	p.history.append([]byte("\x9d52;c;c1-secret\x07\nvisible-after-c1"))
	p.mu.Unlock()
	if strings.Contains(s.observerSources()[p.meta.ID].Text, "c1-secret") {
		t.Fatal("8-bit control string leaked")
	}
	p.mu.Lock()
	p.history.append([]byte(strings.Repeat("unknown control payload", maxHistory/20)))
	p.mu.Unlock()
	source = s.observerSources()[p.meta.ID]
	if source.Text != "" || source.unavailable == "" {
		t.Fatal("truncated history was treated as trusted parser input")
	}
	response := observerRefresh(s, p)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "truncated history") {
		t.Fatal("unsafe recovered context did not explain unavailability")
	}
	o.run = func(context.Context, ObserverConfig, []byte) (ObserverContent, error) {
		t.Fatal("unsafe history reached model")
		return ObserverContent{}, nil
	}
	if o.runNext(context.Background(), time.Now()) {
		t.Fatal("truncated context scheduled")
	}
}

func TestDisabledObserverDoesNotCaptureTerminalContext(t *testing.T) {
	_, o, p := observerFixture(t)
	p.mu.Lock()
	done := make(chan bool, 1)
	go func() { done <- o.runNext(context.Background(), time.Now()) }()
	select {
	case ran := <-done:
		p.mu.Unlock()
		if ran {
			t.Fatal("disabled observer ran")
		}
	case <-time.After(time.Second):
		p.mu.Unlock()
		<-done
		t.Fatal("disabled observer attempted to capture a locked terminal")
	}
}

func TestObserverStoreRoundTripsMaximumValidatedContent(t *testing.T) {
	s, o, p := observerFixture(t)
	content := ObserverContent{Summary: strings.Repeat("<", 2000)}
	for range 8 {
		content.Steps = append(content.Steps, strings.Repeat("<", 400))
		content.NextSteps = append(content.NextSteps, strings.Repeat("<", 400))
		content.Blockers = append(content.Blockers, strings.Repeat("<", 400))
	}
	envelope, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": content})
	if _, err := parseObserverOutput(envelope); err != nil {
		t.Fatal("fixture exceeds actual provider content limits", err)
	}
	for range maxSessions {
		id, _ := newID()
		o.state.Records[id] = observerRecord{SessionSummary: SessionSummary{ObserverContent: content, SessionID: id, SessionCreatedAt: p.meta.CreatedAt, Status: "ready", Provider: "claude", Model: "haiku"}}
	}
	if err := o.saveLocked(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(o.path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 1<<20 || info.Size() > observerStoreLimit {
		t.Fatalf("test did not exercise larger valid bounded state: %d", info.Size())
	}
	restored := newObserver(s)
	if restored.err != "" || len(restored.state.Records) != maxSessions {
		t.Fatal("valid maximum summaries could not be restored", restored.err)
	}
	for _, rec := range restored.state.Records {
		if rec.Summary != content.Summary || len(rec.Blockers) != 8 {
			t.Fatal("restored content changed")
		}
	}
}

func TestObserverLateCompletionCannotChangeReplacementIdentity(t *testing.T) {
	s, o, p := observerFixture(t)
	observerEnable(t, s, true)
	if !o.runNext(context.Background(), time.Now()) {
		t.Fatal("initial record absent")
	}
	source := s.observerSources()[p.meta.ID]
	staleSnapshot := s.observerSources()
	replacement := o.state.Records[p.meta.ID]
	replacement.SessionCreatedAt = replacement.SessionCreatedAt.Add(time.Second)
	replacement.Summary = "New identity summary."
	o.state.Records[p.meta.ID] = replacement
	o.finishRun(source, o.state.Config, ObserverContent{Summary: "Late old result."}, nil, false, time.Now(), staleSnapshot)
	got, ok := o.state.Records[p.meta.ID]
	if !ok || !got.SessionCreatedAt.Equal(replacement.SessionCreatedAt) || got.Summary != replacement.Summary {
		t.Fatal("old worker changed or deleted another session identity")
	}
}
