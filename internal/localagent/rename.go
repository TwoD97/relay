package localagent

import (
	"errors"
	"net/http"
	"os"
	"time"
)

// Match the Linux runtime's metadata-only PATCH contract. Changing a label
// never restarts a provider, changes its launch identity, or clears its output.
func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Title     *string `json:"title"`
		Workspace *string `json:"workspace"`
	}
	if err := decode(w, r, &patch); err != nil {
		fail(w, 400, err)
		return
	}
	if patch.Title != nil && !validLabel(*patch.Title, 120) || patch.Workspace != nil && !validLabel(*patch.Workspace, 80) {
		fail(w, 400, errors.New("labels must be nonempty UTF-8 without control characters; title is limited to 120 bytes and workspace to 80 bytes"))
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		fail(w, 503, errors.New("local agent service is stopping"))
		return
	}
	p := s.sessions[r.PathValue("id")]
	if p == nil {
		s.mu.Unlock()
		fail(w, 404, os.ErrNotExist)
		return
	}
	p.mu.Lock()
	previous := p.meta
	if patch.Title != nil {
		p.meta.Title = *patch.Title
	}
	if patch.Workspace != nil {
		p.meta.Workspace = *patch.Workspace
	}
	p.meta.UpdatedAt = time.Now().UTC()
	meta := p.meta
	p.mu.Unlock()
	err := s.persistLocked()
	if err != nil {
		p.mu.Lock()
		p.meta.Title, p.meta.Workspace = previous.Title, previous.Workspace
		// A provider can exit while persistence releases p.mu. Restore only
		// this mutation's fields, preserving its newer exit status/time.
		if p.meta.UpdatedAt.Equal(meta.UpdatedAt) {
			p.meta.UpdatedAt = previous.UpdatedAt
		}
		p.mu.Unlock()
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, errors.New("could not save the session name; existing labels were preserved"))
		return
	}
	jsonResponse(w, 200, meta)
}
