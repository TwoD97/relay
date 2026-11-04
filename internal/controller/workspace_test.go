package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const workspaceAPI = "/api/mastermind/workspace"

func workspaceFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := &Server{config: Config{StateDir: t.TempDir()}}
	if err := s.initializeWorkspace(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterWorkspaceRoutes(mux)
	return s, mux
}

func workspaceCall(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func workspaceResult(t *testing.T, w *httptest.ResponseRecorder, code int) WorkspaceMutation {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d want %d: %s", w.Code, code, w.Body.String())
	}
	var result WorkspaceMutation
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWorkspaceCRUDCASAndRestart(t *testing.T) {
	s, h := workspaceFixture(t)
	pageID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	one := workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": 0, "id": pageID, "title": "Plan", "html": "<h1>Private draft</h1>"}), 201)
	if one.Workspace.Revision != 1 || one.Page == nil || one.Page.ID != pageID || strings.Contains(string(mustWorkspaceJSON(t, one.Workspace)), "Private draft") {
		t.Fatal("creation lost identity or leaked HTML in index")
	}
	two := workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/tasks", map[string]any{"revision": 1, "id": taskID, "title": "Review", "detail": "Read the result", "pageId": pageID, "hostId": "test-host", "sessionId": strings.Repeat("c", 32)}), 201)
	if two.Task == nil || two.Task.Status != "todo" || two.Workspace.Revision != 2 {
		t.Fatal("task defaults or revision changed")
	}
	page := workspaceCall(t, h, "GET", workspaceAPI+"/pages/"+pageID, nil)
	var got WorkspacePage
	if json.Unmarshal(page.Body.Bytes(), &got) != nil || got.Revision != 2 || got.HTML != one.Page.HTML {
		t.Fatal("page did not return current global revision")
	}
	before, _ := os.ReadFile(s.workspace.path)
	stale := workspaceCall(t, h, "PUT", workspaceAPI+"/pages/"+pageID, map[string]any{"revision": 1, "title": "stale", "html": "lost"})
	after, _ := os.ReadFile(s.workspace.path)
	if stale.Code != 409 || string(before) != string(after) {
		t.Fatal("stale writer changed saved state")
	}
	duplicate := workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": 2, "id": pageID, "title": "duplicate", "html": "lost"})
	if duplicate.Code != 409 {
		t.Fatal("unknown create could be duplicated")
	}
	three := workspaceResult(t, workspaceCall(t, h, "DELETE", workspaceAPI+"/pages/"+pageID, map[string]any{"revision": 2}), 200)
	if len(three.Workspace.Pages) != 0 || len(three.Workspace.Tasks) != 1 || three.Workspace.Tasks[0].PageID != "" || three.Workspace.Tasks[0].ID != taskID {
		t.Fatal("page deletion removed its task instead of detaching it")
	}
	restarted := &Server{config: s.config}
	if err := restarted.initializeWorkspace(); err != nil {
		t.Fatal(err)
	}
	if state := workspaceState(restarted.workspace.record); state.Revision != 3 || len(state.Tasks) != 1 || state.Tasks[0].Title != "Review" {
		t.Fatal("restart lost committed state")
	}
}

func mustWorkspaceJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWorkspaceConcurrentWritersHaveSingleWinner(t *testing.T) {
	s, h := workspaceFixture(t)
	const count = 12
	results := make(chan int, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			data := fmt.Sprintf(`{"revision":0,"id":"%032x","title":"Task %d"}`, i+1, i)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", workspaceAPI+"/tasks", strings.NewReader(data)))
			results <- w.Code
		}(i)
	}
	group.Wait()
	close(results)
	success := 0
	for code := range results {
		if code == 201 {
			success++
		} else if code != 409 {
			t.Fatalf("unexpected concurrency response %d", code)
		}
	}
	if success != 1 || s.workspace.record.Revision != 1 || len(s.workspace.record.Tasks) != 1 {
		t.Fatal("concurrent CAS lost or duplicated an update")
	}
}

func TestWorkspaceInvalidDataAndLimits(t *testing.T) {
	s, h := workspaceFixture(t)
	for _, tc := range []struct {
		name, path, body string
		code             int
	}{
		{"unknown", "/pages", `{"revision":0,"title":"x","html":"","secret":true}`, 400},
		{"missing-revision", "/pages", `{"title":"x","html":""}`, 400},
		{"trailing", "/pages", `{"revision":0,"title":"x"}{}`, 400},
		{"oversize-wire", "/pages", strings.Repeat(" ", 1<<20) + `{}`, 400},
		{"oversize-html", "/pages", string(mustWorkspaceJSON(t, map[string]any{"revision": 0, "title": "x", "html": strings.Repeat("x", maxWorkspaceHTML+1)})), 422},
		{"traversal-id", "/pages", `{"revision":0,"id":"../outside","title":"x"}`, 400},
		{"task-page-missing", "/tasks", `{"revision":0,"title":"x","pageId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, 422},
		{"task-state", "/tasks", `{"revision":0,"title":"x","status":"approved"}`, 422},
		{"session-host-missing", "/tasks", `{"revision":0,"title":"x","sessionId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, 422},
		{"task-html", "/tasks", `{"revision":0,"title":"x","html":"hidden"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", workspaceAPI+tc.path, strings.NewReader(tc.body)))
			if w.Code != tc.code {
				t.Fatalf("got %d want %d", w.Code, tc.code)
			}
		})
	}
	if s.workspace.record.Revision != 0 {
		t.Fatal("invalid request mutated workspace")
	}
	for i := 0; i < maxWorkspacePages; i++ {
		workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": i, "title": "Page", "html": ""}), 201)
	}
	if w := workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": maxWorkspacePages, "title": "one too many"}); w.Code != 422 {
		t.Fatal("page count was not bounded")
	}
}

func TestWorkspacePreservesCorruptStateAndFailedWrite(t *testing.T) {
	for _, content := range []string{"not-json", `{"format":99}`, strings.Repeat("x", maxWorkspaceBytes+1)} {
		dir := t.TempDir()
		path := filepath.Join(dir, "workspace.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		s := &Server{config: Config{StateDir: dir}}
		if s.initializeWorkspace() == nil {
			t.Fatal("invalid workspace accepted")
		}
		mux := http.NewServeMux()
		s.RegisterWorkspaceRoutes(mux)
		if w := workspaceCall(t, mux, "GET", workspaceAPI, nil); w.Code != 503 {
			t.Fatal("invalid workspace not isolated")
		}
		if w := workspaceCall(t, mux, "POST", workspaceAPI+"/pages", map[string]any{"revision": 0, "title": "overwrite"}); w.Code != 503 {
			t.Fatal("invalid workspace was overwritten")
		}
		data, _ := os.ReadFile(path)
		if string(data) != content {
			t.Fatal("corrupt source bytes changed")
		}
	}
	s, h := workspaceFixture(t)
	original := s.workspace.path
	s.workspace.path = filepath.Join(s.config.StateDir, "blocked")
	if err := os.Mkdir(s.workspace.path, 0700); err != nil {
		t.Fatal(err)
	}
	if w := workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": 0, "title": "not saved"}); w.Code != 500 {
		t.Fatal("disk failure reported success")
	}
	if s.workspace.record.Revision != 0 || len(s.workspace.record.Pages) != 0 {
		t.Fatal("failed save published uncommitted state")
	}
	s.workspace.path = original
	workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": 0, "title": "saved"}), 201)
}

func TestWorkspaceRejectsSymlinkState(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "outside")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "workspace.json")); err != nil {
		t.Skip("symlink creation unavailable")
	}
	s := &Server{config: Config{StateDir: dir}}
	if s.initializeWorkspace() == nil {
		t.Fatal("symlink state accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "preserve" {
		t.Fatal("symlink target changed")
	}
}

func TestWorkspaceBrowserRoutesKeepExistingAuthentication(t *testing.T) {
	s, server, client := testController(t, "")
	if got := request(t, client, "GET", server.URL+workspaceAPI, "", "", ""); got.StatusCode != 401 {
		t.Fatal("workspace bypassed browser authentication")
	}
	login(t, s, client)
	body := `{"revision":1,"title":"Private page","html":"<script>not executed by the API</script>"}`
	if got := request(t, client, "POST", server.URL+workspaceAPI+"/pages", body, "", server.URL); got.StatusCode != 403 {
		t.Fatal("workspace mutation bypassed CSRF")
	}
	if got := request(t, client, "POST", server.URL+workspaceAPI+"/pages", body, s.csrf, "https://untrusted.example"); got.StatusCode != 403 {
		t.Fatal("cross-origin workspace mutation accepted")
	}
	if got := request(t, client, "POST", server.URL+workspaceAPI+"/pages", body, s.csrf, server.URL); got.StatusCode != 201 {
		t.Fatalf("authenticated workspace mutation failed: %d", got.StatusCode)
	}
	if got := request(t, client, "GET", server.URL+workspaceAPI, "", "", server.URL); got.StatusCode != 200 || got.Header.Get("Content-Type") != "application/json" {
		t.Fatal("workspace returned active HTML instead of JSON")
	}
	if got := request(t, client, "GET", server.URL+"/agent/mastermind/workspace", "", "", server.URL); got.StatusCode != 404 {
		t.Fatal("agent interface exposed on browser listener")
	}
}

func TestWorkspaceTaskUpdateDeleteAndStoreBounds(t *testing.T) {
	s, h := workspaceFixture(t)
	created := workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/tasks", map[string]any{"revision": 0, "title": "Draft", "detail": "old", "status": "doing"}), 201)
	id := created.Task.ID
	updated := workspaceResult(t, workspaceCall(t, h, "PUT", workspaceAPI+"/tasks/"+id, map[string]any{"revision": 1, "title": "Reviewed", "detail": "new", "status": "done"}), 200)
	if updated.Task.Title != "Reviewed" || updated.Task.Status != "done" || updated.Task.Detail != "new" {
		t.Fatal("full task update was not saved")
	}
	w := workspaceCall(t, h, "GET", workspaceAPI+"/tasks/"+id, nil)
	var got struct {
		WorkspaceTask
		Revision uint64 `json:"revision"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Revision != 2 || got.Status != "done" {
		t.Fatal("task reconciliation did not return current revision")
	}
	deleted := workspaceResult(t, workspaceCall(t, h, "DELETE", workspaceAPI+"/tasks/"+id, map[string]any{"revision": 2}), 200)
	if len(deleted.Workspace.Tasks) != 0 || deleted.Workspace.Revision != 3 {
		t.Fatal("delete did not commit")
	}
	if w = workspaceCall(t, h, "GET", workspaceAPI+"/tasks/"+id, nil); w.Code != 404 {
		t.Fatal("deleted task still available")
	}
	// Seed a full valid record to exercise API bounds without 200 disk syncs.
	record := cloneWorkspace(s.workspace.record)
	for i := 0; i < maxWorkspaceTasks; i++ {
		record.Tasks = append(record.Tasks, WorkspaceTask{ID: fmt.Sprintf("%032x", i+1), Title: "Task", Status: "todo", UpdatedAt: time.Now().UTC()})
	}
	if err := s.workspace.saveLocked(record); err != nil {
		t.Fatal(err)
	}
	if w = workspaceCall(t, h, "POST", workspaceAPI+"/tasks", map[string]any{"revision": 3, "title": "Overflow"}); w.Code != 422 {
		t.Fatal("task limit not enforced")
	}
	// JSON escapes control bytes; enforce the total on serialized bytes too.
	oversize := cloneWorkspace(record)
	for i := 0; i < maxWorkspacePages; i++ {
		oversize.Pages = append(oversize.Pages, WorkspacePage{WorkspacePageSummary: WorkspacePageSummary{ID: fmt.Sprintf("%032x", i+1), Title: "Page", UpdatedAt: time.Now().UTC()}, Revision: 3, HTML: strings.Repeat("\x01", maxWorkspaceHTML)})
	}
	if err := s.workspace.saveLocked(oversize); !errors.Is(err, errWorkspaceSize) {
		t.Fatalf("encoded storage limit ignored: %v", err)
	}
	if len(s.workspace.record.Pages) != 0 || len(s.workspace.record.Tasks) != maxWorkspaceTasks {
		t.Fatal("oversize save published uncommitted state")
	}
}
