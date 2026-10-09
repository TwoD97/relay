package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Accepted work is owned by the controller, not the requesting browser. No
// runtime mutation happens before its durable job intent exists.
func (s *Server) maintainHostHarness(w http.ResponseWriter, r *http.Request) {
	id, harness, action := r.PathValue("id"), r.PathValue("harness"), r.PathValue("action")
	if (harness != "claude" && harness != "codex") || (action != "update" && action != "repair") {
		writeError(w, 404, "Unknown harness maintenance action")
		return
	}
	s.mu.Lock()
	host, exists := s.store.get(id)
	local := id == "local" && s.config.LocalSocket != ""
	if local {
		host, exists = Host{ID: "local", Target: s.config.LocalSocket, Status: "online"}, true
	}
	l := s.links[id]
	if !exists {
		s.mu.Unlock()
		writeError(w, 404, "Host not found")
		return
	}
	if s.ctx.Err() != nil || host.Status != "online" || l == nil || l.httpTransport == nil || (!local && (l.conn == nil || !l.conn.Ready())) {
		s.mu.Unlock()
		writeError(w, 503, "Connect to this host before maintaining its harnesses")
		return
	}
	if l.repairing {
		s.mu.Unlock()
		writeError(w, 409, "Runtime files are being prepared. Wait for that operation to finish.")
		return
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		writeError(w, 429, "Other maintenance jobs are being prepared. Try again shortly.")
		return
	}
	job, err := s.maintenance.add(host, harness, action)
	if err != nil {
		<-s.repairSlots
		s.mu.Unlock()
		writeError(w, 409, err.Error())
		return
	}
	l.repairing = true
	client := &http.Client{Transport: l.httpTransport}
	s.connectWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.connectWG.Done()
		defer func() { <-s.repairSlots; s.mu.Lock(); l.repairing = false; s.mu.Unlock() }()
		prepare := func(ctx context.Context) error {
			if local {
				return checkLocalMaintenance(ctx, client, harness, action)
			}
			prepareCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var persistenceFailed atomic.Bool
			result, err := l.conn.Repair(prepareCtx, s.config.BinaryDir, s.config.Version, func(stage string) {
				if persistenceFailed.Load() {
					return
				}
				if !s.maintenanceChange(job.ID, func(rec *maintenanceRecord) { rec.Job.Stage = stage }) {
					persistenceFailed.Store(true)
					cancel()
				}
			})
			if persistenceFailed.Load() {
				return errors.New("Maintenance preparation stopped because its state could not be saved. No session was started.")
			}
			if err == nil {
				s.repairStage(id, l, &RuntimeOperation{Status: "completed", Stage: "Maintenance binary ready. Existing sessions remain running.", InstalledVersion: result.InstalledVersion, RunningVersion: result.RunningVersion, RestartRequired: result.RestartRequired})
			}
			return err
		}
		s.runMaintenanceJob(job, client, local, prepare)
	}()
	writeJSON(w, http.StatusAccepted, job)
}

type runtimeHTTPError struct {
	Status  int
	Message string
}

func (e *runtimeHTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("runtime returned HTTP %d", e.Status)
}

func startMaintenanceInput(ctx context.Context, client *http.Client, id, command string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		_, err := runtimeJSON(ctx, client, "POST", "/api/sessions/"+id+"/input", map[string]string{"data": command})
		if err == nil {
			return nil
		}
		var rejection *runtimeHTTPError
		// Older daemons publish a session before its PTY is assigned. This
		// exact response is issued before any write; every other outcome,
		// including a lease conflict or I/O failure, is returned without retry.
		if !errors.As(err, &rejection) || rejection.Status != http.StatusConflict || rejection.Message != "session is not accepting input" {
			return err
		}
		data, err := runtimeJSON(ctx, client, "GET", "/api/sessions", nil)
		if err != nil {
			return err
		}
		var sessions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(data, &sessions); err != nil {
			return err
		}
		running := false
		for _, session := range sessions {
			if session.ID == id && session.Status == "running" {
				running = true
				break
			}
		}
		if !running {
			return errors.New("setup session exited before accepting input")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func runtimeJSON(ctx context.Context, client *http.Client, method, path string, body any) ([]byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay-runtime"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Never replay a mutation after a reused bridge fails or redirects. Its
	// outcome can be uncertain even when no successful response was received.
	req.GetBody = nil
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	requestClient.Jar = nil
	response, err := requestClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	limit := int64(64 << 10)
	// Session cwd fields can make a valid 32-session list larger than 64 KiB.
	// Mutations retain the smaller response cap.
	if method == http.MethodGet && path == "/api/sessions" {
		limit = 512 << 10
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("runtime response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			return nil, &runtimeHTTPError{Status: response.StatusCode, Message: failure.Error}
		}
		return nil, &runtimeHTTPError{Status: response.StatusCode}
	}
	return data, nil
}
