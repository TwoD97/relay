package controller

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Control messages are instructions, never terminal keystrokes. A coordinator
// claims each once and reports an outcome; uncertain work is not redispatched.
type ControlMessage struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Text      string    `json:"text"`
	Status    string    `json:"status"`
	Reply     string    `json:"reply,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Claim     string    `json:"claim,omitempty"`
	Key       string    `json:"key,omitempty"`
	Delivery  string    `json:"delivery,omitempty"`
}
type ControlLesson struct {
	ID        string    `json:"id"`
	Problem   string    `json:"problem"`
	Action    string    `json:"action"`
	Outcome   string    `json:"outcome"`
	Evidence  string    `json:"evidence"`
	CreatedAt time.Time `json:"createdAt"`
}
type controlRecord struct {
	FrozenBy     string           `json:"frozenBy,omitempty"`
	Format       int              `json:"format"`
	Revision     uint64           `json:"revision"`
	Recovery     bool             `json:"recovery"`
	Messages     []ControlMessage `json:"messages"`
	Lessons      []ControlLesson  `json:"lessons"`
	RecoverySeen map[string]bool  `json:"recoverySeen,omitempty"`
}
type controlStore struct {
	mu       sync.Mutex
	path     string
	record   controlRecord
	err      error
	listener time.Time
	wake     chan struct{}
}

func openControlStore(dir string) *controlStore {
	s := &controlStore{path: filepath.Join(dir, "control-center.json"), wake: make(chan struct{}, 1), record: controlRecord{Format: 1, Recovery: true, Messages: []ControlMessage{}, Lessons: []ControlLesson{}}}
	data, err := readWorkspaceFile(s.path, 2<<20)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		err = strictWorkspaceJSON(data, &s.record)
	}
	if err == nil && (s.record.Format != 1 || len(s.record.Messages) > 200 || len(s.record.Lessons) > 100) {
		err = errors.New("invalid control state")
	}
	if err == nil {
		err = validateControlRecord(s.record)
	}
	if err != nil {
		s.err = errors.New("Mastermind control state could not be read. Preserve control-center.json before recovery.")
	}
	return s
}

func validateControlRecord(record controlRecord) error {
	if record.FrozenBy != "" && !validMastermindID(record.FrozenBy) {
		return errors.New("invalid inbox migration fence")
	}
	seen := map[string]bool{}
	for _, message := range record.Messages {
		if !validMastermindID(message.ID) || seen[message.ID] || !boundedControlText(message.Text, 8000) || message.CreatedAt.IsZero() || message.UpdatedAt.IsZero() {
			return errors.New("invalid inbox entry")
		}
		seen[message.ID] = true
		if message.Source != "desktop" && message.Source != "telegram" && message.Source != "recovery" && message.Source != "discord" && message.Source != "whatsapp-personal" && message.Source != "whatsapp-business" {
			return errors.New("invalid instruction source")
		}
		if message.Status != "queued" && message.Status != "processing" && message.Status != "completed" && message.Status != "cancelled" {
			return errors.New("invalid instruction state")
		}
		if (message.Status == "processing" || message.Status == "completed") && message.Claim == "" {
			return errors.New("instruction has no claim owner")
		}
		if message.Status == "completed" && !boundedControlText(message.Reply, 8000) {
			return errors.New("completed instruction has no result")
		}
	}
	seen = map[string]bool{}
	for _, lesson := range record.Lessons {
		if !validMastermindID(lesson.ID) || seen[lesson.ID] || !boundedControlText(lesson.Problem, 2000) || !boundedControlText(lesson.Action, 2000) || !boundedControlText(lesson.Evidence, 2000) || (lesson.Outcome != "success" && lesson.Outcome != "failed") {
			return errors.New("invalid lesson")
		}
		seen[lesson.ID] = true
	}
	return nil
}

func saveControlFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".control-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncStoreDirectory(filepath.Dir(path))
}
func (s *controlStore) snapshot() (controlRecord, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.record
	r.Messages = append([]ControlMessage{}, r.Messages...)
	r.Lessons = append([]ControlLesson{}, r.Lessons...)
	r.RecoverySeen = make(map[string]bool, len(s.record.RecoverySeen))
	for key, value := range s.record.RecoverySeen {
		r.RecoverySeen[key] = value
	}
	return r, s.listener, s.err
}
func (s *controlStore) update(change func(*controlRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.FrozenBy != "" {
		return errors.New("This inbox moved to another coordinator. Refresh Relay before sending another instruction.")
	}
	if s.err != nil {
		return s.err
	}
	r := s.record
	r.Messages = append([]ControlMessage{}, r.Messages...)
	r.Lessons = append([]ControlLesson{}, r.Lessons...)
	r.RecoverySeen = make(map[string]bool, len(s.record.RecoverySeen))
	for key, value := range s.record.RecoverySeen {
		r.RecoverySeen[key] = value
	}
	if err := change(&r); err != nil {
		return err
	}
	r.Revision++
	if err := validateControlRecord(r); err != nil {
		return err
	}
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded) > 2<<20 {
		return errors.New("The control inbox reached its 2 MiB limit. Review and remove old lessons or shorten the result.")
	}
	if err := saveControlFile(s.path, r); err != nil {
		s.err = errors.New("Control state save could not be confirmed. Restart Relay to inspect it before repeating this action.")
		return s.err
	}
	s.record = r
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}
func (s *controlStore) enqueue(id, source, text, key string) (ControlMessage, error) {
	text = strings.TrimSpace(text)
	if !validMastermindID(id) || !boundedControlText(text, 8000) {
		return ControlMessage{}, errors.New("An exact message ID and 1–8000 characters of instruction are required")
	}
	var result ControlMessage
	err := s.update(func(r *controlRecord) error {
		for _, item := range r.Messages {
			if item.ID == id || key != "" && item.Key == key {
				if item.Text != text {
					return errors.New("This message identity already has different content")
				}
				result = item
				return nil
			}
		}
		if source == "recovery" && key != "" {
			if r.RecoverySeen[key] {
				return errors.New("This blocked task was already diagnosed; send a fresh instruction to request another attempt")
			}
			if len(r.RecoverySeen) >= 200 {
				return errors.New("Recovery history is full; request further diagnosis explicitly")
			}
			r.RecoverySeen[key] = true
		}
		if len(r.Messages) >= 200 {
			index := -1
			for i, item := range r.Messages {
				if item.Status == "completed" || item.Status == "cancelled" {
					index = i
					break
				}
			}
			if index < 0 {
				return errors.New("Mastermind inbox is full; review existing messages first")
			}
			r.Messages = append(r.Messages[:index], r.Messages[index+1:]...)
		}
		now := time.Now().UTC()
		result = ControlMessage{ID: id, Source: source, Text: text, Status: "queued", Key: key, CreatedAt: now, UpdatedAt: now}
		r.Messages = append(r.Messages, result)
		return nil
	})
	return result, err
}
func boundedControlText(text string, max int) bool {
	return strings.TrimSpace(text) != "" && utf8.ValidString(text) && utf8.RuneCountInString(text) <= max && !strings.ContainsRune(text, 0)
}

func (s *controlStore) claim(id, launch string) (ControlMessage, error) {
	var result ControlMessage
	err := s.update(func(r *controlRecord) error {
		for i := range r.Messages {
			item := &r.Messages[i]
			if item.ID != id {
				continue
			}
			if item.Status != "queued" {
				return errors.New("Instruction was already claimed or completed; inspect its outcome instead of repeating it")
			}
			item.Status = "processing"
			item.Claim = launch
			item.UpdatedAt = time.Now().UTC()
			result = *item
			return nil
		}
		return errors.New("Instruction no longer exists")
	})
	return result, err
}
func (s *controlStore) complete(id, launch, reply string) error {
	if !boundedControlText(reply, 8000) {
		return errors.New("Provide a concise result, at most 8000 characters")
	}
	return s.update(func(r *controlRecord) error {
		for i := range r.Messages {
			item := &r.Messages[i]
			if item.ID != id {
				continue
			}
			if item.Status != "processing" || item.Claim != launch {
				return errors.New("Only the coordinator that claimed this instruction can complete it")
			}
			item.Status = "completed"
			item.Reply = reply
			item.UpdatedAt = time.Now().UTC()
			return nil
		}
		return errors.New("Instruction no longer exists")
	})
}
