//go:build linux

package runtime

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

type Attention struct {
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Server) sessionEvent(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		fail(w, 401, errors.New("invalid event capability"))
		return
	}
	bearer := strings.TrimPrefix(authorization, "Bearer ")
	p.mu.Lock()
	token := p.eventToken
	p.mu.Unlock()
	if token == "" || subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) != 1 {
		fail(w, 401, errors.New("invalid event capability"))
		return
	}
	var event struct {
		Kind   string `json:"kind"`
		Source string `json:"source"`
	}
	if err = decode(w, r, &event); err != nil {
		fail(w, 400, err)
		return
	}
	if event.Kind != "working" && event.Kind != "completed" && event.Kind != "notification" && event.Kind != "permission" {
		fail(w, 400, errors.New("unsupported event kind"))
		return
	}
	s.mu.Lock()
	p.mu.Lock()
	validSource := (event.Source == "claude-hook" && p.meta.Harness == "claude") || (event.Source == "codex-notify" && p.meta.Harness == "codex")
	if !validSource || p.finished || p.stopped {
		p.mu.Unlock()
		s.mu.Unlock()
		fail(w, 409, errors.New("event does not match an active harness"))
		return
	}
	if event.Kind == "working" {
		p.meta.Attention = nil
	} else {
		p.meta.Attention = &Attention{Kind: event.Kind, Source: event.Source, UpdatedAt: time.Now().UTC()}
	}
	p.meta.UpdatedAt = time.Now().UTC()
	p.mu.Unlock()
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) ackAttention(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	p, ok := s.sessions[r.PathValue("id")]
	if !ok {
		s.mu.Unlock()
		fail(w, 404, os.ErrNotExist)
		return
	}
	p.mu.Lock()
	p.meta.Attention = nil
	p.meta.UpdatedAt = time.Now().UTC()
	p.mu.Unlock()
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

// Hook commands carry no secret in argv. notify reads the owning session's
// capability from its environment and discards provider payloads.
func harnessArguments(id, executable string) []string {
	if id == "codex" {
		notify, _ := json.Marshal([]string{executable, "notify", "--source", "codex-notify", "--kind", "completed"})
		return []string{"-c", "notify=" + string(notify)}
	}
	if id != "claude" {
		return nil
	}
	command := func(kind string) map[string]any {
		return map[string]any{"type": "command", "command": shellQuote(executable) + " notify --source claude-hook --kind " + kind, "timeout": 3}
	}
	entry := func(kind, matcher string) map[string]any {
		e := map[string]any{"hooks": []any{command(kind)}}
		if matcher != "" {
			e["matcher"] = matcher
		}
		return e
	}
	hooks := map[string]any{
		"UserPromptSubmit":  []any{entry("working", "")},
		"PostToolUse":       []any{entry("working", "")},
		"PermissionRequest": []any{entry("permission", "")},
		"Stop":              []any{entry("completed", "")},
		"Notification":      []any{entry("permission", "permission_prompt"), entry("notification", "idle_prompt")},
	}
	settings, _ := json.Marshal(map[string]any{"hooks": hooks})
	return []string{"--settings", string(settings)}
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
