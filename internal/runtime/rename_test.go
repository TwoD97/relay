//go:build linux

package runtime

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPatchSessionSaveFailurePreservesConcurrentRealExit(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "printf ready; IFS= read -r value; exit 7")
	ready(t, p)
	before := p.snapshot()
	s.cancel()
	<-s.flushDone
	blockerID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	blocker := newLiveSession(Session{ID: blockerID, Title: "Fixture", Workspace: "Tests", Harness: "shell", Cwd: s.stateDir, Status: "exited", CreatedAt: time.Now().UTC()})
	blocker.finished = true
	close(blocker.done)
	s.mu.Lock()
	s.sessions[blockerID] = blocker
	err = s.persistLocked()
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
		response := httptest.NewRecorder()
		request := httptest.NewRequest("PATCH", "/api/sessions/"+before.ID, strings.NewReader(`{"title":"Cannot persist"}`))
		s.Handler().ServeHTTP(response, request)
		result <- response.Code
	}()
	eventually(t, func() bool { return p.snapshot().Title == "Cannot persist" })
	// The original shell finishes while PATCH is stalled at its metadata write.
	// finish() must remain authoritative even when the rename then fails.
	if err := p.writeInput("finish\n"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return p.snapshot().Status == "exited" })
	finished := p.snapshot()
	blocker.mu.Unlock()
	locked = false
	select {
	case status := <-result:
		if status != 500 {
			t.Fatal("failed persistence reported success", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed rename did not finish")
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("original shell was not reaped")
	}
	after := p.snapshot()
	if after.Title != before.Title || after.Workspace != before.Workspace || after.Status != "exited" || after.ExitCode == nil || *after.ExitCode != 7 || !after.UpdatedAt.Equal(finished.UpdatedAt) || after.ProcessIdentity != finished.ProcessIdentity {
		t.Fatal("failed rename overwrote the real process exit")
	}
}
