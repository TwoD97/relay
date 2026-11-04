package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/TwoD97/relay/internal/conversations"
)

func conversationRequest(w http.ResponseWriter, r *http.Request) (conversations.Request, error) {
	q := conversations.Request{Operation: "list"}
	if r.Method == http.MethodPatch {
		var body struct {
			Harness string `json:"harness"`
			Cwd     string `json:"cwd"`
			Title   string `json:"title"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return q, err
		}
		q.Operation, q.ID, q.Harness, q.Cwd, q.Title = "rename", r.PathValue("conversation"), body.Harness, body.Cwd, body.Title
	} else {
		if len(r.URL.RawQuery) > 16384 {
			return q, errors.New("Conversation query is too long")
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return q, errors.New("Invalid conversation query")
		}
		for key, list := range values {
			if (key != "harness" && key != "cwd") || len(list) != 1 {
				return q, errors.New("Invalid conversation query")
			}
		}
		q.Harness, q.Cwd = values.Get("harness"), values.Get("cwd")
	}
	return q, q.Validate()
}

func (s *Server) hostConversations(w http.ResponseWriter, r *http.Request) {
	request, err := conversationRequest(w, r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	host, exists := s.store.get(id)
	local := id == "local" && s.config.LocalSocket != ""
	if local {
		exists = true
		host.Status = "online"
	}
	l := s.links[id]
	if !exists {
		s.mu.Unlock()
		writeError(w, 404, "Linux host not found")
		return
	}
	if s.ctx.Err() != nil || host.Status != "online" || l == nil || l.httpTransport == nil || (!local && (l.conn == nil || !l.conn.Ready())) {
		s.mu.Unlock()
		writeError(w, 503, "Connect to this host to read saved conversations")
		return
	}
	if l.repairing {
		s.mu.Unlock()
		writeError(w, 409, "Relay files are being prepared. Try again when that operation finishes.")
		return
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		writeError(w, 429, "Conversation lookup is busy. Try again shortly.")
		return
	}
	l.repairing = true
	s.connectWG.Add(1)
	s.mu.Unlock()
	defer s.connectWG.Done()
	defer func() { <-s.repairSlots; s.mu.Lock(); l.repairing = false; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	var data []byte
	if local {
		data, err = localConversations(ctx, s.config.LocalSocket, request)
	} else {
		err = l.conn.RefreshCLI(ctx, s.config.BinaryDir, s.config.Version)
		if err == nil {
			payload, _ := json.Marshal(request)
			data, err = l.conn.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" conversations --json-stdin`, bytes.NewReader(payload))
		}
	}
	s.mu.Lock()
	current := s.links[id] == l
	s.mu.Unlock()
	if !current {
		writeError(w, 503, "The host connection changed. A requested rename may already be saved; refresh before trying again.")
		return
	}
	if err != nil {
		message := "Could not read saved conversations. Check the project path and provider installation."
		if request.Operation == "rename" {
			message = "Could not confirm the conversation rename. Refresh the list before trying again; no request was replayed."
		}
		if local {
			message = err.Error()
		}
		writeError(w, 502, message)
		return
	}
	if len(data) > 128<<10 {
		writeError(w, 502, "Conversation metadata response exceeded its limit")
		return
	}
	if request.Operation == "rename" {
		var entry conversations.Entry
		if json.Unmarshal(data, &entry) != nil || !validConversationEntry(entry, request.Harness) || entry.ID != request.ID || entry.Title != request.Title {
			writeError(w, 502, "Invalid conversation rename response; refresh before trying again")
			return
		}
		writeJSON(w, 200, entry)
		return
	}
	var listing conversations.Listing
	if json.Unmarshal(data, &listing) != nil || listing.Harness != request.Harness || len(listing.Conversations) > conversations.Limit || listing.Cwd == "" || (listing.RenameMode != "provider" && listing.RenameMode != "relay") {
		writeError(w, 502, "Invalid saved conversation metadata")
		return
	}
	for _, entry := range listing.Conversations {
		if !validConversationEntry(entry, request.Harness) || entry.Cwd != listing.Cwd {
			writeError(w, 502, "Invalid saved conversation identity")
			return
		}
	}
	listing.ResumeSupported = false
	capCtx, capCancel := context.WithTimeout(ctx, 3*time.Second)
	defer capCancel()
	client := &http.Client{Transport: l.httpTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if health, readErr := runtimeJSON(capCtx, client, "GET", "/api/health", nil); readErr == nil {
		var value struct {
			Capabilities map[string]bool `json:"capabilities"`
		}
		if json.Unmarshal(health, &value) == nil {
			listing.ResumeSupported = value.Capabilities["conversationResumeV1"]
		}
	}
	if !listing.ResumeSupported {
		listing.Detail = "Update the runtime to resume saved conversations. Existing running terminals are preserved."
	}
	s.mu.Lock()
	current = s.links[id] == l && s.ctx.Err() == nil
	s.mu.Unlock()
	if !current {
		writeError(w, 503, "The host connection changed. Refresh conversations after reconnecting.")
		return
	}
	writeJSON(w, 200, listing)
}
func validConversationEntry(e conversations.Entry, harness string) bool {
	return conversations.ValidID(e.ID) && e.Harness == harness && e.Cwd != "" && len(e.Cwd) <= 4096 && !e.UpdatedAt.IsZero() && conversations.ValidTitle(e.Title) && (e.TitleSource == "provider" || e.TitleSource == "relay" || e.TitleSource == "fallback")
}
