package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/TwoD97/relay/internal/viewrequests"
)

func (s *Server) agentOpenView(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HostID string `json:"hostId"`
		viewrequests.Ref
	}
	if decodeJSON(w, r, &body) != nil || body.HostID == "" || !viewrequests.ValidRef(body.Ref) {
		writeError(w, 400, "An exact host and session identity are required")
		return
	}
	s.mu.Lock()
	l := s.links[body.HostID]
	host, ok := s.store.get(body.HostID)
	if body.HostID == "local" && s.hasLocalMastermind() {
		ok = true
		host.Status = "online"
	}
	ready := ok && host.Status == "online" && l != nil && l.httpTransport != nil && !l.repairing
	var client *http.Client
	if ready {
		client = &http.Client{Transport: l.httpTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	s.mu.Unlock()
	if !ready {
		writeError(w, 503, "Connect the selected machine before requesting a terminal window")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, client, "GET", "/api/sessions", nil)
	if err != nil {
		writeError(w, 502, "Could not verify the terminal session. No window was requested.")
		return
	}
	var sessions []struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
		Purpose   string    `json:"purpose"`
	}
	found := false
	if json.Unmarshal(data, &sessions) == nil {
		for _, session := range sessions {
			if session.ID == body.SessionID && session.CreatedAt.Equal(body.SessionCreatedAt) && session.Purpose == "" {
				found = true
				break
			}
		}
	}
	if !found {
		writeError(w, 409, "The terminal identity is stale or is a setup session; refresh sessions before continuing")
		return
	}
	s.mu.Lock()
	current := s.links[body.HostID] == l && s.ctx.Err() == nil
	s.mu.Unlock()
	if !current {
		writeError(w, 503, "The machine connection changed. No window was requested.")
		return
	}
	data, err = runtimeJSON(ctx, client, "POST", "/api/views", body.Ref)
	if err != nil {
		status := 502
		var remote *runtimeHTTPError
		if errors.As(err, &remote) {
			status = remote.Status
		}
		writeError(w, status, "Could not confirm the window request. Older runtimes require an update; do not replay an unknown result.")
		return
	}
	var result viewrequests.Request
	if json.Unmarshal(data, &result) != nil || !viewrequests.SameRef(result.Ref, body.Ref) || result.Status != "pending" || !viewrequests.ValidID(result.ID) {
		writeError(w, 502, "Invalid window request response; do not repeat an unknown result")
		return
	}
	writeJSON(w, 202, result)
}
