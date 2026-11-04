//go:build linux

package runtime

import (
	"errors"
	"net/http"
	"time"
)

// stopSession retains the exact session and its history. It does not replay
// terminal input or infer process state from the displayed text.
func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	var request struct {
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := decode(w, r, &request); err != nil || request.CreatedAt.IsZero() {
		fail(w, 400, errors.New("Exact session creation time is required"))
		return
	}
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	if !p.snapshot().CreatedAt.Equal(request.CreatedAt) {
		fail(w, 409, errors.New("Session identity changed; no process was stopped"))
		return
	}
	p.stop(false)
	s.mu.Lock()
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	jsonResponse(w, 200, p.snapshot())
}
