package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func statusPage(t *testing.T, s *Server) WorkspacePage {
	t.Helper()
	for _, p := range s.workspace.record.Pages {
		if p.Template == statusTemplate {
			return p
		}
	}
	t.Fatal("managed status page absent")
	return WorkspacePage{}
}

func TestStatusTemplateMigratesOnceAndPreservesUserWorkspace(t *testing.T) {
	s, h := workspaceFixture(t)
	page := workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": 0, "title": "Status", "html": "<h1>User-owned status</h1>"}), 201).Page
	task := workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/tasks", map[string]any{"revision": 1, "title": "User task", "status": "blocked", "detail": "Keep my notes", "pageId": page.ID}), 201).Task
	if err := s.ensureStatusTemplate(); err != nil {
		t.Fatal(err)
	}
	managed := statusPage(t, s)
	if managed.ID == page.ID || s.workspace.record.Revision != 3 || len(s.workspace.record.Pages) != 2 || s.workspace.record.Pages[0].HTML != page.HTML || s.workspace.record.Tasks[0] != *task {
		t.Fatal("migration changed user content")
	}
	before, _ := os.ReadFile(s.workspace.path)
	if err := s.ensureStatusTemplate(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.workspace.path)
	if string(before) != string(after) {
		t.Fatal("idempotent startup rewrote workspace")
	}
	restarted := &Server{config: s.config}
	if err := restarted.initializeWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ensureStatusTemplate(); err != nil {
		t.Fatal(err)
	}
	if statusPage(t, restarted).ID != managed.ID || restarted.workspace.record.Revision != 3 {
		t.Fatal("restart duplicated managed template")
	}
}

func TestStatusTemplateReadsAreLiveWithoutWritesAndTaskCASIsPreserved(t *testing.T) {
	s, h := workspaceFixture(t)
	if err := s.ensureStatusTemplate(); err != nil {
		t.Fatal(err)
	}
	managed := statusPage(t, s)
	s.mastermind = &mastermindStore{record: mastermindRecord{MastermindState: MastermindState{Enabled: true, Config: &MastermindConfig{HostID: "local", Harness: "codex", AgentMode: "manual", Cwd: "/fixture"}, Status: "running", UpdatedAt: time.Now().UTC()}}}
	read := func() WorkspacePage {
		t.Helper()
		w := workspaceCall(t, h, "GET", workspaceAPI+"/pages/"+managed.ID, nil)
		var p WorkspacePage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return p
	}
	before, _ := os.ReadFile(s.workspace.path)
	first := read()
	if first.Template != statusTemplate || !strings.Contains(first.HTML, "Process running") || !strings.Contains(first.HTML, "Ready for your next task") {
		t.Fatal("initial live status wrong")
	}
	after, _ := os.ReadFile(s.workspace.path)
	if string(before) != string(after) {
		t.Fatal("read changed stored workspace")
	}
	created := workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/tasks", map[string]any{"revision": 1, "title": "Verify <result>", "status": "doing", "detail": "Pending test evidence"}), 201)
	second := read()
	if second.Revision != 2 || !second.UpdatedAt.After(first.UpdatedAt) || !strings.Contains(second.HTML, "Verify &lt;result&gt;") {
		t.Fatal("task update did not refresh live page")
	}
	s.mastermind.mu.Lock()
	s.mastermind.record.Status = "waiting"
	s.mastermind.record.UpdatedAt = second.UpdatedAt.Add(time.Second)
	s.mastermind.mu.Unlock()
	third := read()
	if third.Revision != 2 || !third.UpdatedAt.After(second.UpdatedAt) || !strings.Contains(third.HTML, "Waiting for connection") {
		t.Fatal("coordinator observation changed CAS or did not refresh page")
	}
	index := workspaceCall(t, h, "GET", workspaceAPI, nil)
	var state WorkspaceState
	if json.Unmarshal(index.Body.Bytes(), &state) != nil || !state.Pages[0].UpdatedAt.Equal(third.UpdatedAt) {
		t.Fatal("index does not invalidate live page cache")
	}
	stale := workspaceCall(t, h, "PUT", workspaceAPI+"/tasks/"+created.Task.ID, map[string]any{"revision": 1, "title": "Wrong", "status": "done"})
	if stale.Code != 409 {
		t.Fatal("template refresh bypassed task CAS")
	}
	if s.workspace.record.Tasks[0].Status != "doing" {
		t.Fatal("process observation marked task complete")
	}
}

func TestStatusTemplateEditingAndDeletionEndAutomaticOwnership(t *testing.T) {
	for _, action := range []string{"edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			s, h := workspaceFixture(t)
			if err := s.ensureStatusTemplate(); err != nil {
				t.Fatal(err)
			}
			page := statusPage(t, s)
			if action == "edit" {
				workspaceResult(t, workspaceCall(t, h, "PUT", workspaceAPI+"/pages/"+page.ID, map[string]any{"revision": 1, "title": "My report", "html": "<h1>Keep this exact report</h1>"}), 200)
			} else {
				workspaceResult(t, workspaceCall(t, h, "DELETE", workspaceAPI+"/pages/"+page.ID, map[string]any{"revision": 1}), 200)
			}
			workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/tasks", map[string]any{"revision": 2, "title": "Later work", "status": "done"}), 201)
			restarted := &Server{config: s.config}
			if err := restarted.initializeWorkspace(); err != nil {
				t.Fatal(err)
			}
			if err := restarted.ensureStatusTemplate(); err != nil {
				t.Fatal(err)
			}
			if restarted.workspace.record.Revision != 3 {
				t.Fatal("removed template recreated")
			}
			if action == "edit" {
				if len(restarted.workspace.record.Pages) != 1 || restarted.workspace.record.Pages[0].Template != "" || restarted.workspace.record.Pages[0].HTML != "<h1>Keep this exact report</h1>" {
					t.Fatal("saved user HTML changed")
				}
			} else if len(restarted.workspace.record.Pages) != 0 {
				t.Fatal("deleted status returned")
			}
		})
	}
}

func TestStatusTemplateCapacityAndFailedPersistencePreserveContent(t *testing.T) {
	s, h := workspaceFixture(t)
	for i := 0; i < 16; i++ {
		workspaceResult(t, workspaceCall(t, h, "POST", workspaceAPI+"/pages", map[string]any{"revision": i, "title": fmt.Sprintf("User %d", i), "html": "keep"}), 201)
	}
	before, _ := os.ReadFile(s.workspace.path)
	if err := s.ensureStatusTemplate(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.workspace.path)
	if string(before) != string(after) || len(s.workspace.record.Pages) != 16 {
		t.Fatal("capacity migration discarded a page")
	}
	s2, _ := workspaceFixture(t)
	if err := os.Mkdir(s2.workspace.path, 0700); err != nil {
		t.Fatal(err)
	}
	if s2.ensureStatusTemplate() == nil || s2.workspace.record.Revision != 0 || len(s2.workspace.record.Pages) != 0 {
		t.Fatal("failed migration invented committed state")
	}
	if info, err := os.Stat(s2.workspace.path); err != nil || !info.IsDir() {
		t.Fatal("failed migration overwrote existing path")
	}
}

func TestControllerStartupAutomaticallySeedsStatus(t *testing.T) {
	s, ts, client := testController(t, "")
	login(t, s, client)
	r := request(t, client, "GET", ts.URL+workspaceAPI, "", "", ts.URL)
	var state WorkspaceState
	if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&state) != nil || state.Revision != 1 || len(state.Pages) != 1 || state.Pages[0].Template != statusTemplate || len(state.Tasks) != 0 {
		t.Fatal("controller did not expose initial live status page")
	}
}
