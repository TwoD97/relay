package localagent

import (
	"github.com/TwoD97/relay/internal/viewrequests"
	"net/http"
)

func (s *Server) registerViews(mux *http.ServeMux) {
	s.mu.Lock()
	if s.views == nil {
		s.views = viewrequests.New(func(ref viewrequests.Ref) bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			p := s.sessions[ref.SessionID]
			if p == nil || s.closed {
				return false
			}
			meta := p.snapshot()
			return meta.CreatedAt.Equal(ref.SessionCreatedAt) && meta.Purpose == ""
		})
	}
	queue := s.views
	s.mu.Unlock()
	queue.Register(mux)
}
