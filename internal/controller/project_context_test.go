package controller

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TwoD97/relay/internal/projectcontext"
)

func TestProjectContextRequiresAuthenticationCSRFAndEnabledHost(t *testing.T) {
	s, ts, client := testController(t, "")
	endpoint := ts.URL + "/api/hosts/local/project-context"
	body := `{"path":"/tmp"}`
	if response := request(t, client, "POST", endpoint, body, s.csrf, ts.URL); response.StatusCode != 401 {
		t.Fatal("unauthenticated project mutation", response.StatusCode)
	}
	login(t, s, client)
	if response := request(t, client, "POST", endpoint, body, "", ts.URL); response.StatusCode != 403 {
		t.Fatal("CSRF bypass", response.StatusCode)
	}
	if response := request(t, client, "POST", endpoint, body, s.csrf, "https://other.example"); response.StatusCode != 403 {
		t.Fatal("cross-origin mutation", response.StatusCode)
	}
	if response := request(t, client, "POST", endpoint, body, s.csrf, ts.URL); response.StatusCode != 404 {
		t.Fatal("disabled local host accepted", response.StatusCode)
	}
	for _, input := range []string{`null`, `{}`, `{"path":"relative"}`, `{"path":"/tmp/../other"}`, `{"path":"/tmp","force":true}`, `{"path":"/tmp"} {}`} {
		if response := request(t, client, "POST", endpoint, input, s.csrf, ts.URL); response.StatusCode != 400 {
			t.Fatalf("invalid request %s: %d", input, response.StatusCode)
		}
	}
}

func TestProjectContextLocalWorksWithoutRuntimeAndPreservesNotes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("project file writer runs on Linux hosts")
	}
	s, ts, client := testController(t, filepath.Join(t.TempDir(), "absent.sock"))
	login(t, s, client)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("existing notes"), 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(projectcontext.Request{Path: dir})
	endpoint := ts.URL + "/api/hosts/local/project-context"
	for range 2 {
		response := request(t, client, "POST", endpoint, string(input), s.csrf, ts.URL)
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("prepare: %d %s", response.StatusCode, data)
		}
		result, err := parseProjectContextResult(data)
		if err != nil || result.Path != dir {
			t.Fatal(result, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if string(data) != "existing notes" {
		t.Fatal("changed existing memory")
	}
	s.mu.Lock()
	s.links["local"].repairing = true
	s.mu.Unlock()
	if response := request(t, client, "POST", endpoint, string(input), s.csrf, ts.URL); response.StatusCode != 409 {
		t.Fatal("concurrent host preparation was accepted", response.StatusCode)
	}
	s.mu.Lock()
	s.links["local"].repairing = false
	s.mu.Unlock()
	for range cap(s.repairSlots) {
		s.repairSlots <- struct{}{}
	}
	if response := request(t, client, "POST", endpoint, string(input), s.csrf, ts.URL); response.StatusCode != 429 {
		t.Fatal("ignored preparation concurrency limit", response.StatusCode)
	}
	for range cap(s.repairSlots) {
		<-s.repairSlots
	}
}

func TestProjectContextResultRejectsIncompleteOrUnexpectedFiles(t *testing.T) {
	for _, input := range []string{`{}`, `{"path":"/tmp","files":[]}`, `{"path":"/tmp","files":[{"path":"../private","status":"created"}]}`, strings.Repeat(" ", 32<<10) + `{}`} {
		if _, err := parseProjectContextResult([]byte(input)); err == nil {
			t.Fatal("invalid worker response accepted")
		}
	}
}
