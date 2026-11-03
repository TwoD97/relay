package localagent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPatchSessionPreservesProcessOutputAndFinishedHistory(t *testing.T) {
	s, server := testService(t)
	meta := createFixture(t, s, server, "manual")
	p, _ := s.lookup(meta.ID)
	p.mu.Lock()
	proc, evidence := p.process, bytes.Clone(p.evidence)
	p.mu.Unlock()
	url := server.URL + "/api/sessions/" + meta.ID
	title := strings.Repeat("界", 40) // Exactly 120 UTF-8 bytes, matching Linux.
	status, data := requestJSON(t, url, "PATCH", map[string]string{"title": title, "workspace": "Updated workspace"})
	var updated Session
	if status != 200 || json.Unmarshal(data, &updated) != nil || updated.Title != title || updated.Workspace != "Updated workspace" || updated.ID != meta.ID || !updated.CreatedAt.Equal(meta.CreatedAt) || updated.MastermindID != meta.MastermindID || updated.Cwd != meta.Cwd || updated.AgentMode != meta.AgentMode {
		t.Fatalf("rename changed session identity: %d %s", status, data)
	}
	p.mu.Lock()
	same := p.process == proc && bytes.Equal(p.evidence, evidence)
	p.mu.Unlock()
	if !same {
		t.Fatal("rename changed the provider process")
	}
	status, _ = requestJSON(t, url+"/input", "POST", map[string]string{"data": "after rename\r"})
	if status != 204 {
		t.Fatal("renamed terminal rejected input", status)
	}
	awaitHistory(t, s, meta.ID, "RESULT after rename")
	status, _ = requestJSON(t, url+"/input", "POST", map[string]string{"data": "exit 7\r"})
	if status != 204 {
		t.Fatal("could not end fixture", status)
	}
	select {
	case <-p.done:
	case <-time.After(8 * time.Second):
		t.Fatal("fixture did not exit")
	}
	status, data = requestJSON(t, url, "PATCH", map[string]string{"title": "Saved result"})
	if status != 200 || json.Unmarshal(data, &updated) != nil || updated.Status != "exited" || updated.ExitCode == nil || *updated.ExitCode != 7 || updated.Workspace != "Updated workspace" {
		t.Fatalf("finished rename changed outcome: %d %s", status, data)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.stateDir, "new-controller")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.lookup(meta.ID)
	if err != nil || restored.snapshot().Title != "Saved result" || restored.snapshot().Status != "exited" {
		t.Fatal("renamed metadata did not survive service reopen", err)
	}
	awaitHistory(t, reopened, meta.ID, "RESULT after rename")
}

func TestPatchSessionRejectsInvalidLabelsAndFields(t *testing.T) {
	s, server := testService(t)
	meta := createFixture(t, s, server, "manual")
	url := server.URL + "/api/sessions/" + meta.ID
	for _, patch := range []map[string]string{
		{"title": ""}, {"title": "new\ncommand"}, {"title": strings.Repeat("界", 41)},
		{"workspace": strings.Repeat("x", 81)}, {"workspace": "\x00"}, {"cwd": t.TempDir()},
	} {
		status, _ := requestJSON(t, url, "PATCH", patch)
		if status != 400 {
			t.Fatalf("invalid patch accepted: %d", status)
		}
	}
	p, _ := s.lookup(meta.ID)
	if p.snapshot().Title != meta.Title || p.snapshot().Workspace != meta.Workspace {
		t.Fatal("invalid patch modified labels")
	}
	status, _ := requestJSON(t, server.URL+"/api/sessions/"+id(), "PATCH", map[string]string{"title": "Missing"})
	if status != 404 {
		t.Fatal("missing session did not return404", status)
	}
}

func TestPatchSessionSaveFailureDoesNotResurrectExitedProcess(t *testing.T) {
	s, server := testService(t)
	meta := createFixture(t, s, server, "manual")
	p, _ := s.lookup(meta.ID)
	// Stop only the periodic writer so the fixture can hold the durable write
	// behind another session lock. The real provider remains alive until below.
	s.cancel()
	<-s.flushed
	blocker := newSession(Session{ID: id(), Title: "Fixture", Workspace: "Test", Cwd: t.TempDir(), Harness: "codex", Status: "exited", CreatedAt: time.Now().UTC()})
	blocker.finished = true
	close(blocker.done)
	s.mu.Lock()
	s.sessions[blocker.meta.ID] = blocker
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.stateDir, "sessions.json")
	if err := os.Rename(file, file+".fixture-backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(file)
		if err := os.Rename(file+".fixture-backup", file); err != nil {
			t.Error(err)
		}
	}()
	blocker.mu.Lock()
	locked := true
	defer func() {
		if locked {
			blocker.mu.Unlock()
		}
	}()
	result := make(chan int, 1)
	go func() {
		status, _ := requestJSON(t, server.URL+"/api/sessions/"+meta.ID, "PATCH", map[string]string{"title": "Cannot persist"})
		result <- status
	}()
	deadline := time.Now().Add(5 * time.Second)
	for p.snapshot().Title != "Cannot persist" {
		if time.Now().After(deadline) {
			t.Fatal("patch did not reach the blocked writer")
		}
		time.Sleep(time.Millisecond)
	}
	p.stop(false) // Real owned process exit while PATCH holds the store lock.
	finished := p.snapshot()
	if finished.Status != "exited" || finished.ExitCode == nil {
		t.Fatal("fixture exit was not recorded")
	}
	blocker.mu.Unlock()
	locked = false
	select {
	case status := <-result:
		if status != 500 {
			t.Fatal("save failure reported success", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("patch did not complete after failed persistence")
	}
	after := p.snapshot()
	if after.Title != meta.Title || after.Workspace != meta.Workspace || after.Status != finished.Status || after.ExitCode == nil || *after.ExitCode != *finished.ExitCode || !after.UpdatedAt.Equal(finished.UpdatedAt) {
		t.Fatal("failed rename lost labels or resurrected the exited process")
	}
}
