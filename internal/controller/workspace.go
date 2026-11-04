package controller

import (
	"errors"
	"io"
	"net/http"
	"time"
)

type workspaceRequest struct {
	Revision  *uint64 `json:"revision"`
	ID        string  `json:"id,omitempty"`
	Title     string  `json:"title"`
	HTML      string  `json:"html,omitempty"`
	Detail    string  `json:"detail,omitempty"`
	Status    string  `json:"status,omitempty"`
	PageID    string  `json:"pageId,omitempty"`
	HostID    string  `json:"hostId,omitempty"`
	SessionID string  `json:"sessionId,omitempty"`
}

func (s *Server) RegisterWorkspaceRoutes(mux *http.ServeMux) {
	s.registerWorkspaceRoutes(mux, "/api/mastermind/workspace")
}

// These routes must only be installed behind WorkspaceAgentAuthorization on
// the private controller IPC listener, never on the browser listener directly.
func (s *Server) RegisterAgentWorkspaceRoutes(mux *http.ServeMux) {
	s.registerWorkspaceRoutes(mux, "/agent/mastermind/workspace")
}

func (s *Server) registerWorkspaceRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix, s.getWorkspace)
	mux.HandleFunc("GET "+prefix+"/pages/{id}", s.getWorkspacePage)
	mux.HandleFunc("GET "+prefix+"/tasks/{id}", s.getWorkspaceTask)
	for _, kind := range []string{"pages", "tasks"} {
		mux.HandleFunc("POST "+prefix+"/"+kind, func(w http.ResponseWriter, r *http.Request) { s.mutateWorkspace(w, r, kind, true) })
		mux.HandleFunc("PUT "+prefix+"/"+kind+"/{id}", func(w http.ResponseWriter, r *http.Request) { s.mutateWorkspace(w, r, kind, false) })
		mux.HandleFunc("DELETE "+prefix+"/"+kind+"/{id}", func(w http.ResponseWriter, r *http.Request) { s.mutateWorkspace(w, r, kind, false) })
	}
}

func (s *Server) lockedWorkspace(w http.ResponseWriter) (*workspaceStore, bool) {
	if s.workspace == nil {
		writeError(w, 503, workspaceUnavailable)
		return nil, false
	}
	store := s.workspace
	store.mu.Lock()
	if store.err != nil {
		writeError(w, 503, store.err.Error())
		store.mu.Unlock()
		return nil, false
	}
	return store, true
}

func (s *Server) getWorkspace(w http.ResponseWriter, _ *http.Request) {
	mastermind := s.statusMastermind()
	store, ok := s.lockedWorkspace(w)
	if !ok {
		return
	}
	defer store.mu.Unlock()
	writeJSON(w, 200, liveWorkspaceState(store.record, mastermind))
}

func (s *Server) getWorkspacePage(w http.ResponseWriter, r *http.Request) {
	mastermind := s.statusMastermind()
	store, ok := s.lockedWorkspace(w)
	if !ok {
		return
	}
	defer store.mu.Unlock()
	for _, page := range store.record.Pages {
		if page.ID == r.PathValue("id") {
			page.Revision = store.record.Revision
			page, err := renderStatusPage(store.record, mastermind, page)
			if err != nil {
				writeError(w, 500, "The status page could not be rendered. Saved workspace content is unchanged.")
				return
			}
			writeJSON(w, 200, page)
			return
		}
	}
	writeError(w, 404, "Workspace page not found")
}

func (s *Server) getWorkspaceTask(w http.ResponseWriter, r *http.Request) {
	store, ok := s.lockedWorkspace(w)
	if !ok {
		return
	}
	defer store.mu.Unlock()
	for _, task := range store.record.Tasks {
		if task.ID == r.PathValue("id") {
			writeJSON(w, 200, struct {
				WorkspaceTask
				Revision uint64 `json:"revision"`
			}{task, store.record.Revision})
			return
		}
	}
	writeError(w, 404, "Workspace task not found")
}

func decodeWorkspaceRequest(w http.ResponseWriter, r *http.Request, kind string) (workspaceRequest, error) {
	// JSON escaping can make a valid 128 KiB HTML string larger on the wire.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	data, err := io.ReadAll(r.Body)
	var request workspaceRequest
	if err != nil || strictWorkspaceJSON(data, &request) != nil || request.Revision == nil || *request.Revision > maxMastermindRevision {
		return request, errors.New("Invalid workspace request or missing revision")
	}
	if r.Method == http.MethodDelete {
		var deletion struct {
			Revision *uint64 `json:"revision"`
		}
		if strictWorkspaceJSON(data, &deletion) != nil {
			return request, errors.New("Delete accepts only revision")
		}
	} else if kind == "pages" {
		var page struct {
			Revision *uint64 `json:"revision"`
			ID       string  `json:"id,omitempty"`
			Title    string  `json:"title"`
			HTML     string  `json:"html"`
		}
		if strictWorkspaceJSON(data, &page) != nil {
			return request, errors.New("Invalid page fields")
		}
	} else {
		var task struct {
			Revision  *uint64 `json:"revision"`
			ID        string  `json:"id,omitempty"`
			Title     string  `json:"title"`
			Detail    string  `json:"detail,omitempty"`
			Status    string  `json:"status,omitempty"`
			PageID    string  `json:"pageId,omitempty"`
			HostID    string  `json:"hostId,omitempty"`
			SessionID string  `json:"sessionId,omitempty"`
		}
		if strictWorkspaceJSON(data, &task) != nil {
			return request, errors.New("Invalid task fields")
		}
	}
	return request, nil
}

func (s *Server) mutateWorkspace(w http.ResponseWriter, r *http.Request, kind string, create bool) {
	mastermind := s.statusMastermind()
	request, err := decodeWorkspaceRequest(w, r, kind)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	id := r.PathValue("id")
	if create {
		id = request.ID
		if id == "" {
			id = randomID()[:32]
		}
	} else if request.ID != "" && request.ID != id {
		writeError(w, 400, "Workspace item ID does not match the request path")
		return
	}
	if !validMastermindID(id) {
		writeError(w, 400, "Workspace ID must be 32 lowercase hexadecimal characters")
		return
	}
	deleting := r.Method == http.MethodDelete
	if !deleting && (!validWorkspaceTitle(request.Title) || kind == "pages" && !validWorkspaceHTML(request.HTML)) {
		writeError(w, 422, "Use a nonempty title up to 160 characters and HTML up to 128 KiB")
		return
	}
	if kind == "tasks" && request.Status == "" {
		request.Status = "todo"
	}
	item := WorkspaceTask{ID: id, Title: request.Title, Detail: request.Detail, Status: request.Status, PageID: request.PageID, HostID: request.HostID, SessionID: request.SessionID}
	if kind == "tasks" && !deleting && !validWorkspaceTask(item) {
		writeError(w, 422, "Invalid task fields; detail is limited to 4 KiB and status is todo, doing, blocked, or done")
		return
	}
	store, ok := s.lockedWorkspace(w)
	if !ok {
		return
	}
	defer store.mu.Unlock()
	if store.record.FrozenBy != "" {
		writeError(w, 409, "This workspace moved to another coordinator. Refresh Relay before editing.")
		return
	}
	if *request.Revision != store.record.Revision {
		writeError(w, 409, "Workspace changed. Read the current revision before making another change.")
		return
	}
	if store.record.Revision >= maxMastermindRevision {
		writeError(w, 409, "Workspace revision limit reached")
		return
	}
	next := cloneWorkspace(store.record)
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	response := WorkspaceMutation{}
	if kind == "pages" {
		index := -1
		for i, page := range next.Pages {
			if page.ID == id {
				index = i
				break
			}
		}
		if create && index >= 0 {
			writeError(w, 409, "Page ID already exists; read that page to reconcile the earlier request")
			return
		}
		if !create && index < 0 {
			writeError(w, 404, "Workspace page not found")
			return
		}
		if create && len(next.Pages) >= maxWorkspacePages {
			writeError(w, 422, "Workspace is limited to 16 pages")
			return
		}
		if deleting {
			next.Pages = append(next.Pages[:index], next.Pages[index+1:]...)
			for i := range next.Tasks {
				if next.Tasks[i].PageID == id {
					next.Tasks[i].PageID = ""
					next.Tasks[i].UpdatedAt = next.UpdatedAt
				}
			}
		} else {
			page := WorkspacePage{WorkspacePageSummary: WorkspacePageSummary{ID: id, Title: request.Title, UpdatedAt: next.UpdatedAt}, HTML: request.HTML, Revision: next.Revision}
			if create {
				next.Pages = append(next.Pages, page)
			} else {
				next.Pages[index] = page
			}
			response.Page = &page
		}
	} else {
		index := -1
		for i, task := range next.Tasks {
			if task.ID == id {
				index = i
				break
			}
		}
		if create && index >= 0 {
			writeError(w, 409, "Task ID already exists; read that task to reconcile the earlier request")
			return
		}
		if !create && index < 0 {
			writeError(w, 404, "Workspace task not found")
			return
		}
		if create && len(next.Tasks) >= maxWorkspaceTasks {
			writeError(w, 422, "Workspace is limited to 200 tasks")
			return
		}
		if item.PageID != "" && !deleting {
			found := false
			for _, page := range next.Pages {
				if page.ID == item.PageID {
					found = true
					break
				}
			}
			if !found {
				writeError(w, 422, "Task page does not exist")
				return
			}
		}
		if deleting {
			next.Tasks = append(next.Tasks[:index], next.Tasks[index+1:]...)
		} else {
			item.UpdatedAt = next.UpdatedAt
			if create {
				next.Tasks = append(next.Tasks, item)
			} else {
				next.Tasks[index] = item
			}
			response.Task = &item
		}
	}
	if err := store.saveLocked(next); err != nil {
		if errors.Is(err, errWorkspaceSize) {
			writeError(w, 422, err.Error())
			return
		}
		writeError(w, 500, "Workspace save could not be confirmed. Read the current state before retrying; do not repeat an unknown create.")
		return
	}
	response.Workspace = liveWorkspaceState(next, mastermind)
	code := http.StatusOK
	if create {
		code = http.StatusCreated
	}
	writeJSON(w, code, response)
}
