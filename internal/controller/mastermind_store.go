package controller

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TwoD97/relay/internal/transport"
)

const maxMastermindRevision = uint64(1<<53 - 1)

type MastermindConfig struct {
	HostID    string `json:"hostId"`
	Harness   string `json:"harness"`
	AgentMode string `json:"agentMode"`
	Cwd       string `json:"cwd"`
}

// This deliberately contains no transcript, provider credentials, or process
// control capability. Ownership is verified against the runtime's metadata.
type MastermindSession struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Status       string    `json:"status"`
	Harness      string    `json:"harness"`
	AgentMode    string    `json:"agentMode,omitempty"`
	MastermindID string    `json:"mastermindId,omitempty"`
	Title        string    `json:"title"`
	Workspace    string    `json:"workspace"`
	Cwd          string    `json:"cwd"`
	Purpose      string    `json:"purpose,omitempty"`
	ExitCode     *int      `json:"exitCode,omitempty"`
}

type MastermindState struct {
	Revision  uint64             `json:"revision"`
	Enabled   bool               `json:"enabled"`
	Config    *MastermindConfig  `json:"config,omitempty"`
	Status    string             `json:"status"`
	Execution string             `json:"execution,omitempty"`
	Blocker   string             `json:"blocker,omitempty"`
	Stage     string             `json:"stage"`
	Error     string             `json:"error,omitempty"`
	Session   *MastermindSession `json:"session,omitempty"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

type mastermindRecord struct {
	Format int `json:"format"`
	MastermindState
	// queued is the only phase allowed to dispatch one create. creating is
	// committed before dispatch; all later recovery is read-only by token.
	Phase          string    `json:"phase"`
	Token          string    `json:"token,omitempty"`
	CanonicalCwd   string    `json:"canonicalCwd,omitempty"`
	HostTarget     string    `json:"hostTarget,omitempty"`
	HostPort       int       `json:"hostPort,omitempty"`
	HostCreatedAt  time.Time `json:"hostCreatedAt,omitempty"`
	WorkerPath     string    `json:"workerPath,omitempty"`
	WorkerStateDir string    `json:"workerStateDir,omitempty"`
}

type mastermindStore struct {
	mu         sync.Mutex
	path       string
	record     mastermindRecord
	err        string
	loadFailed bool
}

func unavailableMastermindStore(dir string) *mastermindStore {
	message := "Mastermind settings could not be read. Preserve or restore mastermind.json before configuring another session. Other Relay features remain available."
	return &mastermindStore{path: filepath.Join(dir, "mastermind.json"), err: message, loadFailed: true, record: mastermindRecord{Format: 1, MastermindState: MastermindState{Status: "uncertain", Blocker: "storage", Stage: "Mastermind settings need recovery", Error: message, UpdatedAt: time.Now().UTC()}}}
}

func validMastermindWorkerPath(value string) bool {
	return strings.HasPrefix(value, "/") && len(value) <= 4096 && strings.IndexFunc(value, unicode.IsControl) < 0
}

func mastermindPendingPhase(phase string) bool {
	return phase == "creating" || phase == "worker-prepared" || phase == "worker-dispatched"
}

func validMastermindID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func validMastermindConfig(cfg MastermindConfig) error {
	if cfg.HostID == "" || len(cfg.HostID) > 128 || (cfg.Harness != "claude" && cfg.Harness != "codex") ||
		(cfg.AgentMode != "manual" && cfg.AgentMode != "auto") || cfg.Cwd == "" {
		return errors.New("Choose a machine, Claude Code or Codex, a permission mode, and a working folder")
	}
	return (transport.DirectoryQuery{Path: cfg.Cwd}).Validate()
}

func cloneMastermind(record mastermindRecord) mastermindRecord {
	if record.Config != nil {
		cfg := *record.Config
		record.Config = &cfg
	}
	if record.Session != nil {
		session := *record.Session
		if session.ExitCode != nil {
			code := *session.ExitCode
			session.ExitCode = &code
		}
		record.Session = &session
	}
	return record
}

func openMastermindStore(dir string) (*mastermindStore, error) {
	s := &mastermindStore{path: filepath.Join(dir, "mastermind.json"), record: mastermindRecord{
		Format: 1, MastermindState: MastermindState{Status: "unconfigured", Stage: "Choose your Mastermind", UpdatedAt: time.Now().UTC()},
	}}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || len(data) > 64<<10 {
		return nil, errors.New("Could not read Mastermind state; preserve mastermind.json for inspection")
	}
	var rec mastermindRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("read Mastermind state: %w", err)
	}
	if rec.Execution == "" && rec.Phase != "queued" {
		rec.Execution = "native"
	}
	if rec.Format != 1 || rec.Revision > maxMastermindRevision || rec.Config == nil || validMastermindConfig(*rec.Config) != nil || rec.HostTarget == "" ||
		(rec.Execution != "" && rec.Execution != "native" && rec.Execution != "worker") ||
		(rec.Phase != "queued" && !mastermindPendingPhase(rec.Phase) && rec.Phase != "attached" && rec.Phase != "rejected") ||
		(rec.Phase != "queued" && (!validMastermindID(rec.Token) || rec.CanonicalCwd == "")) ||
		((rec.Phase == "worker-prepared" || rec.Phase == "worker-dispatched") && rec.Execution != "worker") ||
		(rec.Execution == "worker" && (!validMastermindWorkerPath(rec.WorkerPath) || (rec.WorkerStateDir != "" && !validMastermindWorkerPath(rec.WorkerStateDir)))) ||
		((rec.Phase == "attached" || rec.Phase == "worker-prepared" || rec.Phase == "worker-dispatched") && (rec.Session == nil || !mastermindMatches(rec, *rec.Session))) ||
		(rec.Session != nil && rec.Phase != "attached" && rec.Phase != "worker-prepared" && rec.Phase != "worker-dispatched") {
		return nil, errors.New("Invalid Mastermind state; preserve mastermind.json for inspection")
	}
	if !rec.Enabled {
		rec.Status, rec.Stage = "disabled", "Mastermind is disabled; its session is preserved"
	} else if mastermindPendingPhase(rec.Phase) {
		rec.Status, rec.Stage = "uncertain", "Checking a previous Mastermind startup; nothing will be replayed"
	} else if rec.Phase == "rejected" {
		rec.Status, rec.Stage = "blocked", "The previous start was rejected; retry after resolving setup"
		rec.Blocker = "launch"
	} else {
		rec.Status, rec.Stage = "waiting", "Waiting to reconnect to the Mastermind machine"
	}
	s.record = rec
	return s, nil
}

func (s *mastermindStore) current() mastermindRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMastermind(s.record)
}

func (s *mastermindStore) saveLocked(next mastermindRecord) error {
	next.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil || len(data) > 64<<10 {
		return errors.New("Mastermind state exceeds its size limit")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".mastermind-*")
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
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	if err = syncStoreDirectory(filepath.Dir(s.path)); err != nil {
		return err
	}
	s.record, s.err = cloneMastermind(next), ""
	return nil
}

// Every worker transition is scoped to its configuration revision. A late
// response cannot replace the terminal selected by a newer configuration.
func (s *mastermindStore) transition(revision uint64, change func(*mastermindRecord)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Revision != revision || !s.record.Enabled || s.err != "" {
		return false
	}
	next := cloneMastermind(s.record)
	change(&next)
	if err := s.saveLocked(next); err != nil {
		s.err = "Mastermind state could not be saved. No further start requests will be sent; inspect this machine's sessions before retrying."
		s.record.Status, s.record.Stage, s.record.Error = "uncertain", "Mastermind state needs attention", s.err
		s.record.Blocker = "storage"
		return false
	}
	return true
}

func (s *mastermindStore) notice(revision uint64, status, stage, message string, reason ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Revision != revision || !s.record.Enabled || s.err != "" {
		return
	}
	blocker := ""
	if len(reason) != 0 {
		blocker = reason[0]
	}
	if s.record.Status == status && s.record.Stage == stage && s.record.Error == message && s.record.Blocker == blocker {
		return
	}
	s.record.Status, s.record.Stage, s.record.Error = status, stage, message
	s.record.Blocker = blocker
	s.record.UpdatedAt = time.Now().UTC()
}
