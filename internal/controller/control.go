package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

func (s *Server) withActiveCoordinator(w http.ResponseWriter, r *http.Request, action func(string) error) bool {
	launch, _ := r.Context().Value(authenticatedCoordinatorKey{}).(string)
	s.mastermind.mu.Lock()
	defer s.mastermind.mu.Unlock()
	if !validMastermindID(launch) || !s.mastermind.record.Enabled || s.mastermind.record.Token != launch {
		writeError(w, 401, "This coordinator's capability was revoked. No instruction was handled.")
		return false
	}
	if err := action(launch); err != nil {
		writeError(w, 409, err.Error())
		return false
	}
	return true
}

func (s *Server) RegisterControlRoutes(mux *http.ServeMux) {
	s.registerPersonalWhatsApp(mux)
	s.RegisterWhatsAppBusinessRoutes(mux)
	mux.HandleFunc("GET /api/mastermind/control", s.controlState)
	mux.HandleFunc("POST /api/mastermind/control/config", s.configureControl)
	mux.HandleFunc("POST /api/mastermind/control/messages", s.enqueueControl)
	mux.HandleFunc("POST /api/mastermind/control/messages/{message}/cancel", s.cancelControl)
	mux.HandleFunc("DELETE /api/mastermind/control/lessons/{lesson}", s.deleteLesson)
	mux.HandleFunc("POST /api/mastermind/control/discord", s.configureDiscord)
	mux.HandleFunc("DELETE /api/mastermind/control/discord", s.disconnectDiscord)
	mux.HandleFunc("POST /api/mastermind/control/telegram", s.configureTelegram)
	mux.HandleFunc("DELETE /api/mastermind/control/telegram", s.disconnectTelegram)
}
func (s *Server) RegisterAgentControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /agent/control", s.controlState)
	mux.HandleFunc("GET /agent/control/wait", s.waitControl)
	mux.HandleFunc("POST /agent/control/messages/{message}/claim", s.claimControl)
	mux.HandleFunc("POST /agent/control/messages/{message}/reply", s.replyControl)
	mux.HandleFunc("POST /agent/control/lessons", s.addLesson)
}
func (s *Server) controlState(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/agent/control" {
		s.withActiveCoordinator(w, r, func(string) error { s.controlStateContent(w, r); return nil })
		return
	}
	s.controlStateContent(w, r)
}
func (s *Server) controlStateContent(w http.ResponseWriter, r *http.Request) {
	state, listener, err := s.control.snapshot()
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	// Claim identifiers and deduplication keys are private bookkeeping.
	for i := range state.Messages {
		state.Messages[i].Claim = ""
		state.Messages[i].Key = ""
	}
	writeJSON(w, 200, map[string]any{"revision": state.Revision, "recovery": state.Recovery, "messages": state.Messages, "lessons": state.Lessons, "listenerAt": listener, "personalWhatsApp": s.personalWhatsApp.view(), "telegram": s.telegram.view(), "discord": s.discord.view()})
}
func (s *Server) configureControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Revision uint64 `json:"revision"`
		Recovery bool   `json:"recovery"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	err := s.control.update(func(state *controlRecord) error {
		if state.Revision != body.Revision {
			return fmt.Errorf("Control settings changed; refresh before saving")
		}
		state.Recovery = body.Recovery
		return nil
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.controlState(w, r)
}
func (s *Server) enqueueControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	item, err := s.control.enqueue(body.ID, "desktop", body.Text, "")
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	item.Claim = ""
	item.Key = ""
	writeJSON(w, 201, item)
}
func (s *Server) cancelControl(w http.ResponseWriter, r *http.Request) {
	err := s.control.update(func(state *controlRecord) error {
		for i := range state.Messages {
			item := &state.Messages[i]
			if item.ID != r.PathValue("message") {
				continue
			}
			if item.Status != "queued" {
				return fmt.Errorf("Only queued instructions can be cancelled; processing work must be reviewed in the terminal")
			}
			item.Status = "cancelled"
			item.UpdatedAt = time.Now().UTC()
			return nil
		}
		return fmt.Errorf("Instruction not found")
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.controlState(w, r)
}
func (s *Server) waitControl(w http.ResponseWriter, r *http.Request) {
	// A read is never a claim: a lost polling response cannot consume work.
	s.control.mu.Lock()
	s.control.listener = time.Now().UTC()
	s.control.mu.Unlock()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		state, _, err := s.control.snapshot()
		if err != nil {
			writeError(w, 503, err.Error())
			return
		}
		queued := []ControlMessage{}
		for _, item := range state.Messages {
			if item.Status == "queued" {
				item.Key = ""
				queued = append(queued, item)
			}
		}
		if len(queued) > 0 {
			s.withActiveCoordinator(w, r, func(string) error { writeJSON(w, 200, map[string]any{"messages": queued}); return nil })
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			writeError(w, 503, "Relay is reconnecting")
			return
		case <-timer.C:
			s.withActiveCoordinator(w, r, func(string) error { writeJSON(w, 200, map[string]any{"messages": queued}); return nil })
			return
		case <-s.control.wake:
		}
	}
}
func (s *Server) claimControl(w http.ResponseWriter, r *http.Request) {
	s.withActiveCoordinator(w, r, func(launch string) error {
		item, err := s.control.claim(r.PathValue("message"), launch)
		if err != nil {
			return err
		}
		item.Claim = ""
		item.Key = ""
		writeJSON(w, 200, item)
		return nil
	})
}
func (s *Server) replyControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.withActiveCoordinator(w, r, func(launch string) error {
		if err := s.control.complete(r.PathValue("message"), launch, body.Text); err != nil {
			return err
		}
		s.controlStateContent(w, r)
		return nil
	})
}
func (s *Server) addLesson(w http.ResponseWriter, r *http.Request) {
	var item ControlLesson
	if err := decodeJSON(w, r, &item); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !validMastermindID(item.ID) || !boundedControlText(item.Problem, 2000) || !boundedControlText(item.Action, 2000) || !boundedControlText(item.Evidence, 2000) || (item.Outcome != "success" && item.Outcome != "failed") {
		writeError(w, 400, "A lesson needs its ID, problem, action, success/failed outcome, and verifiable evidence (2000 characters per field)")
		return
	}
	item.CreatedAt = time.Now().UTC()
	s.withActiveCoordinator(w, r, func(string) error {
		if err := s.control.update(func(state *controlRecord) error {
			for _, old := range state.Lessons {
				if old.ID == item.ID {
					return fmt.Errorf("Lesson already exists; inspect it before repeating")
				}
			}
			if len(state.Lessons) >= 100 {
				return fmt.Errorf("Review and remove old lessons before adding more")
			}
			state.Lessons = append(state.Lessons, item)
			return nil
		}); err != nil {
			return err
		}
		writeJSON(w, 201, item)
		return nil
	})
}
func (s *Server) deleteLesson(w http.ResponseWriter, r *http.Request) {
	err := s.control.update(func(state *controlRecord) error {
		for i, item := range state.Lessons {
			if item.ID == r.PathValue("lesson") {
				state.Lessons = append(state.Lessons[:i], state.Lessons[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("Lesson not found")
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.controlState(w, r)
}

func (s *Server) monitorControl() {
	defer s.controlWG.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.queueRecovery()
		}
	}
}
func (s *Server) queueRecovery() {
	state, _, err := s.control.snapshot()
	if err != nil || !state.Recovery || s.workspace == nil {
		return
	}
	s.workspace.mu.Lock()
	tasks := append([]WorkspaceTask{}, s.workspace.record.Tasks...)
	s.workspace.mu.Unlock()
	for _, task := range tasks {
		if task.Status != "blocked" || task.HostID == "" || task.SessionID == "" {
			continue
		}
		// One request per blocked episode, not per heartbeat or detail edit.
		key := "blocked:" + task.ID
		if state.RecoverySeen[key] {
			continue
		}
		exists := false
		for _, message := range state.Messages {
			if message.Key == key {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		sum := sha256.Sum256([]byte(key))
		id := hex.EncodeToString(sum[:16])
		instruction := fmt.Sprintf("Diagnose blocked task %s (%s), host %s, session %s. Read current task and exact live session identity first. Inspect evidence and attempt at most one bounded, non-destructive recovery. Ask the user before destructive actions, restarting/interruption, or bypassing a permission decision. Do not infer failure from silence. Report result and update the task; record a lesson only with evidence. Blocker: %s", task.ID, task.Title, task.HostID, task.SessionID, task.Detail)
		_, _ = s.control.enqueue(id, "recovery", instruction, key)
	}
}
