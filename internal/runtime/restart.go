//go:build linux

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TwoD97/relay/internal/conversations"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// RuntimeIdentity names this user's managed runtime directory across process
// replacements. It is discovery metadata, never an authentication credential.
func (s *Server) initializeRuntimeIdentity() error {
	path := filepath.Join(s.stateDir, "runtime-identity.json")
	data, err := readPrivateFile(path, 1024)
	if errors.Is(err, os.ErrNotExist) {
		s.runtimeID, err = newID()
		if err != nil {
			return err
		}
		data, _ = json.Marshal(map[string]string{"id": s.runtimeID})
		if err = atomicPrivateWrite(path, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var value struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &value) != nil || !validID(value.ID) {
			return errors.New("invalid saved runtime identity; preserve runtime-identity.json for inspection")
		}
		s.runtimeID = value.ID
	}
	s.instanceID, err = newID()
	return err
}

type RestartSource struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}
type RestartEntry struct {
	Source  Session  `json:"source"`
	Mode    string   `json:"mode"`
	Status  string   `json:"status"`
	Detail  string   `json:"detail,omitempty"`
	Session *Session `json:"session,omitempty"`
}
type RestartPlan struct {
	RuntimeID   string         `json:"runtimeId"`
	InstanceID  string         `json:"instanceId"`
	Fingerprint string         `json:"fingerprint"`
	Entries     []RestartEntry `json:"entries"`
	Blockers    []string       `json:"blockers"`
}
type RestartOperation struct {
	ID           string         `json:"id"`
	RuntimeID    string         `json:"runtimeId"`
	FromInstance string         `json:"fromInstance"`
	Status       string         `json:"status"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
	Entries      []RestartEntry `json:"entries"`
}
type restorationKey struct{}
type restorationReceipt struct {
	ID              string
	SourceID        string
	SourceCreatedAt time.Time
}

type restartState struct {
	mu        sync.Mutex
	operation *RestartOperation
	err       string
	done      chan struct{}
}

func (s *Server) initializeRestart() error {
	s.restartRequested = make(chan struct{})
	s.restart = &restartState{done: make(chan struct{})}
	data, err := readPrivateFile(filepath.Join(s.stateDir, "restart.json"), 512<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.restart.err = "Could not read the saved restart operation; no sessions will be restored automatically"
		return nil
	}
	var op RestartOperation
	if json.Unmarshal(data, &op) != nil || !validID(op.ID) || op.RuntimeID != s.runtimeID || !validID(op.FromInstance) || len(op.Entries) > maxSessions || op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() {
		s.restart.err = "Invalid saved restart operation; no sessions will be restored automatically"
		return nil
	}
	seen := map[string]bool{}
	for _, entry := range op.Entries {
		if !validRestartEntry(op.ID, entry) || seen[entry.Source.ID] {
			s.restart.err = "Invalid saved restart session identity"
			return nil
		}
		seen[entry.Source.ID] = true
	}
	switch op.Status {
	case "waiting-restart", "restoring", "completed", "attention", "reviewed":
	default:
		s.restart.err = "Invalid saved restart state"
		return nil
	}
	s.restart.operation = &op
	s.recoveryQuiescing = op.Status == "waiting-restart" || op.Status == "restoring"
	return nil
}
func (s *Server) saveRestartLocked(op *RestartOperation) error {
	op.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	if len(data) > 512<<10 {
		return errors.New("restart record exceeds its bounded size")
	}
	err = atomicPrivateWrite(filepath.Join(s.stateDir, "restart.json"), data)
	if err != nil {
		s.restart.err = "Restart persistence was not confirmed. Preserve restart.json and reopen the runtime to inspect the durable intent before another operation."
	}
	return err
}
func (s *Server) RestartRequested() <-chan struct{} { return s.restartRequested }
func (s *Server) restartPlanLocked() RestartPlan {
	plan := RestartPlan{RuntimeID: s.runtimeID, InstanceID: s.instanceID, Entries: []RestartEntry{}, Blockers: []string{}}
	metas := []Session{}
	for _, p := range s.sessions {
		meta := p.snapshot()
		if meta.Status == "running" {
			metas = append(metas, meta)
		}
	}
	sortSessions(metas)
	for _, meta := range metas {
		entry := RestartEntry{Source: meta, Status: "pending", Mode: "resume"}
		switch {
		case meta.Purpose != "":
			entry.Detail = "Finish sign-in or maintenance before restarting this runtime"
		case meta.MastermindID != "":
			entry.Detail = "Stop this Mastermind session before restarting its runtime"
		case meta.Harness == "shell":
			entry.Mode = "reopen"
			entry.Detail = "Reopen the project folder in a new shell; running commands and shell state cannot be restored"
		case meta.ConversationID == "":
			entry.Detail = "No proven provider conversation ID is available; finish this session before restarting"
		}
		if meta.Purpose != "" || meta.MastermindID != "" || (meta.Harness != "shell" && meta.ConversationID == "") {
			entry.Status = "blocked"
			plan.Blockers = append(plan.Blockers, meta.Title+": "+entry.Detail)
		}
		plan.Entries = append(plan.Entries, entry)
	}
	if len(s.sessions)+len(plan.Entries) > maxSessions {
		plan.Blockers = append(plan.Blockers, "Restored sessions need separate records so old logs remain available. Remove finished session records until enough of the 32 slots are free.")
	}
	data, _ := json.Marshal(struct {
		RuntimeID, InstanceID string
		Entries               []RestartEntry
		Count                 int
	}{s.runtimeID, s.instanceID, plan.Entries, len(s.sessions)})
	digest := sha256.Sum256(data)
	plan.Fingerprint = hex.EncodeToString(digest[:])
	return plan
}
func (s *Server) restartPreflight(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	plan := s.restartPlanLocked()
	s.mu.Unlock()
	jsonResponse(w, 200, plan)
}
func (s *Server) restartStatus(w http.ResponseWriter, r *http.Request) {
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	if s.restart.err != "" {
		fail(w, 503, errors.New(s.restart.err))
		return
	}
	jsonResponse(w, 200, map[string]any{"operation": s.restart.operation})
}
func (s *Server) restartCommit(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := decode(w, r, &request); err != nil || !validID(request.ID) {
		fail(w, 400, errors.New("restart needs a new operation ID and exact preflight fingerprint"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	if s.closed || s.recoveryQuiescing || s.restart.err != "" {
		fail(w, 409, errors.New("runtime is already shutting down, restoring, or has an unresolved recovery error"))
		return
	}
	if old := s.restart.operation; old != nil && (old.ID == request.ID || (old.Status != "completed" && old.Status != "reviewed")) {
		fail(w, 409, errors.New("Review the previous restart operation before starting another"))
		return
	}
	plan := s.restartPlanLocked()
	if plan.Fingerprint != request.Fingerprint {
		fail(w, 409, errors.New("Sessions changed after preflight. Review the current plan before restarting"))
		return
	}
	if len(plan.Blockers) > 0 {
		fail(w, 409, errors.New(plan.Blockers[0]))
		return
	}
	op := &RestartOperation{ID: request.ID, RuntimeID: s.runtimeID, FromInstance: s.instanceID, Status: "waiting-restart", CreatedAt: time.Now().UTC(), Entries: plan.Entries}
	if err := s.saveRestartLocked(op); err != nil {
		fail(w, 500, err)
		return
	}
	s.restart.operation = op
	s.recoveryQuiescing = true
	jsonResponse(w, 202, op)
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
	close(s.restartRequested)
}
func (s *Server) restartReviewed(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decode(w, r, &request); err != nil {
		fail(w, 400, err)
		return
	}
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	op := s.restart.operation
	if op == nil || op.ID != request.ID || (op.Status != "attention" && op.Status != "completed") {
		fail(w, 409, errors.New("This restart operation is not ready for review"))
		return
	}
	next := *op
	next.Status = "reviewed"
	if err := s.saveRestartLocked(&next); err != nil {
		fail(w, 500, err)
		return
	}
	s.restart.operation = &next
	jsonResponse(w, 200, next)
}

type captureResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureResponse) Header() http.Header    { return c.header }
func (c *captureResponse) WriteHeader(status int) { c.status = status }
func (c *captureResponse) Write(data []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if c.body.Len()+len(data) > 64<<10 {
		return 0, errors.New("restoration response too large")
	}
	return c.body.Write(data)
}
func (s *Server) restoreAcceptedRestart(ctx context.Context) {
	defer close(s.restart.done)
	s.restart.mu.Lock()
	op := s.restart.operation
	if op == nil || (op.Status != "waiting-restart" && op.Status != "restoring") || op.FromInstance == s.instanceID || s.restart.err != "" {
		s.restart.mu.Unlock()
		return
	}
	op.Status = "restoring"
	if err := s.saveRestartLocked(op); err != nil {
		s.restart.err = "Could not persist restart progress; restoration was not dispatched"
		s.restart.mu.Unlock()
		return
	}
	s.restart.mu.Unlock()
	for index := range op.Entries {
		if ctx.Err() != nil {
			return
		}
		token := op.ID + ":" + op.Entries[index].Source.ID
		s.mu.Lock()
		var receipt *Session
		var original *Session
		for _, p := range s.sessions {
			meta := p.snapshot()
			if meta.RestartReceipt == token && validRestoredReceipt(meta, token, op.Entries[index].Source) {
				copy := meta
				receipt = &copy
			}
			if meta.ID == op.Entries[index].Source.ID && meta.CreatedAt.Equal(op.Entries[index].Source.CreatedAt) {
				copy := meta
				original = &copy
			}
		}
		s.mu.Unlock()
		s.restart.mu.Lock()
		entry := &op.Entries[index]
		if receipt != nil {
			entry.Session = receipt
			entry.Status = "started"
			entry.Detail = "A new terminal was created for the saved project or conversation"
		} else if entry.Status == "dispatching" {
			entry.Status = "uncertain"
			entry.Detail = "The previous launch result could not be proven. It was not retried"
		} else if entry.Status == "pending" {
			if original == nil || !sameRestartSource(*original, entry.Source) || original.Status == "running" || (original.Recovery != nil && original.Recovery.Status == "unverified") {
				entry.Status = "failed"
				entry.Detail = "The old session could not be verified as stopped; no replacement was launched"
			} else {
				entry.Status = "dispatching"
				if err := s.saveRestartLocked(op); err != nil {
					s.restart.err = "Could not persist launch intent; no further restoration was dispatched"
					s.restart.mu.Unlock()
					return
				}
				source := entry.Source
				s.restart.mu.Unlock()
				req := createRequest{Title: source.Title, Workspace: source.Workspace, Cwd: source.Cwd, Harness: source.Harness, AgentMode: source.AgentMode, ResumeConversationID: source.ConversationID}
				data, _ := json.Marshal(req)
				request, _ := http.NewRequestWithContext(context.WithValue(ctx, restorationKey{}, restorationReceipt{ID: token, SourceID: source.ID, SourceCreatedAt: source.CreatedAt}), "POST", "http://runtime/api/sessions", bytes.NewReader(data))
				response := &captureResponse{header: make(http.Header)}
				s.createSession(response, request)
				s.restart.mu.Lock()
				entry = &op.Entries[index]
				if response.status == 201 {
					var created Session
					if json.Unmarshal(response.body.Bytes(), &created) == nil && validRestoredReceipt(created, token, source) {
						entry.Status = "started"
						entry.Session = &created
						entry.Detail = "A new terminal was created; verify its provider output"
					} else {
						entry.Status = "uncertain"
						entry.Detail = "The launch response could not be verified; it was not retried"
					}
				} else {
					entry.Status = "failed"
					entry.Detail = fmt.Sprintf("The saved session could not start (HTTP %d). No launch was retried; check the project and installed harness", response.status)
				}
			}
		}
		if err := s.saveRestartLocked(op); err != nil {
			s.restart.err = "Could not persist the launch result. Reload after repairing storage; no launch will be retried"
			s.restart.mu.Unlock()
			return
		}
		s.restart.mu.Unlock()
	}
	s.restart.mu.Lock()
	op.Status = "completed"
	for _, entry := range op.Entries {
		if entry.Status != "started" {
			op.Status = "attention"
		}
	}
	if err := s.saveRestartLocked(op); err != nil {
		s.restart.err = "Could not persist final restart status"
	}
	s.restart.mu.Unlock()
	s.mu.Lock()
	s.recoveryQuiescing = false
	s.mu.Unlock()
}

func validRestartReceipt(meta Session) bool {
	if meta.RestartReceipt == "" {
		return meta.RestartSource == nil
	}
	parts := strings.Split(meta.RestartReceipt, ":")
	return len(parts) == 2 && validID(parts[0]) && validID(parts[1]) && meta.RestartSource != nil && parts[1] == meta.RestartSource.ID && !meta.RestartSource.CreatedAt.IsZero() && meta.Purpose == "" && meta.MastermindID == ""
}
func sameRestartSource(current, source Session) bool {
	return current.ID == source.ID && current.CreatedAt.Equal(source.CreatedAt) && current.Harness == source.Harness && current.Cwd == source.Cwd && current.AgentMode == source.AgentMode && current.ConversationID == source.ConversationID && current.Purpose == source.Purpose && current.MastermindID == source.MastermindID
}
func validRestoredReceipt(meta Session, token string, source Session) bool {
	return validRestartReceipt(meta) && meta.RestartReceipt == token && meta.RestartSource.ID == source.ID && meta.RestartSource.CreatedAt.Equal(source.CreatedAt) && meta.ID != source.ID && !meta.CreatedAt.IsZero() && meta.Harness == source.Harness && meta.AgentMode == source.AgentMode && meta.Cwd == source.Cwd && meta.ConversationID == source.ConversationID
}
func validRestartEntry(operation string, entry RestartEntry) bool {
	source := entry.Source
	if !validID(source.ID) || source.CreatedAt.IsZero() || source.Status != "running" || validateLabel(source.Title, 120) != nil || validateLabel(source.Workspace, 80) != nil || !validHarness(source.Harness) || source.Purpose != "" || source.MastermindID != "" || !filepath.IsAbs(source.Cwd) || len(source.Cwd) > 4096 || strings.IndexFunc(source.Cwd, unicode.IsControl) >= 0 {
		return false
	}
	if source.Harness == "shell" {
		if entry.Mode != "reopen" || source.ConversationID != "" || source.AgentMode != "" {
			return false
		}
	} else if entry.Mode != "resume" || !conversations.ValidID(source.ConversationID) || (source.AgentMode != "" && source.AgentMode != "manual" && source.AgentMode != "auto") {
		return false
	}
	switch entry.Status {
	case "pending", "dispatching", "started", "failed", "uncertain":
	default:
		return false
	}
	if entry.Session != nil && !validRestoredReceipt(*entry.Session, operation+":"+source.ID, source) {
		return false
	}
	return entry.Status != "started" || entry.Session != nil
}
