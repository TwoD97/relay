package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/TwoD97/relay/internal/projectcontext"
)

// No user-controlled shell text: the selected path travels exclusively in JSON
// on stdin. The refreshed CLI can run beside a compatible older live daemon.
const prepareProjectContextScript = `exec "$HOME/.local/share/relay/bin/relay" project-context --json-stdin`

func (s *Server) prepareProjectContext(w http.ResponseWriter, r *http.Request) {
	var request projectcontext.Request
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := projectcontext.ValidatePath(request.Path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	host, exists := s.store.get(id)
	local := id == "local" && s.config.LocalSocket != ""
	if local {
		host, exists = Host{ID: id, Status: "online"}, true
	}
	l := s.links[id]
	if !exists {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "Select a connected Linux host to prepare project context")
		return
	}
	if s.ctx.Err() != nil || host.Status != "online" || l == nil || (!local && (l.conn == nil || !l.conn.Ready())) {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "Connect to this host before preparing project context")
		return
	}
	if l.repairing {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "Runtime files are being prepared. Wait for that operation to finish.")
		return
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "Other host operations are being prepared. Try again shortly.")
		return
	}
	l.repairing = true
	s.connectWG.Add(1)
	s.mu.Unlock()
	defer s.connectWG.Done()
	defer func() { <-s.repairSlots; s.mu.Lock(); l.repairing = false; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	var result projectcontext.Result
	var err error
	if local {
		ctx, localCancel := context.WithTimeout(ctx, 15*time.Second)
		defer localCancel()
		result, err = projectcontext.Prepare(ctx, request.Path)
	} else {
		err = l.conn.RefreshCLI(ctx, s.config.BinaryDir, s.config.Version)
		if err == nil {
			payload, _ := json.Marshal(request)
			var output []byte
			output, err = l.conn.Run(ctx, prepareProjectContextScript, bytes.NewReader(payload))
			if err == nil {
				result, err = parseProjectContextResult(output)
			}
		}
	}
	s.mu.Lock()
	current := s.links[id] == l
	s.mu.Unlock()
	if !current {
		writeError(w, http.StatusServiceUnavailable, "Host connection changed. Project files may already be prepared; reconnect and review before retrying.")
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeError(w, status, "Could not prepare project context: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseProjectContextResult(data []byte) (projectcontext.Result, error) {
	var result projectcontext.Result
	if len(data) > 32<<10 {
		return result, errors.New("project context response exceeds 32 KiB")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("invalid project context response: %w", err)
	}
	if err := projectcontext.ValidatePath(result.Path); err != nil || len(result.Files) < 4 || len(result.Files) > 5 || len(result.Warnings) > 8 {
		return result, errors.New("incomplete project context response")
	}
	seen := map[string]bool{}
	for _, file := range result.Files {
		if seen[file.Path] || (file.Path != "AGENTS.md" && file.Path != "AGENTS.override.md" && file.Path != "CLAUDE.md" && file.Path != "MEMORY.md" && file.Path != "HANDOFF.md") || (file.Status != "created" && file.Status != "updated" && file.Status != "preserved") {
			return result, errors.New("invalid project context file result")
		}
		seen[file.Path] = true
	}
	for _, required := range []string{"AGENTS.md", "CLAUDE.md", "MEMORY.md", "HANDOFF.md"} {
		if !seen[required] {
			return result, errors.New("incomplete project context response")
		}
	}
	return result, nil
}
