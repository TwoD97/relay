//go:build linux

package runtime

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxApprovalInput = 64 << 10
const approvalLifetime = 10 * time.Minute

// Permissions describes observed integration capability, not an inferred policy.
// Replace this value on updates: session snapshots may be encoded concurrently.
type Permissions struct {
	Support        string     `json:"support"`
	Detail         string     `json:"detail"`
	Mode           string     `json:"mode,omitempty"`
	ModeObservedAt *time.Time `json:"modeObservedAt,omitempty"`
}

type Approval struct {
	ID               string          `json:"id"`
	SessionID        string          `json:"sessionId"`
	SessionCreatedAt time.Time       `json:"sessionCreatedAt"`
	Provider         string          `json:"provider"`
	ToolName         string          `json:"toolName"`
	Input            json.RawMessage `json:"input"`
	Cwd              string          `json:"cwd"`
	PermissionMode   string          `json:"permissionMode,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
	ExpiresAt        time.Time       `json:"expiresAt"`
	Status           string          `json:"status"`
	Decision         string          `json:"decision,omitempty"`
	Detail           string          `json:"detail,omitempty"`
}

type approvalWaiter struct {
	record       Approval
	session      *liveSession
	disconnected <-chan struct{}
	wake         chan struct{}
}

type approvalHookEvent struct {
	Provider       string          `json:"provider"`
	Event          string          `json:"event"`
	ToolName       string          `json:"toolName,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
}

func eventCapability(p *liveSession, r *http.Request) bool {
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	p.mu.Lock()
	token := p.eventToken
	p.mu.Unlock()
	return token != "" && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authorization, "Bearer ")), []byte(token)) == 1
}

func hookText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

// approvalHook is reachable only with the live session's private capability.
// Tool arguments remain ephemeral: neither requests nor decisions are persisted.
func (s *Server) approvalHook(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	if !eventCapability(p, r) {
		fail(w, 401, errors.New("invalid event capability"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxApprovalInput)
	var event approvalHookEvent
	if decode(w, r, &event) != nil {
		fail(w, 400, errors.New("invalid or oversized hook payload"))
		return
	}
	if (event.Provider != "claude" && event.Provider != "codex") ||
		(event.Event != "PermissionRequest" && event.Event != "SessionStart" && event.Event != "UserPromptSubmit") ||
		!hookText(event.PermissionMode, 80) || !hookText(event.Cwd, 4096) ||
		(event.Cwd != "" && !strings.HasPrefix(event.Cwd, "/")) {
		fail(w, 400, errors.New("unsupported hook event"))
		return
	}
	permission := event.Event == "PermissionRequest"
	if permission && (event.ToolName == "" || !hookText(event.ToolName, 240) || len(event.Input) == 0 || event.Input[0] != '{' || !json.Valid(event.Input)) {
		fail(w, 400, errors.New("invalid permission request"))
		return
	}
	now := time.Now().UTC()
	// Match the provider and immutable session identity before admitting a request.
	// Lock order is session -> approvals; no approvals path acquires a session lock.
	p.mu.Lock()
	if p.finished || p.stopped || p.meta.Status != "running" || p.meta.Purpose != "" || p.meta.Harness != event.Provider || p.meta.Permissions == nil || p.meta.Permissions.Support == "terminal-only" {
		p.mu.Unlock()
		fail(w, 409, errors.New("hook does not match an active configured agent"))
		return
	}
	permissions := *p.meta.Permissions
	permissions.Support = "active"
	permissions.Detail = "An authenticated provider hook has connected. Only live permission requests can be decided here; other prompts remain in the terminal."
	if event.PermissionMode != "" {
		permissions.Mode = event.PermissionMode
		permissions.ModeObservedAt = &now
	}
	p.meta.Permissions = &permissions
	p.meta.UpdatedAt = now
	var waiter *approvalWaiter
	if permission {
		if event.Cwd == "" {
			event.Cwd = p.meta.Cwd
		}
		id, idErr := newID()
		if idErr != nil {
			p.mu.Unlock()
			fail(w, 500, errors.New("could not create approval"))
			return
		}
		s.approvalMu.Lock()
		s.refreshApprovalsLocked(now)
		total, sessionCount := 0, 0
		for _, item := range s.approvals {
			if item.record.Status == "pending" {
				total++
				if item.record.SessionID == p.meta.ID {
					sessionCount++
				}
			}
		}
		if total >= 64 || sessionCount >= 4 {
			s.approvalMu.Unlock()
			p.mu.Unlock()
			fail(w, 429, errors.New("approval capacity reached; use the terminal"))
			return
		}
		s.pruneApprovalsLocked()
		ttl := s.approvalTTL
		if ttl <= 0 {
			ttl = approvalLifetime
		}
		waiter = &approvalWaiter{record: Approval{ID: id, SessionID: p.meta.ID, SessionCreatedAt: p.meta.CreatedAt, Provider: event.Provider, ToolName: event.ToolName, Input: event.Input, Cwd: event.Cwd, PermissionMode: event.PermissionMode, CreatedAt: now, ExpiresAt: now.Add(ttl), Status: "pending"}, session: p, disconnected: r.Context().Done(), wake: make(chan struct{})}
		if s.approvals == nil {
			s.approvals = make(map[string]*approvalWaiter)
		}
		s.approvals[id] = waiter
		s.approvalMu.Unlock()
		p.meta.Attention = &Attention{Kind: "permission", Source: event.Provider + "-hook", UpdatedAt: now}
	}
	p.mu.Unlock()
	// Persist descriptive capability only. A storage failure must never approve.
	s.mu.Lock()
	s.metadataDirty = true
	s.mu.Unlock()
	if waiter == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	timer := time.NewTimer(time.Until(waiter.record.ExpiresAt))
	defer timer.Stop()
	select {
	case <-waiter.wake:
	case <-timer.C:
	case <-r.Context().Done():
	case <-p.done:
	}
	s.approvalMu.Lock()
	s.refreshApprovalsLocked(time.Now().UTC())
	result := waiter.record
	s.approvalMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	// Empty decisions restore the provider's own terminal flow, never allow.
	decision := "terminal"
	if result.Status == "submitted" {
		decision = result.Decision
	}
	jsonResponse(w, 200, map[string]string{"decision": decision})
}

func finishApproval(item *approvalWaiter, status, decision, detail string) {
	if item.record.Status != "pending" {
		return
	}
	item.record.Status, item.record.Decision, item.record.Detail = status, decision, detail
	close(item.wake)
	item.session = nil
	item.disconnected = nil
}

func (s *Server) refreshApprovalsLocked(now time.Time) {
	for _, item := range s.approvals {
		if item.record.Status != "pending" {
			continue
		}
		select {
		case <-item.disconnected:
			finishApproval(item, "cancelled", "", "The provider hook disconnected; use the terminal.")
			continue
		default:
		}
		select {
		case <-item.session.done:
			finishApproval(item, "cancelled", "", "The session ended.")
			continue
		default:
		}
		if !now.Before(item.record.ExpiresAt) {
			finishApproval(item, "expired", "", "The request expired; use the provider terminal.")
		}
	}
}
func (s *Server) pruneApprovalsLocked() {
	for len(s.approvals) >= 128 {
		var oldest *approvalWaiter
		for _, item := range s.approvals {
			if item.record.Status != "pending" && (oldest == nil || item.record.CreatedAt.Before(oldest.record.CreatedAt)) {
				oldest = item
			}
		}
		if oldest == nil {
			return
		}
		delete(s.approvals, oldest.record.ID)
	}
}
func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	s.approvalMu.Lock()
	s.refreshApprovalsLocked(time.Now().UTC())
	records := make([]Approval, 0, len(s.approvals))
	for _, item := range s.approvals {
		records = append(records, item.record)
	}
	s.approvalMu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt.Before(records[j].CreatedAt) })
	jsonResponse(w, 200, map[string]any{"requests": records})
}
func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID        string    `json:"sessionId"`
		SessionCreatedAt time.Time `json:"sessionCreatedAt"`
		Decision         string    `json:"decision"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if decode(w, r, &request) != nil || (request.Decision != "allow" && request.Decision != "deny" && request.Decision != "terminal") {
		fail(w, 400, errors.New("invalid approval decision"))
		return
	}
	s.approvalMu.Lock()
	s.refreshApprovalsLocked(time.Now().UTC())
	item := s.approvals[r.PathValue("id")]
	var p *liveSession
	if item != nil {
		p = item.session
	}
	s.approvalMu.Unlock()
	if p == nil {
		fail(w, 409, errors.New("approval is stale or resolved"))
		return
	}
	// Serialize against stop/finish as well as competing dashboard decisions.
	p.mu.Lock()
	s.approvalMu.Lock()
	s.refreshApprovalsLocked(time.Now().UTC())
	if item.record.Status != "pending" || p.finished || p.stopped || item.record.SessionID != request.SessionID || !item.record.SessionCreatedAt.Equal(request.SessionCreatedAt) {
		s.approvalMu.Unlock()
		p.mu.Unlock()
		fail(w, 409, errors.New("approval is stale, resolved, or belongs to another session"))
		return
	}
	finishApproval(item, "submitted", request.Decision, "Decision submitted to the waiting hook; provider execution is not confirmed.")
	record := item.record
	s.approvalMu.Unlock()
	p.mu.Unlock()
	jsonResponse(w, 200, record)
}
