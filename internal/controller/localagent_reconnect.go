package controller

import (
	"context"
	"time"
)

// Reconnect repairs only the daemon connection. Session intent/identity stays
// owned by the existing Mastermind state machine; no create or input is replayed.
func (s *Server) queueLocalAgent() bool {
	if s.config.LocalAgentDir == "" || s.config.EnsureLocalAgent == nil {
		return false
	}
	s.mu.Lock()
	if s.ctx.Err() != nil || s.localAgentConnecting {
		s.mu.Unlock()
		return false
	}
	s.localAgentConnecting = true
	s.connectWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.connectWG.Done()
		ctx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
		defer cancel()
		err := s.config.EnsureLocalAgent(ctx)
		s.mu.Lock()
		s.localAgentConnecting = false
		if err != nil {
			s.localAgentError = "Local Mastermind service needs attention: " + err.Error()
		} else {
			s.localAgentError = ""
		}
		s.mu.Unlock()
		if err == nil {
			s.wakeMastermind()
		}
	}()
	return true
}
