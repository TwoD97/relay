package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type maintenanceSession struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Status    string    `json:"status"`
	ExitCode  *int      `json:"exitCode"`
}

func (s *Server) maintenanceChange(id string, change func(*maintenanceRecord)) bool {
	if err := s.maintenance.update(id, change); err != nil {
		// No further mutation may follow an uncommitted transition. Keep a visible
		// in-memory warning even when the durable store itself cannot be written.
		s.maintenance.mu.Lock()
		defer s.maintenance.mu.Unlock()
		for i := range s.maintenance.records {
			rec := &s.maintenance.records[i]
			if rec.Job.ID == id {
				rec.Phase, rec.Job.Status, rec.Job.CleanupStatus = "done", "uncertain", "retained"
				rec.Job.Stage = "Maintenance state could not be saved"
				rec.Job.Error = "No further work was dispatched. Inspect retained output before retrying."
			}
		}
		return false
	}
	return true
}
func (s *Server) maintenanceFinish(id, status, stage, message, cleanup string) {
	s.maintenanceChange(id, func(rec *maintenanceRecord) {
		rec.Phase = "done"
		rec.Job.Status, rec.Job.Stage, rec.Job.Error, rec.Job.CleanupStatus = status, stage, message, cleanup
	})
}

func checkLocalMaintenance(ctx context.Context, client *http.Client, harness, action string) error {
	data, err := runtimeJSON(ctx, client, "GET", "/api/harnesses", nil)
	if err != nil {
		return err
	}
	var tools []struct {
		ID         string `json:"id"`
		Management *struct {
			Actions []string `json:"supportedActions"`
		} `json:"management"`
	}
	if err := json.Unmarshal(data, &tools); err != nil {
		return err
	}
	for _, tool := range tools {
		if tool.ID != harness || tool.Management == nil {
			continue
		}
		for _, supported := range tool.Management.Actions {
			if supported == action {
				return nil
			}
		}
	}
	return errors.New("This local runtime does not support background harness maintenance. Finish its active sessions, then restart the local runtime with the installed Relay version.")
}

func (s *Server) runMaintenanceJob(job MaintenanceJob, client *http.Client, local bool, prepare func(context.Context) error) {
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	if err := prepare(ctx); err != nil {
		s.maintenanceFinish(job.ID, "failed", "Could not prepare maintenance", err.Error(), "retained")
		return
	}
	// A failed progress write may have frozen this job while preparation was
	// unwinding. Never revive that visible terminal failure, even if a helper
	// reports success after cancellation.
	current, ok := s.maintenance.get(job.ID)
	if !ok || current.Phase != "accepted" || current.Job.Status != "preparing" {
		return
	}
	if err := ctx.Err(); err != nil {
		s.maintenanceFinish(job.ID, "failed", "Preparation was interrupted", "No maintenance session was started.", "retained")
		return
	}
	if !s.maintenanceChange(job.ID, func(rec *maintenanceRecord) {
		rec.Phase, rec.Job.Status, rec.Job.Stage = "creating", "starting", "Starting background maintenance"
	}) {
		return
	}
	route := "/api/sessions"
	var body any = map[string]string{"title": map[string]string{"update": "Update", "repair": "Repair"}[job.Action] + " " + map[string]string{"claude": "Claude Code", "codex": "Codex"}[job.Harness], "workspace": "Setup", "cwd": "~", "harness": "shell"}
	if local {
		route, body = "/api/harnesses/"+job.Harness+"/"+job.Action, nil
	}
	data, err := runtimeJSON(ctx, client, "POST", route, body)
	if err != nil {
		s.maintenanceFinish(job.ID, "uncertain", "Could not confirm maintenance session creation", "No request was repeated. Inspect this host's sessions before trying again. "+err.Error(), "retained")
		return
	}
	var session maintenanceSession
	if json.Unmarshal(data, &session) != nil || !validMaintenanceSessionID(session.ID) || session.CreatedAt.IsZero() {
		s.maintenanceFinish(job.ID, "uncertain", "Maintenance returned an invalid session identity", "The session could not be safely identified. Nothing was replayed or removed.", "retained")
		return
	}
	if !s.maintenanceChange(job.ID, func(rec *maintenanceRecord) {
		rec.Job.SessionID, rec.Job.SessionCreatedAt = session.ID, &session.CreatedAt
		if local {
			rec.Phase, rec.Job.Status, rec.Job.Stage = "observing", "running", "Maintenance is running in the background"
		} else {
			rec.Phase, rec.Job.Stage = "input", "Starting the maintenance worker"
		}
	}) {
		return
	}
	if local {
		return
	}
	// Closed enums only, no request paths or user-authored command strings.
	command := `exec "$HOME/.local/share/relay/bin/relay" harness-maintain --id ` + job.Harness + ` --action ` + job.Action + "\r"
	if err := startMaintenanceInput(ctx, client, session.ID, command); err != nil {
		s.maintenanceFinish(job.ID, "uncertain", "Could not confirm maintenance startup", "Input was not repeated. Open retained output before trying again. "+err.Error(), "retained")
		return
	}
	s.maintenanceChange(job.ID, func(rec *maintenanceRecord) {
		rec.Phase, rec.Job.Status, rec.Job.Stage = "observing", "running", "Maintenance is running in the background"
	})
}

func (s *Server) monitorMaintenance() {
	defer s.maintenanceWG.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// Four bounded reads at a time, one pass at a time. A job cannot race its own
		// cleanup, and controller shutdown waits for these cancellable requests.
		var wg sync.WaitGroup
		slots := make(chan struct{}, 4)
		for _, rec := range s.maintenance.pending() {
			if s.ctx.Err() != nil {
				break
			}
			select {
			case slots <- struct{}{}:
			case <-s.ctx.Done():
				break
			}
			if s.ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(rec maintenanceRecord) { defer wg.Done(); defer func() { <-slots }(); s.reconcileMaintenance(rec) }(rec)
		}
		wg.Wait()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) maintenanceClient(rec maintenanceRecord) (*http.Client, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, false, s.ctx.Err()
	}
	if rec.Job.HostID == "local" {
		if rec.HostTarget != s.config.LocalSocket || s.config.LocalSocket == "" {
			return nil, false, errors.New("The local runtime configuration no longer matches this job.")
		}
	} else {
		h, ok := s.store.get(rec.Job.HostID)
		if !ok || h.Target != rec.HostTarget || h.Port != rec.HostPort || !h.CreatedAt.Equal(rec.HostCreatedAt) {
			return nil, false, errors.New("The saved host identity no longer matches this job. Retained output was not touched.")
		}
		if h.Status != "online" {
			return nil, false, nil
		}
	}
	l := s.links[rec.Job.HostID]
	if l == nil || l.httpTransport == nil {
		return nil, false, nil
	}
	return &http.Client{Transport: l.httpTransport}, true, nil
}

func (s *Server) reconcileMaintenance(rec maintenanceRecord) {
	client, ready, err := s.maintenanceClient(rec)
	if err != nil {
		if s.ctx.Err() == nil {
			s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance host identity changed", err.Error(), "retained")
		}
		return
	}
	if !ready {
		s.maintenanceWait(rec, "Waiting for the host to reconnect")
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, client, "GET", "/api/sessions", nil)
	if err != nil {
		if s.ctx.Err() == nil {
			s.maintenanceWait(rec, "Waiting to read maintenance status")
		}
		return
	}
	var sessions []maintenanceSession
	if json.Unmarshal(data, &sessions) != nil {
		s.maintenanceWait(rec, "Waiting for valid maintenance status")
		return
	}
	var found *maintenanceSession
	for i := range sessions {
		if sessions[i].ID == rec.Job.SessionID {
			if found != nil {
				s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance session identity is ambiguous", "Duplicate session IDs were returned; no cleanup was attempted.", "retained")
				return
			}
			found = &sessions[i]
		}
	}
	if found == nil {
		if rec.Phase == "deleting" {
			s.maintenanceFinish(rec.Job.ID, "succeeded", "Maintenance completed", "", "removed")
		} else {
			s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance output is no longer available", "The owned session disappeared before its result could be verified.", "retained")
		}
		return
	}
	if rec.Job.SessionCreatedAt == nil || !found.CreatedAt.Equal(*rec.Job.SessionCreatedAt) {
		s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance session identity changed", "The session creation time no longer matches. No cleanup was attempted.", "retained")
		return
	}
	if rec.Phase == "deleting" {
		s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance completed; output cleanup needs inspection", "A previous removal was not confirmed. The request was not repeated.", "uncertain")
		return
	}
	switch found.Status {
	case "running":
		s.maintenanceWait(rec, "Maintenance is running in the background")
		return
	case "exited":
		if found.ExitCode == nil {
			s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance exited without a confirmed result", "No exit code was available; output was retained.", "retained")
			return
		}
		if *found.ExitCode != 0 {
			s.maintenanceFinish(rec.Job.ID, "failed", "Maintenance failed", fmt.Sprintf("The worker exited with code %d. Open its retained output for details.", *found.ExitCode), "retained")
			return
		}
	case "interrupted":
		s.maintenanceFinish(rec.Job.ID, "failed", "Maintenance was interrupted", "The runtime interrupted this worker. Its output was retained and no work was replayed.", "retained")
		return
	default:
		s.maintenanceFinish(rec.Job.ID, "uncertain", "Maintenance returned an unknown status", "Output was retained; no cleanup was attempted.", "retained")
		return
	}
	if !s.maintenanceChange(rec.Job.ID, func(current *maintenanceRecord) {
		current.Phase, current.Job.Stage, current.Job.CleanupStatus = "deleting", "Removing successful maintenance output", "pending"
	}) {
		return
	}
	// Only a positively owned, completed, exit-zero session reaches DELETE. The
	// legacy API has no conditional delete; session IDs are runtime-generated and
	// cannot be recreated by a client. A lost response is reconciled by GET only.
	deleteCtx, deleteCancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer deleteCancel()
	_, err = runtimeJSON(deleteCtx, client, "DELETE", "/api/sessions/"+rec.Job.SessionID, nil)
	if err != nil {
		return
	}
	s.maintenanceFinish(rec.Job.ID, "succeeded", "Maintenance completed", "", "removed")
}

func (s *Server) maintenanceWait(rec maintenanceRecord, stage string) {
	if rec.Job.Stage == stage {
		return
	}
	s.maintenanceChange(rec.Job.ID, func(current *maintenanceRecord) { current.Job.Stage = stage })
}
