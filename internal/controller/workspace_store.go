package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxWorkspacePages    = 16
	maxWorkspaceHTML     = 128 << 10
	maxWorkspaceTasks    = 200
	maxWorkspaceBytes    = 4 << 20
	workspaceUnavailable = "Workspace state could not be verified. Preserve workspace.json before trying again; other Relay features remain available."
)

var errWorkspaceSize = errors.New("Workspace exceeds its 4 MiB storage limit")

type WorkspacePageSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
	Template  string    `json:"template,omitempty"`
}

type WorkspacePage struct {
	WorkspacePageSummary
	HTML     string `json:"html"`
	Revision uint64 `json:"revision"`
}

type WorkspaceTask struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	Status    string    `json:"status"`
	PageID    string    `json:"pageId,omitempty"`
	HostID    string    `json:"hostId,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type WorkspaceState struct {
	Revision  uint64                 `json:"revision"`
	UpdatedAt time.Time              `json:"updatedAt"`
	Pages     []WorkspacePageSummary `json:"pages"`
	Tasks     []WorkspaceTask        `json:"tasks"`
}

type WorkspaceMutation struct {
	Workspace WorkspaceState `json:"workspace"`
	Page      *WorkspacePage `json:"page,omitempty"`
	Task      *WorkspaceTask `json:"task,omitempty"`
}

type workspaceRecord struct {
	FrozenBy             string          `json:"frozenBy,omitempty"`
	Format               int             `json:"format"`
	Revision             uint64          `json:"revision"`
	UpdatedAt            time.Time       `json:"updatedAt"`
	Pages                []WorkspacePage `json:"pages"`
	Tasks                []WorkspaceTask `json:"tasks"`
	StatusTemplateSeeded bool            `json:"statusTemplateSeeded,omitempty"`
}

type workspaceStore struct {
	mu           sync.Mutex
	capabilityMu sync.Mutex
	path         string
	record       workspaceRecord
	err          error
}

func (s *Server) initializeWorkspace() error {
	w := &workspaceStore{path: filepath.Join(s.config.StateDir, "workspace.json"), record: workspaceRecord{Format: 1, UpdatedAt: time.Now().UTC(), Pages: []WorkspacePage{}, Tasks: []WorkspaceTask{}}}
	s.workspace = w
	data, err := readWorkspaceFile(w.path, maxWorkspaceBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = strictWorkspaceJSON(data, &w.record)
		if err == nil {
			err = validateWorkspaceRecord(w.record)
		}
	}
	if err != nil {
		w.err = errors.New(workspaceUnavailable)
	}
	return w.err
}

func readWorkspaceFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("workspace file must be a bounded regular file")
	}
	f, err := openWorkspaceRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("workspace file identity changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("workspace file exceeds limit")
	}
	return data, err
}

func strictWorkspaceJSON(data []byte, value any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON document")
	}
	return nil
}

func validWorkspaceTitle(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= 160 && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validWorkspaceHTML(value string) bool {
	return len(value) <= maxWorkspaceHTML && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validWorkspaceTask(task WorkspaceTask) bool {
	if !validMastermindID(task.ID) || !validWorkspaceTitle(task.Title) || len(task.Detail) > 4096 || !utf8.ValidString(task.Detail) || strings.ContainsRune(task.Detail, 0) {
		return false
	}
	switch task.Status {
	case "todo", "doing", "blocked", "done":
	default:
		return false
	}
	if task.PageID != "" && !validMastermindID(task.PageID) {
		return false
	}
	if len(task.HostID) > 128 || !utf8.ValidString(task.HostID) || strings.ContainsAny(task.HostID, "/\\") || strings.IndexFunc(task.HostID, unicode.IsControl) >= 0 {
		return false
	}
	return task.SessionID == "" || task.HostID != "" && validMastermindID(task.SessionID)
}

func validateWorkspaceRecord(record workspaceRecord) error {
	if record.FrozenBy != "" && !validMastermindID(record.FrozenBy) {
		return errors.New("invalid workspace migration fence")
	}
	if record.Format != 1 || record.Revision > maxMastermindRevision || record.UpdatedAt.IsZero() || len(record.Pages) > maxWorkspacePages || len(record.Tasks) > maxWorkspaceTasks {
		return errors.New("invalid workspace state")
	}
	pages, tasks := map[string]bool{}, map[string]bool{}
	statusPages := 0
	for _, page := range record.Pages {
		if !validMastermindID(page.ID) || pages[page.ID] || !validWorkspaceTitle(page.Title) || !validWorkspaceHTML(page.HTML) || page.UpdatedAt.IsZero() || page.Revision > record.Revision {
			return errors.New("invalid workspace page")
		}
		pages[page.ID] = true
		if page.Template != "" {
			if page.Template != statusTemplate || !record.StatusTemplateSeeded {
				return errors.New("invalid workspace template ownership")
			}
			statusPages++
			if statusPages > 1 {
				return errors.New("duplicate workspace status template")
			}
		}
	}
	for _, task := range record.Tasks {
		if !validWorkspaceTask(task) || tasks[task.ID] || task.UpdatedAt.IsZero() || task.PageID != "" && !pages[task.PageID] {
			return errors.New("invalid workspace task")
		}
		tasks[task.ID] = true
	}
	if record.Revision == 0 && (len(record.Pages) != 0 || len(record.Tasks) != 0) {
		return errors.New("uncommitted workspace state")
	}
	return nil
}

func cloneWorkspace(record workspaceRecord) workspaceRecord {
	record.Pages = append([]WorkspacePage{}, record.Pages...)
	record.Tasks = append([]WorkspaceTask{}, record.Tasks...)
	return record
}

func workspaceState(record workspaceRecord) WorkspaceState {
	state := WorkspaceState{Revision: record.Revision, UpdatedAt: record.UpdatedAt, Pages: []WorkspacePageSummary{}, Tasks: append([]WorkspaceTask{}, record.Tasks...)}
	for _, page := range record.Pages {
		state.Pages = append(state.Pages, page.WorkspacePageSummary)
	}
	return state
}

// The caller holds mu. Any uncertain disk commit blocks further writes until
// restart reloads the on-disk revision; clients must read their chosen ID rather
// than replay a create whose response was lost.
func (s *workspaceStore) saveLocked(record workspaceRecord) error {
	if err := validateWorkspaceRecord(record); err != nil {
		return err
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return err
	}
	if encoded.Len() > maxWorkspaceBytes {
		return errWorkspaceSize
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".workspace-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(encoded.Bytes()); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	if err = syncStoreDirectory(filepath.Dir(s.path)); err != nil {
		s.err = errors.New("Workspace commit could not be confirmed. Restart Relay to verify the saved revision before making another change.")
		return s.err
	}
	s.record = record
	return nil
}
