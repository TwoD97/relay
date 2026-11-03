package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"time"
)

// AgentHandler is registered only on the user-private controller IPC listener.
// It exposes an explicit operation allowlist, never the browser/admin router.
func (s *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	s.RegisterAgentWorkspaceRoutes(mux)
	mux.HandleFunc("GET /agent/fleet", func(w http.ResponseWriter, r *http.Request) {
		hosts := s.store.list()
		if s.hasLocalMastermind() {
			hosts = append([]Host{{ID: "local", Name: "This computer", Target: "local", Status: "online", Stage: "Local Mastermind"}}, hosts...)
		}
		writeJSON(w, 200, map[string]any{"hosts": hosts})
	})
	for _, route := range []string{"GET /agent/hosts/{id}/sessions", "POST /agent/hosts/{id}/sessions", "GET /agent/hosts/{id}/sessions/{session}/history", "POST /agent/hosts/{id}/sessions/{session}/input", "DELETE /agent/hosts/{id}/sessions/{session}"} {
		mux.HandleFunc(route, s.agentSession)
	}
	return s.WorkspaceAgentAuthorization(mux)
}

func (s *Server) agentSession(w http.ResponseWriter, r *http.Request) {
	hostID, sessionID := r.PathValue("id"), r.PathValue("session")
	if sessionID != "" {
		decoded, err := hex.DecodeString(sessionID)
		if err != nil || len(decoded) != 16 {
			writeError(w, 400, "Invalid session identity")
			return
		}
	}
	s.mu.Lock()
	l := s.links[hostID]
	host, exists := s.store.get(hostID)
	if hostID == "local" && s.hasLocalMastermind() {
		host, exists = Host{Status: "online"}, true
	}
	ready := exists && host.Status == "online" && l != nil && l.httpTransport != nil && !l.repairing
	var client *http.Client
	if ready {
		client = &http.Client{Transport: l.httpTransport}
	}
	s.mu.Unlock()
	if !ready {
		writeError(w, 503, "This machine is not connected. Reconnect it in Relay before continuing.")
		return
	}
	endpoint := "/api/sessions"
	var body any
	if sessionID != "" {
		endpoint += "/" + sessionID
	}
	if r.Method == "GET" && sessionID != "" {
		endpoint += "/history"
	}
	if r.Method == "POST" && sessionID == "" {
		var request struct {
			Harness   string `json:"harness"`
			Cwd       string `json:"cwd"`
			Workspace string `json:"workspace"`
			Title     string `json:"title"`
			AgentMode string `json:"agentMode,omitempty"`
		}
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		body = request
	} else if r.Method == "POST" {
		var request struct {
			Data string `json:"data"`
		}
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		body, endpoint = request, endpoint+"/input"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, client, r.Method, endpoint, body)
	if err != nil {
		status := 502
		var remote *runtimeHTTPError
		if errors.As(err, &remote) {
			status = remote.Status
		}
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" && sessionID != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "POST" && sessionID == "" {
		w.WriteHeader(201)
	}
	_, _ = w.Write(data)
}
