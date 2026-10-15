package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

func (s *Server) getMastermind(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.mastermind.current().MastermindState)
}

func (s *Server) configureMastermind(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Revision uint64 `json:"revision"`
		MastermindConfig
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := validMastermindConfig(request.MastermindConfig); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// The same lock order is used by forgetHost: a selected host cannot vanish
	// between identity capture and the durable configuration commit.
	s.mu.Lock()
	defer s.mu.Unlock()
	host, exists := s.store.get(request.HostID)
	if request.HostID == "local" && s.config.LocalSocket != "" {
		host, exists = Host{ID: "local", Target: s.config.LocalSocket}, true
	}
	if !exists || s.ctx.Err() != nil {
		writeError(w, 409, "Choose an existing machine; this controller has no local runtime on Windows")
		return
	}
	store := s.mastermind
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadFailed {
		writeError(w, 409, store.err)
		return
	}
	current := store.record
	if current.Revision != request.Revision || current.Revision >= maxMastermindRevision {
		writeError(w, 409, "Mastermind settings changed. Refresh before saving again.")
		return
	}
	same := current.Config != nil && *current.Config == request.MastermindConfig && current.HostTarget == host.Target && current.HostPort == host.Port && current.HostCreatedAt.Equal(host.CreatedAt)
	if same && current.Enabled {
		writeJSON(w, 200, cloneMastermind(current).MastermindState)
		return
	}
	next := cloneMastermind(current)
	next.Revision++
	next.Enabled = true
	if !same {
		cfg := request.MastermindConfig
		next = mastermindRecord{Format: 1, MastermindState: MastermindState{Revision: next.Revision, Enabled: true, Config: &cfg}, Phase: "queued", HostTarget: host.Target, HostPort: host.Port, HostCreatedAt: host.CreatedAt}
	} else if next.Phase == "rejected" {
		resetMastermindAttempt(&next)
	}
	next.Status, next.Stage, next.Error = "waiting", "Waiting for the Mastermind machine and harness", ""
	next.Blocker = ""
	if err := store.saveLocked(next); err != nil {
		writeError(w, 500, "Could not save Mastermind settings; no new start request was dispatched")
		return
	}
	writeJSON(w, 202, cloneMastermind(store.record).MastermindState)
	s.wakeMastermind()
}

func (s *Server) changeMastermind(w http.ResponseWriter, r *http.Request, disable bool) {
	var request struct {
		Revision uint64 `json:"revision"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	store := s.mastermind
	store.mu.Lock()
	defer store.mu.Unlock()
	current := store.record
	if request.Revision != current.Revision || current.Revision >= maxMastermindRevision {
		writeError(w, 409, "Mastermind settings changed. Refresh before trying again.")
		return
	}
	if current.Config == nil {
		writeError(w, 409, "Choose a Mastermind machine and harness first")
		return
	}
	if !disable && store.err != "" {
		writeError(w, 409, store.err)
		return
	}
	if !disable && current.Enabled && mastermindPendingPhase(current.Phase) {
		writeError(w, 409, "The previous Mastermind start is unconfirmed. Inspect this machine's sessions; the request will not be repeated.")
		return
	}
	if (!disable && current.Enabled && (current.Phase == "queued" || (current.Phase == "attached" && current.Status != "exited" && current.Status != "interrupted" && current.Status != "missing"))) || (disable && !current.Enabled) {
		if !disable && current.Phase == "queued" {
			// A failed compatibility probe must not upload the release every poll.
			// This explicit setup check retries preparation, not an unknown launch.
			store.record.Status, store.record.Stage, store.record.Error, store.record.Blocker = "waiting", "Checking Mastermind setup", "", ""
			store.record.UpdatedAt = time.Now().UTC()
			s.wakeMastermind()
		}
		writeJSON(w, 200, cloneMastermind(store.record).MastermindState)
		return
	}
	next := cloneMastermind(current)
	next.Revision++
	next.Enabled = !disable
	next.Blocker = ""
	if disable {
		next.Status, next.Stage, next.Error = "disabled", "Mastermind is disabled; its session is preserved", ""
	} else {
		// Re-enabling a running or unverified session observes it first. A new
		// agent is only allowed after an explicit start of a confirmed end.
		if current.Phase == "rejected" || (current.Phase == "attached" && (current.Status == "exited" || current.Status == "interrupted" || current.Status == "missing")) {
			resetMastermindAttempt(&next)
		}
		next.Status, next.Stage, next.Error = "waiting", "Waiting for the Mastermind machine and harness", ""
	}
	if err := store.saveLocked(next); err != nil {
		writeError(w, 500, "Could not save Mastermind settings; no start request was dispatched")
		return
	}
	writeJSON(w, 202, cloneMastermind(store.record).MastermindState)
	s.wakeMastermind()
}

func resetMastermindAttempt(rec *mastermindRecord) {
	rec.Phase, rec.Token, rec.CanonicalCwd, rec.Session = "queued", "", "", nil
	rec.Execution, rec.WorkerPath, rec.WorkerStateDir, rec.Blocker = "", "", "", ""
}

func (s *Server) startMastermind(w http.ResponseWriter, r *http.Request) {
	s.changeMastermind(w, r, false)
}
func (s *Server) disableMastermind(w http.ResponseWriter, r *http.Request) {
	s.changeMastermind(w, r, true)
}

func (s *Server) wakeMastermind() {
	select {
	case s.mastermindWake <- struct{}{}:
	default:
	}
}

func (s *Server) monitorMastermind() {
	defer s.mastermindWG.Done()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		record := s.mastermind.current()
		if record.Enabled && record.Config != nil && s.ctx.Err() == nil {
			s.reconcileMastermind(record)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.mastermindWake:
		case <-tick.C:
		}
	}
}

func (s *Server) mastermindClient(rec mastermindRecord) (*http.Client, *transport.Connection, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, nil, false, s.ctx.Err()
	}
	if rec.Config.HostID == "local" {
		if s.config.LocalSocket == "" || rec.HostTarget != s.config.LocalSocket {
			return nil, nil, false, errors.New("The local runtime identity changed. Choose the Mastermind machine again; the previous session is preserved.")
		}
	} else {
		host, ok := s.store.get(rec.Config.HostID)
		if !ok || host.Target != rec.HostTarget || host.Port != rec.HostPort || !host.CreatedAt.Equal(rec.HostCreatedAt) {
			return nil, nil, false, errors.New("The saved Mastermind machine identity changed. Choose a machine again; no session was touched.")
		}
		if host.Status != "online" {
			return nil, nil, false, nil
		}
	}
	l := s.links[rec.Config.HostID]
	if l == nil || l.httpTransport == nil || l.repairing {
		return nil, nil, false, nil
	}
	return &http.Client{Transport: l.httpTransport}, l.conn, true, nil
}

func mastermindMatches(rec mastermindRecord, session MastermindSession) bool {
	if rec.Config == nil {
		return false
	}
	providerMatches := session.MastermindID == rec.Token && session.Harness == rec.Config.Harness && session.AgentMode == rec.Config.AgentMode
	if rec.Execution == "worker" {
		providerMatches = session.Harness == "shell" && session.MastermindID == "" && session.AgentMode == ""
	}
	return validMastermindID(session.ID) && !session.CreatedAt.IsZero() && providerMatches &&
		session.Cwd == rec.CanonicalCwd && session.Purpose == "" &&
		(session.Status == "running" || session.Status == "exited" || session.Status == "interrupted") &&
		(rec.Session == nil || (rec.Session.ID == session.ID && rec.Session.CreatedAt.Equal(session.CreatedAt)))
}

func (s *Server) reconcileMastermind(rec mastermindRecord) {
	if rec.Phase == "rejected" || (rec.Phase == "queued" && rec.Status == "blocked" && rec.Blocker == "worker") {
		return
	}
	client, connection, ready, err := s.mastermindClient(rec)
	if err != nil {
		s.mastermind.notice(rec.Revision, "blocked", "Mastermind machine needs attention", err.Error(), "host")
		return
	}
	if !ready {
		s.mastermind.notice(rec.Revision, "waiting", "Waiting for the Mastermind machine to reconnect", "")
		return
	}
	if rec.Phase == "queued" {
		s.createMastermind(rec, client, connection)
		return
	}
	if rec.Execution == "worker" && rec.Session == nil {
		s.mastermind.notice(rec.Revision, "uncertain", "The Mastermind setup session could not be confirmed", "No create or input request will be repeated. Inspect this machine's sessions before changing configuration.", "identity")
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, client, "GET", "/api/sessions", nil)
	if err != nil {
		s.mastermind.notice(rec.Revision, "waiting", "Waiting to verify the existing Mastermind session", "No start request will be repeated.")
		return
	}
	var sessions []MastermindSession
	if json.Unmarshal(data, &sessions) != nil {
		s.mastermind.notice(rec.Revision, "uncertain", "Mastermind session state is invalid", "No start request will be repeated.")
		return
	}
	var found *MastermindSession
	for i := range sessions {
		item := &sessions[i]
		if (rec.Execution == "worker" || item.MastermindID != rec.Token) && (rec.Session == nil || item.ID != rec.Session.ID) {
			continue
		}
		if found != nil || !mastermindMatches(rec, *item) {
			s.mastermind.notice(rec.Revision, "uncertain", "Mastermind session identity does not match", "The existing sessions were preserved. No start request will be repeated.", "identity")
			return
		}
		found = item
	}
	if found == nil {
		if mastermindPendingPhase(rec.Phase) {
			s.mastermind.notice(rec.Revision, "uncertain", "The previous Mastermind start could not be confirmed", "No request was repeated. Inspect this machine's sessions before changing the configuration.")
		} else {
			s.mastermind.notice(rec.Revision, "missing", "The saved Mastermind session is no longer available", "Start a new Mastermind when ready. Nothing was replayed.")
		}
		return
	}
	if rec.Execution == "worker" {
		data, err := readMastermindWorkerStatus(ctx, rec, connection)
		if err != nil {
			s.mastermind.notice(rec.Revision, "uncertain", "Waiting to verify the Mastermind worker", "The worker receipt could not be read. Nothing will be replayed.", "identity")
			return
		}
		if err := verifyMastermindWorkerReceipt(rec, data); err != nil {
			s.mastermind.notice(rec.Revision, "uncertain", "Mastermind worker launch needs verification", err.Error(), "identity")
			return
		}
	}
	s.attachMastermind(rec, *found)
}

func (s *Server) attachMastermind(rec mastermindRecord, session MastermindSession) {
	stage := map[string]string{"running": "Mastermind session is connected", "exited": "Mastermind has exited; saved output is available", "interrupted": "Mastermind was interrupted; no work was replayed"}[session.Status]
	if rec.Phase == "attached" && rec.Session != nil && sameMastermindSession(*rec.Session, session) {
		s.mastermind.notice(rec.Revision, session.Status, stage, "")
		return
	}
	s.mastermind.transition(rec.Revision, func(next *mastermindRecord) {
		next.Phase, next.Session = "attached", &session
		next.Status, next.Stage, next.Error = session.Status, stage, ""
		next.Blocker = ""
	})
}

func sameMastermindSession(a, b MastermindSession) bool {
	if (a.ExitCode == nil) != (b.ExitCode == nil) || (a.ExitCode != nil && *a.ExitCode != *b.ExitCode) {
		return false
	}
	a.ExitCode, b.ExitCode = nil, nil
	return a == b
}

func (s *Server) createMastermind(rec mastermindRecord, client *http.Client, connection *transport.Connection) {
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	blocked := func(reason, message string) {
		s.mastermind.notice(rec.Revision, "blocked", "Mastermind setup needs attention", message, reason)
	}
	data, err := runtimeJSON(ctx, client, "GET", "/api/health", nil)
	if err != nil {
		s.mastermind.notice(rec.Revision, "waiting", "Waiting for the runtime before starting Mastermind", "")
		return
	}
	var health struct {
		Home         string `json:"home"`
		Capabilities struct {
			MastermindV1 bool `json:"mastermindV1"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(data, &health) != nil {
		blocked("runtime", "Could not read the runtime capabilities. Check the connection; no session was started.")
		return
	}
	data, err = runtimeJSON(ctx, client, "GET", "/api/harnesses", nil)
	if err != nil {
		blocked("harness", "Could not verify the selected harness. Open the machine to check its installation and sign-in.")
		return
	}
	var harnesses []struct {
		ID            string `json:"id"`
		Installed     bool   `json:"installed"`
		Authenticated *bool  `json:"authenticated"`
	}
	if json.Unmarshal(data, &harnesses) != nil {
		blocked("harness", "The runtime returned invalid harness status; no agent was started.")
		return
	}
	ready := false
	for _, harness := range harnesses {
		if harness.ID == rec.Config.Harness && harness.Installed && harness.Authenticated != nil && *harness.Authenticated {
			ready = true
		}
	}
	if !ready {
		blocked("harness", "Install and sign in to the selected harness on this machine. Mastermind will start after setup is ready.")
		return
	}
	query := transport.DirectoryQuery{Path: rec.Config.Cwd}
	if (query.Path == "~" || strings.HasPrefix(query.Path, "~/")) && (!strings.HasPrefix(health.Home, "/") || (transport.DirectoryQuery{Path: health.Home}).Validate() != nil) {
		blocked("folder", "The runtime did not report a valid home folder. Choose an absolute working folder.")
		return
	}
	if query.Path == "~" {
		query.Path = health.Home
	} else if strings.HasPrefix(query.Path, "~/") {
		query.Path = path.Join(health.Home, query.Path[2:])
	}
	var directory transport.DirectoryListing
	if rec.Config.HostID == "local" {
		directory, err = localDirectories(ctx, query)
	} else if connection != nil {
		directory, err = connection.ListDirectories(ctx, query)
	} else {
		err = errors.New("SSH connection is unavailable")
	}
	if err != nil || !strings.HasPrefix(directory.Path, "/") || (transport.DirectoryQuery{Path: directory.Path}).Validate() != nil {
		blocked("folder", "Could not resolve the working folder. Open Mastermind settings and choose an existing folder.")
		return
	}
	execution, workerPath, workerStateDir := "native", "", ""
	if !health.Capabilities.MastermindV1 {
		execution = "worker"
		workerPath, workerStateDir, err = s.prepareMastermindWorker(ctx, rec, connection)
		if err != nil {
			blocked("worker", err.Error())
			return
		}
	}
	// Random ownership metadata is persisted before the non-idempotent request.
	// It is never a provider token or a claim based on a session title.
	token := randomID()[:32]
	if ctx.Err() != nil || !s.mastermind.transition(rec.Revision, func(next *mastermindRecord) {
		next.Phase, next.Token, next.CanonicalCwd = "creating", token, directory.Path
		next.Status, next.Stage, next.Error = "starting", "Starting Mastermind once", ""
		next.Execution, next.WorkerPath, next.WorkerStateDir, next.Blocker = execution, workerPath, workerStateDir, ""
	}) {
		return
	}
	rec = s.mastermind.current()
	if rec.Token != token || !rec.Enabled {
		return
	}
	body := map[string]any{
		"title": "Mastermind", "workspace": "Mastermind", "cwd": directory.Path,
		"harness": rec.Config.Harness,
	}
	if execution == "worker" {
		body["harness"] = "shell"
	} else {
		body["agentMode"], body["mastermindId"] = rec.Config.AgentMode, token
	}
	data, err = runtimeJSON(ctx, client, "POST", "/api/sessions", body)
	if err != nil {
		var rejection *runtimeHTTPError
		if errors.As(err, &rejection) && (rejection.Status == 400 || rejection.Status == 422) {
			s.mastermind.transition(rec.Revision, func(next *mastermindRecord) {
				next.Phase, next.Status, next.Stage, next.Error = "rejected", "blocked", "Mastermind startup was rejected", rejection.Error()
				next.Blocker = "launch"
			})
			return
		}
		// Even an error response might come from a proxy after dispatch. Verify
		// the durable token by GET; never retry POST based on a transport guess.
		s.mastermind.notice(rec.Revision, "uncertain", "Checking whether Mastermind started", "The start was not repeated. "+err.Error())
		return
	}
	var session MastermindSession
	if json.Unmarshal(data, &session) != nil || !mastermindMatches(rec, session) {
		s.mastermind.notice(rec.Revision, "uncertain", "Mastermind startup returned an invalid identity", "No request was repeated. Inspect the machine's sessions.")
		return
	}
	if execution == "worker" {
		s.dispatchMastermindWorker(ctx, rec, session, client)
		return
	}
	s.attachMastermind(rec, session)
}

func (s *Server) dispatchMastermindWorker(ctx context.Context, rec mastermindRecord, session MastermindSession, client *http.Client) {
	// A returned shell identity must be durable before any input can execute.
	if !s.mastermind.transition(rec.Revision, func(next *mastermindRecord) {
		next.Phase, next.Session = "worker-prepared", &session
		next.Stage = "Preparing Mastermind in its persistent terminal"
	}) {
		return
	}
	if ctx.Err() != nil || !s.mastermind.transition(rec.Revision, func(next *mastermindRecord) {
		next.Phase, next.Stage = "worker-dispatched", "Starting the compatible Mastermind worker once"
	}) {
		return
	}
	dispatched := s.mastermind.current()
	if !dispatched.Enabled || dispatched.Revision != rec.Revision || dispatched.Token != rec.Token || dispatched.Session == nil || dispatched.Session.ID != session.ID || !dispatched.Session.CreatedAt.Equal(session.CreatedAt) {
		return
	}
	command := "exec " + mastermindShellArguments(append([]string{dispatched.WorkerPath}, mastermindWorkerArgs(dispatched, "run")...)) + "\r"
	err := startMaintenanceInput(ctx, client, session.ID, command)
	message := "Checking the durable worker receipt before connecting. No command will be replayed."
	if err != nil {
		message = "The worker input outcome is unconfirmed. Its durable receipt will be checked; no command will be replayed."
	}
	s.mastermind.notice(rec.Revision, "uncertain", "Verifying the Mastermind worker launch", message, "identity")
	s.wakeMastermind()
}
