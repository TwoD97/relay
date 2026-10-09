package controller

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

const maxMaintenanceJobs = 256
const maxActiveMaintenanceJobs = 16

// MaintenanceJob is an operation record, never evidence inferred from a session
// title or purpose. Only the separately persisted session identity grants cleanup.
type MaintenanceJob struct {
	ID               string     `json:"id"`
	HostID           string     `json:"hostId"`
	Harness          string     `json:"harness"`
	Action           string     `json:"action"`
	Status           string     `json:"status"`
	Stage            string     `json:"stage"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	SessionID        string     `json:"sessionId,omitempty"`
	SessionCreatedAt *time.Time `json:"sessionCreatedAt,omitempty"`
	Error            string     `json:"error,omitempty"`
	CleanupStatus    string     `json:"cleanupStatus,omitempty"`
}

type maintenanceRecord struct {
	Job           MaintenanceJob `json:"job"`
	HostTarget    string         `json:"hostTarget"`
	HostPort      int            `json:"hostPort"`
	HostCreatedAt time.Time      `json:"hostCreatedAt"`
	// accepted -> creating -> input -> observing -> deleting -> done. Each
	// mutation's phase is flushed before dispatch, so restart never replays it.
	Phase string `json:"phase"`
}

type maintenanceStore struct {
	mu      sync.Mutex
	path    string
	records []maintenanceRecord
}

func validMaintenanceSessionID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16
}

func activeMaintenance(status string) bool {
	return status == "preparing" || status == "starting" || status == "running"
}

func openMaintenanceStore(dir string) (*maintenanceStore, error) {
	s := &maintenanceStore{path: filepath.Join(dir, "maintenance-jobs.json"), records: []maintenanceRecord{}}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	closeErr := f.Close() // Windows cannot replace this file with its read handle open.
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > 2<<20 {
		return nil, errors.New("maintenance job store exceeds size limit")
	}
	var disk struct {
		Format int                 `json:"format"`
		Jobs   []maintenanceRecord `json:"jobs"`
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, fmt.Errorf("read maintenance jobs: %w", err)
	}
	if disk.Format != 1 || len(disk.Jobs) > maxMaintenanceJobs {
		return nil, errors.New("invalid maintenance job store")
	}
	seen := map[string]bool{}
	active := 0
	for i := range disk.Jobs {
		rec := &disk.Jobs[i]
		job := &rec.Job
		id, err := hex.DecodeString(job.ID)
		if err != nil || len(id) != 24 || seen[job.ID] || job.HostID == "" || job.CreatedAt.IsZero() ||
			(job.Harness != "claude" && job.Harness != "codex") || (job.Action != "update" && job.Action != "repair") ||
			(job.SessionID != "" && (!validMaintenanceSessionID(job.SessionID) || job.SessionCreatedAt == nil || job.SessionCreatedAt.IsZero())) {
			return nil, errors.New("invalid maintenance job identity; preserve the store for inspection")
		}
		validPhaseStatus := (rec.Phase == "accepted" && job.Status == "preparing") ||
			((rec.Phase == "creating" || rec.Phase == "input") && job.Status == "starting") ||
			((rec.Phase == "observing" || rec.Phase == "deleting") && job.Status == "running") ||
			(rec.Phase == "done" && (job.Status == "succeeded" || job.Status == "failed" || job.Status == "uncertain"))
		if !validPhaseStatus || len(job.Stage) > 1024 || len(job.Error) > 4096 {
			return nil, errors.New("invalid maintenance job state")
		}
		if activeMaintenance(job.Status) {
			active++
		}
		if active > maxActiveMaintenanceJobs {
			return nil, errors.New("too many active maintenance jobs")
		}
		seen[job.ID] = true
		switch rec.Phase {
		case "accepted":
			job.Status, job.Stage, job.Error = "failed", "Preparation was interrupted", "The controller stopped before session creation. Start a new operation when ready."
			rec.Phase = "done"
		case "creating", "input":
			job.Status, job.Stage, job.Error = "uncertain", "Maintenance startup needs inspection", "The controller stopped while a session or command was being started. Nothing was replayed; inspect retained output before starting another operation."
			job.CleanupStatus, rec.Phase = "retained", "done"
		case "observing", "deleting":
			if job.SessionID == "" {
				return nil, errors.New("maintenance observation has no owned session")
			}
		case "done":
			if activeMaintenance(job.Status) {
				return nil, errors.New("invalid completed maintenance state")
			}
		default:
			return nil, errors.New("invalid maintenance phase")
		}
	}
	s.records = disk.Jobs
	// Recovery decisions must be durable before any polling or new operation.
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *maintenanceStore) saveLocked() error {
	data, err := json.MarshalIndent(struct {
		Format int                 `json:"format"`
		Jobs   []maintenanceRecord `json:"jobs"`
	}{1, s.records}, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return errors.New("maintenance job store exceeds size limit")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".maintenance-*")
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
	return syncStoreDirectory(filepath.Dir(s.path))
}

func (s *maintenanceStore) list() []MaintenanceJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]MaintenanceJob, 0, len(s.records))
	for _, rec := range s.records {
		jobs = append(jobs, rec.Job)
	}
	return jobs
}
func (s *maintenanceStore) pending() []maintenanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := []maintenanceRecord{}
	for _, rec := range s.records {
		if rec.Phase == "observing" || rec.Phase == "deleting" {
			records = append(records, rec)
		}
	}
	return records
}
func (s *maintenanceStore) get(id string) (maintenanceRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.records {
		if rec.Job.ID == id {
			return rec, true
		}
	}
	return maintenanceRecord{}, false
}
func (s *maintenanceStore) update(id string, change func(*maintenanceRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].Job.ID != id {
			continue
		}
		old := s.records[i]
		change(&s.records[i])
		s.records[i].Job.Stage = boundedMaintenanceText(s.records[i].Job.Stage, 1024)
		s.records[i].Job.Error = boundedMaintenanceText(s.records[i].Job.Error, 4096)
		s.records[i].Job.UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			s.records[i] = old
			return err
		}
		return nil
	}
	return errors.New("maintenance job no longer exists")
}
func (s *maintenanceStore) add(host Host, harness, action string) (MaintenanceJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := 0
	for _, rec := range s.records {
		if !activeMaintenance(rec.Job.Status) {
			continue
		}
		active++
		if rec.Job.HostID == host.ID && rec.Job.Harness == harness {
			return MaintenanceJob{}, errors.New("this harness already has a maintenance job running")
		}
	}
	if active >= maxActiveMaintenanceJobs {
		return MaintenanceJob{}, errors.New("16 maintenance jobs are active; wait for one to finish")
	}
	old := s.records
	records := append([]maintenanceRecord{}, old...)
	if len(records) == maxMaintenanceJobs {
		// Never discard unresolved ownership or failed output to make room.
		pruned := false
		for i, rec := range records {
			if rec.Job.Status == "succeeded" && rec.Job.CleanupStatus == "removed" {
				records = append(records[:i], records[i+1:]...)
				pruned = true
				break
			}
		}
		if !pruned {
			return MaintenanceJob{}, errors.New("maintenance history is full; preserve and inspect failed or uncertain jobs before adding more")
		}
	}
	now := time.Now().UTC()
	job := MaintenanceJob{ID: randomID(), HostID: host.ID, Harness: harness, Action: action, Status: "preparing", Stage: "Preparing maintenance", CreatedAt: now, UpdatedAt: now}
	s.records = append(records, maintenanceRecord{Job: job, HostTarget: host.Target, HostPort: host.Port, HostCreatedAt: host.CreatedAt, Phase: "accepted"})
	if err := s.saveLocked(); err != nil {
		s.records = old
		return MaintenanceJob{}, err
	}
	return job, nil
}

func boundedMaintenanceText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit-3]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}
