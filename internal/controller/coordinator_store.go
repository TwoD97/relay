package controller

import (
	"encoding/json"
	"errors"
	"github.com/TwoD97/relay/internal/peers"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type MigrationOperation struct {
	ID        string    `json:"id"`
	HostID    string    `json:"hostId"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type CoordinatorBinding struct {
	HostID        string    `json:"hostId"`
	CoordinatorID string    `json:"coordinatorId"`
	Target        string    `json:"-"`
	Port          int       `json:"-"`
	CreatedAt     time.Time `json:"-"`
}
type migrationRecord struct {
	LastHosted    *MastermindState    `json:"lastHosted,omitempty"`
	SourceStopped bool                `json:"sourceStopped"`
	Format        int                 `json:"format"`
	Operation     *MigrationOperation `json:"operation,omitempty"`
	Binding       *CoordinatorBinding `json:"hosted,omitempty"`
	Host          Host                `json:"host"`
	CoordinatorID string              `json:"coordinatorId"`
	SourceFrozen  bool                `json:"sourceFrozen"`
	SeedDigest    string              `json:"seedDigest,omitempty"`
	Source        mastermindRecord    `json:"source"`
}
type migrationStore struct {
	mu        sync.Mutex
	path      string
	record    migrationRecord
	err       string
	preparing bool
	plan      *migrationPlan
	client    coordinatorClientFactory
}

func openMigrationStore(dir string) *migrationStore {
	store := &migrationStore{path: filepath.Join(dir, "mastermind-migration.json"), record: migrationRecord{Format: 1}}
	data, err := readWorkspaceFile(store.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return store
	}
	if err != nil || strictWorkspaceJSON(data, &store.record) != nil || store.record.Format != 1 {
		store.err = "Mastermind migration state could not be verified. Preserve mastermind-migration.json before recovery."
		return store
	}
	rec := store.record
	if rec.SourceFrozen && rec.Operation == nil {
		store.err = "Migration fence has no operation identity"
		return store
	}
	if rec.Operation != nil && (rec.Source.Config == nil || validMastermindConfig(*rec.Source.Config) != nil || !validMastermindID(rec.Operation.ID) || rec.Operation.HostID != rec.Host.ID || !validMastermindID(rec.CoordinatorID)) {
		store.err = "Invalid migration identity"
		return store
	}
	if rec.Binding != nil {
		if rec.Operation == nil || rec.Binding.HostID != rec.Host.ID || rec.Binding.CoordinatorID != rec.CoordinatorID || !rec.SourceFrozen || !rec.SourceStopped {
			store.err = "Invalid hosted coordinator binding"
			return store
		}
		store.record.Binding.Target = rec.Host.Target
		store.record.Binding.Port = rec.Host.Port
		store.record.Binding.CreatedAt = rec.Host.CreatedAt
	}
	if rec.Operation != nil {
		switch rec.Operation.Status {
		case "preparing":
			store.record.Operation.Status = "failed"
			store.record.Operation.Stage = "Controller stopped before source quiesce; no agent activation was retried"
		case "quiescing", "starting":
			store.record.Operation.Status = "uncertain"
			store.record.Operation.Stage = "Handoff result requires read-only verification; no stop or activation was retried"
		case "active", "uncertain", "failed":
		default:
			store.err = "Invalid migration phase"
		}
	}
	return store
}
func (s *migrationStore) saveLocked(next migrationRecord) error {
	if s.err != "" {
		return errors.New(s.err)
	}
	next.Format = 1
	if next.Operation != nil {
		op := *next.Operation
		op.UpdatedAt = time.Now().UTC()
		next.Operation = &op
	}
	data, err := json.Marshal(next)
	if err != nil || len(data) > 256<<10 {
		return errors.New("Migration state exceeds its bounded size")
	}
	if err = writeRestartFile(s.path, data); err != nil {
		s.err = "Migration state save was not confirmed. No further handoff action will be dispatched; inspect the destination before recovery."
		return errors.New(s.err)
	}
	s.record = next
	return nil
}
func (s *migrationStore) snapshot() (migrationRecord, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.record
	if rec.Operation != nil {
		op := *rec.Operation
		rec.Operation = &op
	}
	if rec.Binding != nil {
		binding := *rec.Binding
		rec.Binding = &binding
	}
	return rec, s.err
}
func (s *Server) coordinatorDelegated() bool {
	if s.migration == nil {
		return false
	}
	s.migration.mu.Lock()
	defer s.migration.mu.Unlock()
	return s.migration.record.SourceFrozen || s.migration.err != ""
}

type coordinatorSeed struct {
	SourceRevision uint64          `json:"sourceRevision"`
	ID             string          `json:"id"`
	SourceID       string          `json:"sourceId"`
	Harness        string          `json:"harness"`
	AgentMode      string          `json:"agentMode"`
	Workspace      workspaceRecord `json:"workspace"`
	Control        controlRecord   `json:"control"`
	Hosts          []Host          `json:"hosts"`
}
type coordinatorReceipt struct {
	SourceRevision uint64 `json:"sourceRevision"`
	Format         int    `json:"format"`
	ID             string `json:"id"`
	SourceID       string `json:"sourceId"`
	Digest         string `json:"digest"`
	Phase          string `json:"phase"`
	Harness        string `json:"harness"`
	AgentMode      string `json:"agentMode"`
	Error          string `json:"error,omitempty"`
}
type coordinatorStore struct {
	mu      sync.Mutex
	path    string
	receipt coordinatorReceipt
	err     string
}

func openCoordinatorStore(dir string) *coordinatorStore {
	s := &coordinatorStore{path: filepath.Join(dir, "coordinator-receipt.json"), receipt: coordinatorReceipt{Format: 1}}
	data, err := readWorkspaceFile(s.path, 4096)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err != nil || strictWorkspaceJSON(data, &s.receipt) != nil || s.receipt.Format != 1 || !validMastermindID(s.receipt.ID) || len(s.receipt.Digest) != 64 || !peers.ValidSource(s.receipt.SourceID) || (s.receipt.Phase != "seeding" && s.receipt.Phase != "ready" && s.receipt.Phase != "activating" && s.receipt.Phase != "activated" && s.receipt.Phase != "uncertain") {
		s.err = "Hosted coordinator receipt could not be verified"
	}
	return s
}
func (s *coordinatorStore) saveLocked(next coordinatorReceipt) error {
	if s.err != "" {
		return errors.New(s.err)
	}
	data, _ := json.Marshal(next)
	if err := writeRestartFile(s.path, data); err != nil {
		s.err = "Hosted coordinator receipt save was not confirmed"
		return errors.New(s.err)
	}
	s.receipt = next
	return nil
}
