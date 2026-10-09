package controller

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) repairRuntime(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "local" {
		writeError(w, http.StatusBadRequest, "The local runtime uses the installed Relay bundle.")
		return
	}
	s.mu.Lock()
	host, exists := s.store.get(id)
	l := s.links[id]
	if !exists {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	if s.ctx.Err() != nil || host.Status != "online" || l == nil || l.conn == nil || !l.conn.Ready() {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "Connect to this host before repairing its runtime.")
		return
	}
	if l.repairing {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "Runtime repair is already running.")
		return
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "Other runtime repairs are still running. Try again shortly.")
		return
	}
	l.repairing = true
	s.store.runtimeOperation(id, &RuntimeOperation{Status: "running", Stage: "Verifying the bundled runtime"})
	s.connectWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.connectWG.Done()
		defer func() {
			<-s.repairSlots
			s.mu.Lock()
			l.repairing = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
		defer cancel()
		result, err := l.conn.Repair(ctx, s.config.BinaryDir, s.config.Version, func(stage string) {
			s.repairStage(id, l, &RuntimeOperation{Status: "running", Stage: stage})
		})
		if err != nil {
			s.repairStage(id, l, &RuntimeOperation{Status: "error", Stage: "Runtime repair failed", Error: err.Error()})
			return
		}
		warning := ""
		if err := l.conn.InstallAgentSkill(ctx); err != nil {
			warning = err.Error()
		}
		s.connectionWarning(id, l, warning)
		stage := "Runtime files verified and repaired. Sessions are still running."
		if result.RestartRequired {
			stage = "Runtime update installed for its next start. Active sessions remain on the current runtime."
		}
		s.repairStage(id, l, &RuntimeOperation{Status: "completed", Stage: stage, InstalledVersion: result.InstalledVersion, RunningVersion: result.RunningVersion, RestartRequired: result.RestartRequired})
	}()
	host, _ = s.store.get(id)
	writeJSON(w, http.StatusAccepted, host)
}

func (s *Server) repairStage(id string, l *link, operation *RuntimeOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links[id] == l {
		s.store.runtimeOperation(id, operation)
	}
}
