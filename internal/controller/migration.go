package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

type migrationCounts struct {
	Pages          int `json:"pages"`
	Tasks          int `json:"tasks"`
	Lessons        int `json:"lessons"`
	QueuedMessages int `json:"queuedMessages"`
}
type migrationPlan struct {
	HostID         string           `json:"hostId"`
	Fingerprint    string           `json:"fingerprint"`
	SourceRevision uint64           `json:"sourceRevision"`
	Harness        string           `json:"harness"`
	AgentMode      string           `json:"agentMode"`
	Counts         migrationCounts  `json:"counts"`
	Blockers       []string         `json:"blockers"`
	Warnings       []string         `json:"warnings"`
	Host           Host             `json:"-"`
	CoordinatorID  string           `json:"-"`
	Expires        time.Time        `json:"-"`
	Source         mastermindRecord `json:"-"`
	Seed           coordinatorSeed  `json:"-"`
}
type coordinatorClientFactory func(context.Context, Host, *link, string, bool) (*http.Client, error)
type coordinatorTransport interface {
	StageCoordinatorBundle(context.Context, string, string) (string, error)
	DialCoordinatorContext(context.Context, string) (net.Conn, error)
}

func (s *Server) coordinatorClient(ctx context.Context, host Host, l *link, id string, prepare bool) (*http.Client, error) {
	if s.migration.client != nil {
		return s.migration.client(ctx, host, l, id, prepare)
	}
	if l == nil || l.conn == nil {
		return nil, errors.New("Destination SSH connection is unavailable")
	}
	connection, ok := any(l.conn).(coordinatorTransport)
	if !ok {
		return nil, errors.New("This Relay SSH transport cannot host a coordinator yet")
	}
	if prepare {
		if err := l.conn.RefreshCLI(ctx, s.config.BinaryDir, s.config.Version); err != nil {
			return nil, err
		}
		bundle, err := connection.StageCoordinatorBundle(ctx, s.config.BinaryDir, s.config.Version)
		if err != nil {
			return nil, err
		}
		out, err := l.conn.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" `+mastermindShellArguments([]string{"coordinator", "prepare", "--id", id, "--binaries", bundle}), nil)
		if err != nil {
			return nil, errors.New("Could not prepare the persistent destination controller")
		}
		var result struct {
			Protocol int    `json:"protocol"`
			ID       string `json:"id"`
			Version  string `json:"version"`
		}
		if json.Unmarshal(out, &result) != nil || result.Protocol != 1 || result.ID != id || result.Version != s.config.Version {
			return nil, errors.New("Destination controller identity could not be verified")
		}
	}
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return connection.DialCoordinatorContext(ctx, id)
	}, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func (s *Server) migrationHost(id string) (Host, *link, error) {
	host, l, err := s.restartLink(id)
	if err != nil {
		return host, l, err
	}
	if id == "local" {
		return host, l, errors.New("Choose another saved Linux machine")
	}
	return host, l, nil
}
func migrationFingerprint(source mastermindRecord, workspace workspaceRecord, control controlRecord, host Host) string {
	var session *RestartSourceIdentity
	if source.Session != nil {
		session = &RestartSourceIdentity{ID: source.Session.ID, CreatedAt: source.Session.CreatedAt}
	}
	data, _ := json.Marshal(struct {
		Revision           uint64
		Token              string
		Session            *RestartSourceIdentity
		Workspace, Control uint64
		Host               Host
	}{source.Revision, source.Token, session, workspace.Revision, control.Revision, Host{ID: host.ID, Target: host.Target, Port: host.Port, CreatedAt: host.CreatedAt}})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type RestartSourceIdentity struct {
	ID        string
	CreatedAt time.Time
}

func (s *Server) migrationSnapshot(host Host) (migrationPlan, error) {
	s.mastermind.mu.Lock()
	defer s.mastermind.mu.Unlock()
	s.workspace.mu.Lock()
	defer s.workspace.mu.Unlock()
	s.control.mu.Lock()
	defer s.control.mu.Unlock()
	source := cloneMastermind(s.mastermind.record)
	if s.mastermind.err != "" || s.workspace.err != nil || s.control.err != nil {
		return migrationPlan{}, errors.New("Coordinator state could not be verified; preserve its files before moving")
	}
	if source.Config == nil {
		return migrationPlan{}, errors.New("Configure Mastermind before moving it")
	}
	plan := migrationPlan{HostID: host.ID, Host: host, SourceRevision: source.Revision, Harness: source.Config.Harness, AgentMode: source.Config.AgentMode, Blockers: []string{}, Warnings: []string{}, CoordinatorID: s.peerSourceID()[:32], Expires: time.Now().Add(5 * time.Minute), Source: source}
	plan.Fingerprint = migrationFingerprint(source, s.workspace.record, s.control.record, host)
	if source.Revision > maxMastermindRevision-3 || s.workspace.record.Revision > maxMastermindRevision-3 || s.control.record.Revision > maxMastermindRevision-3 {
		plan.Blockers = append(plan.Blockers, "The source revision limit cannot safely advance during handoff")
	}
	plan.Counts = migrationCounts{Pages: len(s.workspace.record.Pages), Tasks: len(s.workspace.record.Tasks), Lessons: len(s.control.record.Lessons)}
	if mastermindPendingPhase(source.Phase) || source.Status == "uncertain" {
		plan.Blockers = append(plan.Blockers, "The source coordinator launch is unconfirmed. Resolve its current session before moving.")
	}
	if s.workspace.record.FrozenBy != "" || s.control.record.FrozenBy != "" {
		plan.Blockers = append(plan.Blockers, "This source workspace already has a durable handoff fence. Inspect its migration before another move.")
	}
	seed := coordinatorSeed{SourceRevision: source.Revision, SourceID: s.peerSourceID(), Harness: plan.Harness, AgentMode: plan.AgentMode, Workspace: cloneWorkspace(s.workspace.record), Control: s.control.record, Hosts: []Host{}}
	seed.Control.Messages = append([]ControlMessage{}, s.control.record.Messages...)
	seed.Control.Lessons = append([]ControlLesson{}, s.control.record.Lessons...)
	seed.Control.RecoverySeen = nil
	for i := range seed.Control.Messages {
		message := &seed.Control.Messages[i]
		if message.Status == "processing" {
			plan.Blockers = append(plan.Blockers, "An inbox instruction is still processing. Finish or review it before moving.")
		}
		if message.Status == "queued" {
			plan.Counts.QueuedMessages++
		}
		message.Key = ""
		if message.Status == "completed" {
			message.Claim = "completed-before-handoff"
			message.Delivery = "not-transferred"
		} else {
			message.Claim = ""
		}
		message.Source = "desktop"
	}
	s.fleet.mu.Lock()
	endpoints := s.fleet.identity.Endpoints
	for _, other := range s.store.list() {
		if other.ID == host.ID {
			continue
		}
		endpoint, found := endpoints[other.ID]
		if !found || endpoint.Target == "" {
			plan.Blockers = append(plan.Blockers, "Connect "+other.Name+" first so its actual SSH endpoint can be shared")
			continue
		}
		seed.Hosts = append(seed.Hosts, Host{ID: other.ID, Name: other.Name, Target: endpoint.Target, Port: endpoint.Port, CreatedAt: other.CreatedAt})
	}
	s.fleet.mu.Unlock()
	for i := range seed.Workspace.Tasks {
		task := &seed.Workspace.Tasks[i]
		if task.HostID == host.ID {
			task.HostID = "local"
		} else if task.HostID == "local" {
			task.HostID = ""
			task.SessionID = ""
			plan.Warnings = append(plan.Warnings, "A task linked to this desktop was copied without its local terminal link.")
		}
	}
	plan.Warnings = append(plan.Warnings, "The source terminal remains as a stopped record. Provider conversation files and sign-in credentials are not copied.", "Messaging connectors must be configured on the destination; existing source connectors pause after the move.")
	plan.Seed = seed
	return plan, nil
}
func (s *Server) preflightMigration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HostID string `json:"hostId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.migration.mu.Lock()
	if s.migration.preparing {
		s.migration.mu.Unlock()
		writeError(w, 429, "Another destination check is running")
		return
	}
	s.migration.preparing = true
	s.migration.mu.Unlock()
	defer func() { s.migration.mu.Lock(); s.migration.preparing = false; s.migration.mu.Unlock() }()
	rec, problem := s.migration.snapshot()
	if problem != "" {
		writeError(w, 503, problem)
		return
	}
	if rec.Binding != nil || rec.SourceFrozen {
		writeError(w, 409, "Moving an already hosted coordinator again is not yet available from this desktop. Its workspace remains on the current hosted machine; no old local snapshot was copied.")
		return
	}
	if rec.Operation != nil && rec.Operation.Status != "failed" {
		writeError(w, 409, "Inspect the existing handoff before another move")
		return
	}
	host, l, err := s.migrationHost(req.HostID)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	plan, err := s.migrationSnapshot(host)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if plan.Source.Session != nil && plan.Source.Session.Status == "running" {
		sourceClient, _, ready, e := s.mastermindClient(plan.Source)
		if e != nil || !ready {
			plan.Blockers = append(plan.Blockers, "Connect the source coordinator machine so its exact process can be stopped")
		} else {
			data, e := runtimeJSON(ctx, sourceClient, "GET", "/api/health", nil)
			var health struct {
				Capabilities map[string]bool `json:"capabilities"`
			}
			if e != nil || json.Unmarshal(data, &health) != nil || !health.Capabilities["sessionStopV1"] {
				plan.Blockers = append(plan.Blockers, "The source runtime needs record-preserving session stop support. Finish this coordinator and update its local service before moving it.")
			}
		}
	}
	if len(plan.Blockers) == 0 {
		client, e := s.coordinatorClient(ctx, host, l, plan.CoordinatorID, true)
		if e != nil {
			plan.Blockers = append(plan.Blockers, e.Error())
		} else {
			data, e := peerRequest(ctx, client, "POST", "/coordinator/standby", coordinatorStandbyRequest{SourceID: plan.Seed.SourceID, Harness: plan.Harness, AgentMode: plan.AgentMode, Hosts: plan.Seed.Hosts}, 64<<10)
			var standby coordinatorStandby
			if e != nil || json.Unmarshal(data, &standby) != nil || standby.Protocol != 1 {
				plan.Blockers = append(plan.Blockers, "Destination standby could not be verified; no source process was changed")
			} else {
				plan.Blockers = append(plan.Blockers, standby.Blockers...)
				plan.Warnings = append(plan.Warnings, standby.Warnings...)
			}
		}
	}
	s.migration.mu.Lock()
	s.migration.plan = &plan
	s.migration.mu.Unlock()
	writeJSON(w, 200, plan)
}
func (s *Server) startMigration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		HostID      string `json:"hostId"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := decodeJSON(w, r, &req); err != nil || !validMastermindID(req.ID) {
		writeError(w, 400, "A new exact handoff identity is required")
		return
	}
	host, l, err := s.migrationHost(req.HostID)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	fresh, err := s.migrationSnapshot(host)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	store := s.migration
	store.mu.Lock()
	plan := store.plan
	if store.err != "" || store.record.SourceFrozen || plan == nil || plan.HostID != req.HostID || plan.Fingerprint != req.Fingerprint || fresh.Fingerprint != req.Fingerprint || time.Now().After(plan.Expires) || len(plan.Blockers) > 0 || len(fresh.Blockers) > 0 || (store.record.Operation != nil && (store.record.Operation.Status != "failed" || store.record.Operation.ID == req.ID)) {
		store.mu.Unlock()
		writeError(w, 409, "The reviewed destination or source state changed. Check destination again; no move was dispatched.")
		return
	}
	operation := MigrationOperation{ID: req.ID, HostID: req.HostID, Status: "preparing", Stage: "Verifying the prepared destination before pausing the source", UpdatedAt: time.Now().UTC()}
	record := migrationRecord{Format: 1, Operation: &operation, Host: host, CoordinatorID: plan.CoordinatorID, Source: plan.Source}
	if err = store.saveLocked(record); err != nil {
		store.mu.Unlock()
		writeError(w, 500, err.Error())
		return
	}
	copy := *plan
	store.mu.Unlock()
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		s.migrationStage(req.ID, "failed", "Controller stopped before migration dispatch", "")
		writeError(w, 503, "Controller is stopping; migration was not dispatched")
		return
	}
	s.connectWG.Add(1)
	s.mu.Unlock()
	go func() { defer s.connectWG.Done(); s.runMigration(record, copy, l) }()
	writeJSON(w, 202, operation)
}
func (s *Server) migrationStage(id, status, stage, problem string) error {
	store := s.migration
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.record.Operation == nil || store.record.Operation.ID != id {
		return errors.New("Migration identity changed")
	}
	next := store.record
	op := *next.Operation
	op.Status, op.Stage, op.Error = status, stage, problem
	next.Operation = &op
	return store.saveLocked(next)
}
func (s *Server) freezeMigration(record migrationRecord, plan migrationPlan) (coordinatorSeed, error) {
	store := s.migration
	store.mu.Lock()
	defer store.mu.Unlock()
	s.mastermind.mu.Lock()
	defer s.mastermind.mu.Unlock()
	s.workspace.mu.Lock()
	defer s.workspace.mu.Unlock()
	s.control.mu.Lock()
	defer s.control.mu.Unlock()
	if migrationFingerprint(s.mastermind.record, s.workspace.record, s.control.record, record.Host) != plan.Fingerprint {
		return coordinatorSeed{}, errors.New("Source state changed before the handoff fence; no destination was started")
	}
	next := store.record
	if next.Operation == nil || next.Operation.ID != record.Operation.ID {
		return coordinatorSeed{}, errors.New("Migration identity changed")
	}
	next.SourceFrozen = true
	op := *next.Operation
	op.Status = "quiescing"
	op.Stage = "Pausing source instructions and preserving its workspace"
	next.Operation = &op
	err := store.saveLocked(next)
	if err != nil {
		return coordinatorSeed{}, err
	}
	seed := plan.Seed
	seed.ID = record.Operation.ID
	seed.Workspace.FrozenBy = ""
	seed.Control.FrozenBy = ""
	workspace := cloneWorkspace(s.workspace.record)
	workspace.FrozenBy = record.Operation.ID
	workspace.Revision++
	workspace.UpdatedAt = time.Now().UTC()
	if err = s.workspace.saveLocked(workspace); err != nil {
		return seed, err
	}
	control := s.control.record
	control.FrozenBy = record.Operation.ID
	control.Revision++
	if err = saveControlFile(s.control.path, control); err != nil {
		s.control.err = errors.New("Source inbox migration fence was not confirmed")
		return seed, err
	}
	s.control.record = control
	source := cloneMastermind(s.mastermind.record)
	source.Enabled = false
	source.Revision++
	source.Status = "disabled"
	source.Stage = "Coordinator paused for a verified machine handoff"
	if err = s.mastermind.saveLocked(source); err != nil {
		return seed, err
	}
	return seed, nil
}
func (s *Server) runMigration(record migrationRecord, plan migrationPlan, l *link) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	id := record.Operation.ID
	client, err := s.coordinatorClient(ctx, record.Host, l, record.CoordinatorID, false)
	if err != nil {
		s.migrationStage(id, "failed", "Destination was not ready; source remains unchanged", err.Error())
		return
	}
	data, err := peerRequest(ctx, client, "POST", "/coordinator/standby", coordinatorStandbyRequest{SourceID: plan.Seed.SourceID, Harness: plan.Harness, AgentMode: plan.AgentMode, Hosts: plan.Seed.Hosts}, 64<<10)
	var standby coordinatorStandby
	if err != nil || json.Unmarshal(data, &standby) != nil || standby.Protocol != 1 || len(standby.Blockers) > 0 {
		s.migrationStage(id, "failed", "Destination standby changed; source remains unchanged", "Check the destination before another move")
		return
	}
	seed, err := s.freezeMigration(record, plan)
	if err != nil {
		current, _ := s.migration.snapshot()
		if current.SourceFrozen {
			s.migrationStage(id, "uncertain", "Source handoff fence needs review; no destination was activated", err.Error())
		} else {
			s.migrationStage(id, "failed", "Source changed before the handoff; check the current plan again", err.Error())
		}
		return
	}
	digest := seedDigest(seed)
	s.migration.mu.Lock()
	next := s.migration.record
	next.SeedDigest = digest
	err = s.migration.saveLocked(next)
	s.migration.mu.Unlock()
	if err != nil {
		return
	}
	data, err = peerRequest(ctx, client, "POST", "/coordinator/seed", seed, 64<<10)
	var receipt coordinatorReceipt
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.ID != id || receipt.Digest != digest || receipt.Phase != "ready" {
		s.migrationStage(id, "uncertain", "Destination seed was not confirmed; source capability stays paused", "No seed or activation request was repeated. Inspect the exact destination receipt.")
		return
	}
	if plan.Source.Session != nil && plan.Source.Session.Status == "running" {
		sourceClient, _, ready, e := s.mastermindClient(plan.Source)
		if e != nil || !ready {
			s.migrationStage(id, "uncertain", "Source process could not be verified; destination remains dormant", "Reconnect the source machine before recovery")
			return
		}
		data, e := runtimeJSON(ctx, sourceClient, "GET", "/api/sessions", nil)
		var sessions []MastermindSession
		var exact *MastermindSession
		if e == nil && json.Unmarshal(data, &sessions) == nil {
			for _, item := range sessions {
				if item.ID == plan.Source.Session.ID && mastermindMatches(plan.Source, item) {
					copy := item
					exact = &copy
				}
			}
		}
		if exact == nil {
			s.migrationStage(id, "uncertain", "Source session identity could not be proven; destination remains dormant", "")
			return
		}
		if exact.Status == "running" {
			data, e = runtimeJSON(ctx, sourceClient, "POST", "/api/sessions/"+exact.ID+"/stop", map[string]any{"createdAt": exact.CreatedAt})
			var stopped MastermindSession
			if e != nil || json.Unmarshal(data, &stopped) != nil || stopped.ID != exact.ID || !stopped.CreatedAt.Equal(exact.CreatedAt) || stopped.Status == "running" {
				s.migrationStage(id, "uncertain", "Source stop result was not confirmed; destination remains dormant", "The stop was not repeated")
				return
			}
		}
	}
	s.migration.mu.Lock()
	stopped := s.migration.record
	stopped.SourceStopped = true
	err = s.migration.saveLocked(stopped)
	s.migration.mu.Unlock()
	if err != nil {
		return
	}
	if err = s.migrationStage(id, "starting", "Source stopped; activating the destination coordinator once", ""); err != nil {
		return
	}
	_, err = peerRequest(ctx, client, "POST", "/coordinator/activate", map[string]string{"id": id, "digest": digest}, 64<<10)
	if err != nil {
		s.migrationStage(id, "uncertain", "Destination activation result is unconfirmed; checking its saved receipt", "Activation will not be repeated")
	}
	for ctx.Err() == nil {
		if s.reconcileMigration(ctx) {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	s.migrationStage(id, "uncertain", "Waiting to verify the destination coordinator", "Only read-only status checks will continue; no launch is repeated")
}
func (s *Server) reconcileMigration(ctx context.Context) bool {
	rec, problem := s.migration.snapshot()
	if problem != "" || rec.Operation == nil || rec.SeedDigest == "" || !rec.SourceFrozen || !rec.SourceStopped {
		return false
	}
	if rec.Operation.Status == "active" {
		return true
	}
	host, l, err := s.migrationHost(rec.Host.ID)
	if err != nil || host.Target != rec.Host.Target || host.Port != rec.Host.Port || !host.CreatedAt.Equal(rec.Host.CreatedAt) {
		return false
	}
	client, err := s.coordinatorClient(ctx, host, l, rec.CoordinatorID, false)
	if err != nil {
		return false
	}
	data, err := peerRequest(ctx, client, "GET", "/coordinator/status", nil, 64<<10)
	var status coordinatorStatus
	if err != nil || json.Unmarshal(data, &status) != nil || status.Error != "" || status.Receipt.ID != rec.Operation.ID || status.Receipt.Digest != rec.SeedDigest || status.Receipt.Phase != "activated" || status.Mastermind.Session == nil || status.Mastermind.Config == nil || status.Mastermind.Config.HostID != "local" || status.Mastermind.Config.Harness != rec.Source.Config.Harness || status.Mastermind.Config.AgentMode != rec.Source.Config.AgentMode {
		return false
	}
	if status.Mastermind.Status != "running" && status.Mastermind.Status != "exited" && status.Mastermind.Status != "interrupted" {
		return false
	}
	store := s.migration
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.record.Operation == nil || store.record.Operation.ID != rec.Operation.ID {
		return false
	}
	next := store.record
	hosted := cloneHosted(status.Mastermind)
	hosted.ControllerHostID = host.ID
	hosted.Config.HostID = host.ID
	if hosted.Local != nil {
		hosted.Local.HostID = host.ID
	}
	hosted.UpdatedAt = time.Now().UTC()
	next.LastHosted = &hosted
	next.Binding = &CoordinatorBinding{HostID: host.ID, CoordinatorID: rec.CoordinatorID, Target: host.Target, Port: host.Port, CreatedAt: host.CreatedAt}
	op := *next.Operation
	op.Status = "active"
	op.Stage = "Mastermind is hosted on " + host.Name + "; its workspace continues when this desktop closes"
	op.Error = ""
	next.Operation = &op
	return store.saveLocked(next) == nil
}
func (s *Server) getMigration(w http.ResponseWriter, r *http.Request) {
	rec, problem := s.migration.snapshot()
	if rec.Operation != nil && (rec.Operation.Status == "starting" || rec.Operation.Status == "uncertain") {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		s.reconcileMigration(ctx)
		cancel()
		rec, problem = s.migration.snapshot()
	}
	writeJSON(w, 200, map[string]any{"operation": rec.Operation, "hosted": rec.Binding, "detail": problem})
}
