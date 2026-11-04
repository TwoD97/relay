package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type runtimeRestartJob struct {
	ID            string    `json:"id"`
	HostID        string    `json:"hostId"`
	HostCreatedAt time.Time `json:"hostCreatedAt"`
	HostTarget    string    `json:"-"`
	HostPort      int       `json:"-"`
	RuntimeID     string    `json:"runtimeId"`
	FromInstance  string    `json:"fromInstance"`
	Status        string    `json:"status"`
	Stage         string    `json:"stage"`
	UpdatedAt     time.Time `json:"updatedAt"`
}
type runtimeRestartRecord struct {
	Job    runtimeRestartJob `json:"job"`
	Target string            `json:"target"`
	Port   int               `json:"port"`
}
type runtimeRestartStore struct {
	mu    sync.Mutex
	path  string
	jobs  map[string]runtimeRestartRecord
	err   string
	write func(string, []byte) error
}

func openRuntimeRestarts(dir string) *runtimeRestartStore {
	store := &runtimeRestartStore{path: filepath.Join(dir, "runtime-restarts.json"), jobs: map[string]runtimeRestartRecord{}, write: writeRestartFile}
	data, err := readWorkspaceFile(store.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return store
	}
	if err != nil || len(data) > 256<<10 || strictWorkspaceJSON(data, &store.jobs) != nil || len(store.jobs) > 64 {
		store.err = "Invalid runtime restart records; no restart will be dispatched"
		return store
	}
	for host, rec := range store.jobs {
		validPhase := rec.Job.Status == "preparing" || rec.Job.Status == "committing" || rec.Job.Status == "observing" || rec.Job.Status == "failed" || rec.Job.Status == "uncertain" || rec.Job.Status == "reviewed"
		if !validPhase || rec.Job.HostID != host || !validMaintenanceSessionID(rec.Job.ID) || !validMaintenanceSessionID(rec.Job.RuntimeID) || !validMaintenanceSessionID(rec.Job.FromInstance) {
			store.err = "Invalid runtime restart identity"
			break
		}
	}
	for host, rec := range store.jobs {
		if rec.Job.Status == "preparing" {
			rec.Job.Status = "failed"
			rec.Job.Stage = "Controller stopped during preparation; no runtime stop was dispatched"
			store.jobs[host] = rec
		}
		if rec.Job.Status == "committing" {
			rec.Job.Status = "uncertain"
			rec.Job.Stage = "Controller stopped while accepting the restart. Review runtime status; shutdown was not retried"
			store.jobs[host] = rec
		}
	}
	if store.err == "" {
		data, err := json.Marshal(store.jobs)
		if err == nil {
			err = writeRestartFile(store.path, data)
		}
		if err != nil {
			store.err = "Could not persist recovered restart status"
		}
	}
	return store
}
func (s *runtimeRestartStore) change(host string, mutate func(*runtimeRestartRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != "" {
		return errors.New(s.err)
	}
	before, exists := s.jobs[host]
	next := before
	mutate(&next)
	next.Job.UpdatedAt = time.Now().UTC()
	s.jobs[host] = next
	if len(s.jobs) > 64 {
		if exists {
			s.jobs[host] = before
		} else {
			delete(s.jobs, host)
		}
		return errors.New("runtime restart record limit reached")
	}
	data, err := json.Marshal(s.jobs)
	if err == nil {
		err = s.write(s.path, data)
	}
	if err != nil {
		var uncertain *restartStoreUncertainError
		if errors.As(err, &uncertain) {
			s.err = "Saving runtime restart intent was not confirmed. Preserve runtime-restarts.json and reopen Relay before another operation"
			return errors.New(s.err)
		}
		if exists {
			s.jobs[host] = before
		} else {
			delete(s.jobs, host)
		}
	}
	return err
}

type restartStoreUncertainError struct{ error }

func writeRestartFile(path string, data []byte) error {
	if len(data) > 256<<10 {
		return errors.New("runtime restart record size exceeded")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".runtime-restarts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err := syncStoreDirectory(filepath.Dir(path)); err != nil {
		return &restartStoreUncertainError{err}
	}
	return nil
}
func (s *runtimeRestartStore) get(id string) (runtimeRestartRecord, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id], s.err
}
func (s *Server) restartLink(id string) (Host, *link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	host, ok := s.store.get(id)
	if id == "local" && s.config.LocalSocket != "" {
		host, ok = Host{ID: "local", Target: s.config.LocalSocket, Status: "online"}, true
	}
	l := s.links[id]
	if !ok || host.Status != "online" || l == nil || l.httpTransport == nil {
		return Host{}, nil, errors.New("Connect to this machine before reviewing a runtime restart")
	}
	return host, l, nil
}
func (s *Server) runtimeRestartPreflight(w http.ResponseWriter, r *http.Request) {
	_, l, err := s.restartLink(r.PathValue("id"))
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, &http.Client{Transport: l.httpTransport}, "GET", "/api/recovery/preflight", nil)
	if err != nil {
		writeError(w, 502, "This runtime cannot safely prepare Restart and restore. Finish and close its active sessions, then restart its runtime with the installed Relay version.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func (s *Server) runtimeRestartStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, storeErr := s.restarts.get(id)
	if storeErr != "" {
		writeError(w, 503, storeErr)
		return
	}
	host, l, err := s.restartLink(id)
	var operation json.RawMessage
	detail := ""
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		data, readErr := runtimeJSON(ctx, &http.Client{Transport: l.httpTransport}, "GET", "/api/recovery", nil)
		cancel()
		if readErr == nil {
			var result struct {
				Operation json.RawMessage `json:"operation"`
			}
			if json.Unmarshal(data, &result) == nil {
				operation = result.Operation
			}
		} else {
			detail = "Runtime recovery status is temporarily unavailable"
		}
		if rec.Job.ID != "" && (rec.Target != host.Target || rec.Port != host.Port || !rec.Job.HostCreatedAt.Equal(host.CreatedAt)) {
			writeError(w, 409, "Saved restart belongs to a different machine identity")
			return
		}
	} else {
		detail = err.Error()
	}
	var job *runtimeRestartJob
	if rec.Job.ID != "" {
		copy := rec.Job
		job = &copy
	}
	writeJSON(w, 200, map[string]any{"job": job, "operation": operation, "detail": detail})
}
func (s *Server) startRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var request struct {
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !validMaintenanceSessionID(request.ID) || len(request.Fingerprint) != 64 {
		writeError(w, 400, "Restart requires a fresh operation ID and reviewed preflight fingerprint")
		return
	}
	host, l, err := s.restartLink(id)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	if id == "local" && s.config.EnsureLocalRuntime == nil {
		writeError(w, 409, "This local service does not support runtime restart")
		return
	}
	old, storeErr := s.restarts.get(id)
	if storeErr != "" {
		writeError(w, 503, storeErr)
		return
	}
	if old.Job.ID == request.ID || (old.Job.ID != "" && old.Job.Status != "failed" && old.Job.Status != "reviewed") {
		writeError(w, 409, "Review the previous restart operation before another restart")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	data, err := runtimeJSON(ctx, &http.Client{Transport: l.httpTransport}, "GET", "/api/recovery/preflight", nil)
	cancel()
	var plan struct {
		RuntimeID   string   `json:"runtimeId"`
		InstanceID  string   `json:"instanceId"`
		Fingerprint string   `json:"fingerprint"`
		Blockers    []string `json:"blockers"`
	}
	if err != nil || json.Unmarshal(data, &plan) != nil || !validMaintenanceSessionID(plan.RuntimeID) || !validMaintenanceSessionID(plan.InstanceID) {
		writeError(w, 409, "Runtime Restart and restore is unavailable; finish active work before its first manual upgrade")
		return
	}
	if plan.Fingerprint != request.Fingerprint || len(plan.Blockers) > 0 {
		writeError(w, 409, "The restart plan changed or has blockers. Review it again")
		return
	}
	s.mu.Lock()
	if s.links[id] != l || l.repairing || s.ctx.Err() != nil {
		s.mu.Unlock()
		writeError(w, 409, "Machine maintenance or reconnection is already running")
		return
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		writeError(w, 429, "Other machine maintenance is running")
		return
	}
	current, currentErr := s.restarts.get(id)
	if currentErr != "" || (current.Job.ID != "" && current.Job.Status != "failed" && current.Job.Status != "reviewed") {
		<-s.repairSlots
		s.mu.Unlock()
		writeError(w, 409, "Another restart operation must be reviewed first")
		return
	}
	job := runtimeRestartJob{ID: request.ID, HostID: id, HostCreatedAt: host.CreatedAt, RuntimeID: plan.RuntimeID, FromInstance: plan.InstanceID, Status: "preparing", Stage: "Preparing the verified runtime binary"}
	err = s.restarts.change(id, func(rec *runtimeRestartRecord) {
		*rec = runtimeRestartRecord{Job: job, Target: host.Target, Port: host.Port}
	})
	if err != nil {
		<-s.repairSlots
		s.mu.Unlock()
		writeError(w, 500, err.Error())
		return
	}
	l.repairing = true
	s.connectWG.Add(1)
	s.mu.Unlock()
	go s.runRuntimeRestart(id, l, job, request.Fingerprint)
	writeJSON(w, 202, job)
}
func (s *Server) restartJobStage(host, id, status, stage string) error {
	return s.restarts.change(host, func(rec *runtimeRestartRecord) {
		if rec.Job.ID == id {
			rec.Job.Status = status
			rec.Job.Stage = stage
		}
	})
}
func (s *Server) runRuntimeRestart(host string, l *link, job runtimeRestartJob, fingerprint string) {
	defer s.connectWG.Done()
	defer func() { <-s.repairSlots; s.mu.Lock(); l.repairing = false; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	fail := func(status, detail string) { _ = s.restartJobStage(host, job.ID, status, detail) }
	if host != "local" {
		if l.conn == nil {
			fail("failed", "SSH connection is unavailable; no restart was dispatched")
			return
		}
		if _, err := l.conn.Repair(ctx, s.config.BinaryDir, s.config.Version, nil); err != nil {
			fail("failed", "Runtime preparation failed; no restart was dispatched: "+err.Error())
			return
		}
	}
	s.mu.Lock()
	stillLinked := s.links[host] == l
	s.mu.Unlock()
	if ctx.Err() != nil || !stillLinked {
		fail("failed", "Preparation was interrupted; no restart was dispatched")
		return
	}
	if err := s.restartJobStage(host, job.ID, "committing", "Requesting one cooperative runtime shutdown"); err != nil {
		return
	}
	client := &http.Client{Transport: l.httpTransport}
	data, err := runtimeJSON(ctx, client, "POST", "/api/recovery/start", map[string]string{"id": job.ID, "fingerprint": fingerprint})
	var operation struct {
		ID           string `json:"id"`
		RuntimeID    string `json:"runtimeId"`
		FromInstance string `json:"fromInstance"`
	}
	if err != nil || json.Unmarshal(data, &operation) != nil || operation.ID != job.ID || operation.RuntimeID != job.RuntimeID || operation.FromInstance != job.FromInstance {
		fail("uncertain", "Restart acceptance was not confirmed. It was not retried. Reconnect and review runtime recovery status")
		return
	}
	if err = s.restartJobStage(host, job.ID, "observing", "Waiting for the accepted runtime shutdown"); err != nil {
		return
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if ctx.Err() != nil {
			fail("uncertain", "Controller stopped observing the accepted restart; reconnect to review it")
			return
		}
		probe, stop := context.WithTimeout(ctx, 2*time.Second)
		data, healthErr := runtimeJSON(probe, client, "GET", "/api/health", nil)
		stop()
		if healthErr != nil {
			break
		}
		var health struct {
			RuntimeID  string `json:"runtimeId"`
			InstanceID string `json:"instanceId"`
		}
		if json.Unmarshal(data, &health) == nil && health.InstanceID != job.FromInstance {
			break
		}
		if time.Now().After(deadline) {
			fail("uncertain", "The old runtime has not stopped. Relay did not force-kill it or repeat shutdown")
			return
		}
		select {
		case <-ctx.Done():
			continue
		case <-time.After(200 * time.Millisecond):
		}
	}
	if host == "local" {
		err = s.config.EnsureLocalRuntime(ctx)
	} else {
		_, err = l.conn.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" ensure`, nil)
	}
	if err != nil {
		fail("uncertain", "The accepted runtime restart could not be observed. Reconnect the machine; the saved restore intent will not be duplicated")
		return
	}
	fail("observing", "Runtime started. Reviewing the saved restoration receipts")
}
func (s *Server) reviewRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rec, errText := s.restarts.get(id)
	if errText != "" || rec.Job.ID != request.ID {
		writeError(w, 409, "Restart identity changed; reload its status")
		return
	}
	if rec.Job.Status == "preparing" || rec.Job.Status == "committing" {
		writeError(w, 409, "Wait until the restart request has finished before reviewing it")
		return
	}
	_, l, err := s.restartLink(id)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	s.mu.Lock()
	working := l.repairing
	s.mu.Unlock()
	if working {
		writeError(w, 409, "The controller is still observing this restart; wait for it to finish")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, &http.Client{Transport: l.httpTransport}, "GET", "/api/recovery", nil)
	if err != nil {
		writeError(w, 502, "Read the current runtime status before acknowledging this operation")
		return
	}
	var result struct {
		Operation *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"operation"`
	}
	if json.Unmarshal(data, &result) != nil {
		writeError(w, 502, "Invalid runtime recovery status")
		return
	}
	if result.Operation != nil && result.Operation.ID == request.ID {
		if result.Operation.Status != "completed" && result.Operation.Status != "attention" && result.Operation.Status != "reviewed" {
			writeError(w, 409, "Restoration is still in progress")
			return
		}
		if result.Operation.Status != "reviewed" {
			if _, err = runtimeJSON(ctx, &http.Client{Transport: l.httpTransport}, "POST", "/api/recovery/review", request); err != nil {
				writeError(w, 502, "Review acknowledgement was not confirmed; no launch was retried")
				return
			}
		}
	}
	if err = s.restartJobStage(id, request.ID, "reviewed", "Recovery reviewed; no session launch was repeated"); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"reviewed": true})
}
