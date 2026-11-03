package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWorkspaceAgentCapabilityScopeAndRevocation(t *testing.T) {
	s, _ := workspaceFixture(t)
	launch := strings.Repeat("a", 32)
	s.mastermind = &mastermindStore{record: mastermindRecord{Token: launch, MastermindState: MastermindState{Enabled: true}}}
	path, err := s.PrepareWorkspaceAgentCapability(launch)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := readWorkspaceCapability(s.config.StateDir, launch)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareWorkspaceAgentCapability(launch); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("preparing an existing launch rotated its capability")
	}
	if runtime.GOOS != "windows" {
		for name, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("capability storage is not private")
			}
		}
	}
	mux := http.NewServeMux()
	s.RegisterAgentWorkspaceRoutes(mux)
	// Even an accidental extra handler cannot make /launch reachable through
	// this middleware; root installs only the explicit /agent/ allowlist.
	mux.HandleFunc("GET /launch", func(w http.ResponseWriter, _ *http.Request) { t.Error("browser login reached by agent capability") })
	h := s.WorkspaceAgentAuthorization(mux)
	call := func(path string, headers map[string][]string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+capability.Token)
		for name, values := range headers {
			r.Header[name] = values
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := call("/agent/mastermind/workspace", nil); code != 200 {
		t.Fatalf("valid agent denied: %d", code)
	}
	if code := call("/launch", nil); code != 404 {
		t.Fatalf("agent authorized browser login route: %d", code)
	}
	for _, headers := range []map[string][]string{
		{"Origin": {"http://127.0.0.1"}}, {"Origin": {""}}, {"Cookie": {"session=ignored"}},
		{"Sec-Fetch-Site": {"same-origin"}}, {"Sec-Fetch-Mode": {"cors"}},
	} {
		if code := call("/agent/mastermind/workspace", headers); code != 403 {
			t.Fatalf("browser credentials accepted: %d", code)
		}
	}
	for _, headers := range []map[string][]string{
		{"Authorization": {"Bearer " + strings.Repeat("b", 64)}},
		{"Authorization": {"Bearer " + capability.Token, "Bearer " + capability.Token}},
	} {
		if code := call("/agent/mastermind/workspace", headers); code != 401 {
			t.Fatalf("invalid capability accepted: %d", code)
		}
	}
	s.mastermind.mu.Lock()
	s.mastermind.record.Enabled = false
	s.mastermind.mu.Unlock()
	if code := call("/agent/mastermind/workspace", nil); code != 401 {
		t.Fatal("paused coordinator retained write authority")
	}
	s.mastermind.mu.Lock()
	s.mastermind.record.Enabled = true
	s.mastermind.record.Token = strings.Repeat("b", 32)
	s.mastermind.mu.Unlock()
	if _, err = s.PrepareWorkspaceAgentCapability(strings.Repeat("b", 32)); err != nil {
		t.Fatal(err)
	}
	if code := call("/agent/mastermind/workspace", nil); code != 401 {
		t.Fatal("replaced coordinator retained write authority")
	}
	// Restart preserves the capability only for the same current launch.
	restarted := &Server{config: s.config, mastermind: &mastermindStore{record: mastermindRecord{Token: launch, MastermindState: MastermindState{Enabled: true}}}}
	if err := restarted.initializeWorkspace(); err != nil {
		t.Fatal(err)
	}
	mux = http.NewServeMux()
	restarted.RegisterAgentWorkspaceRoutes(mux)
	h = restarted.WorkspaceAgentAuthorization(mux)
	if code := call("/agent/mastermind/workspace", nil); code != 200 {
		t.Fatal("restart lost active capability")
	}
}

func TestWorkspaceCapabilityConcurrentPrepareAndInvalidPreservation(t *testing.T) {
	s, _ := workspaceFixture(t)
	launch := strings.Repeat("c", 32)
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.PrepareWorkspaceAgentCapability(launch); errors <- err }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readWorkspaceCapability(s.config.StateDir, launch); err != nil {
		t.Fatal(err)
	}
	path := workspaceCapabilityPath(s.config.StateDir, launch)
	if err := os.WriteFile(path, []byte("partial secret: preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareWorkspaceAgentCapability(launch); err == nil {
		t.Fatal("invalid capability was replaced")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "partial secret: preserve" {
		t.Fatal("invalid capability bytes changed")
	}
	if _, err := s.PrepareWorkspaceAgentCapability("../escape"); err == nil {
		t.Fatal("untrusted identity accepted")
	}
}

func TestWorkspaceCapabilityRejectsSymlink(t *testing.T) {
	s, _ := workspaceFixture(t)
	launch := strings.Repeat("d", 32)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.config.StateDir, "agent-capabilities")); err != nil {
		t.Skip("symlink creation unavailable")
	}
	if _, err := s.PrepareWorkspaceAgentCapability(launch); err == nil {
		t.Fatal("linked capability directory accepted")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("linked external directory was changed")
	}
}
