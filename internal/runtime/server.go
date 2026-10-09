//go:build linux

// Package runtime owns durable session metadata and live PTYs. Its HTTP handler
// must be exposed only through the private Unix socket or an authenticated proxy.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxSessions    = 32
	maxHistory     = 1 << 20
	maxInput       = 64 << 10
	maxSubscribers = 8
)

type Session struct {
	Permissions     *Permissions     `json:"permissions,omitempty"`
	ProcessIdentity *ProcessIdentity `json:"processIdentity,omitempty"`
	Recovery        *SessionRecovery `json:"recovery,omitempty"`
	Purpose         string           `json:"purpose,omitempty"`
	Attention       *Attention       `json:"attention,omitempty"`
	ID              string           `json:"id"`
	Title           string           `json:"title"`
	Workspace       string           `json:"workspace"`
	Cwd             string           `json:"cwd"`
	Harness         string           `json:"harness"`
	Status          string           `json:"status"`
	CreatedAt       time.Time        `json:"createdAt"`
	UpdatedAt       time.Time        `json:"updatedAt"`
	ExitCode        *int             `json:"exitCode,omitempty"`
}

type Server struct {
	observer                          *observerManager
	approvalMu                        sync.Mutex
	approvals                         map[string]*approvalWaiter
	approvalTTL                       time.Duration
	metadataDirty                     bool
	storageError                      string
	mu                                sync.Mutex
	stateDir, version, home, hostname string
	sessions                          map[string]*liveSession
	installing                        map[string]bool
	closed                            bool
	cancel                            context.CancelFunc
	flushDone                         chan struct{}
	toolsMu                           chan struct{}
	harnessMu                         sync.Mutex
	harnessCache                      []Harness
	harnessCacheAt                    time.Time
}

type storedState struct {
	Protocol int       `json:"protocol"`
	Sessions []Session `json:"sessions"`
}

func New(stateDir, version string) (*Server, error) {
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	if err = privateDir(dir); err != nil {
		return nil, err
	}
	if err = privateDir(filepath.Join(dir, "history")); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	s := &Server{stateDir: dir, version: version, home: home, hostname: hostname, sessions: make(map[string]*liveSession), flushDone: make(chan struct{}), toolsMu: make(chan struct{}, 1)}
	if err = s.load(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.observer = newObserver(s)
	go s.observer.loop(ctx)
	go s.flushLoop(ctx)
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/observer", s.getObserver)
	mux.HandleFunc("POST /api/observer/config", s.configureObserver)
	mux.HandleFunc("POST /api/observer/refresh", s.refreshObserver)
	mux.HandleFunc("GET /api/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/approvals/{id}/decision", s.decideApproval)
	mux.HandleFunc("POST /api/sessions/{id}/approval-hook", s.approvalHook)
	mux.HandleFunc("POST /api/sessions/{id}/events", s.sessionEvent)
	mux.HandleFunc("POST /api/sessions/{id}/attention/ack", s.ackAttention)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.patchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	mux.HandleFunc("GET /api/sessions/{id}/terminal", s.terminal)
	mux.HandleFunc("GET /api/sessions/{id}/history", s.history)
	mux.HandleFunc("POST /api/sessions/{id}/input", s.input)
	mux.HandleFunc("GET /api/harnesses", s.harnesses)
	mux.HandleFunc("POST /api/harnesses/{id}/install", s.installHarness)
	mux.HandleFunc("POST /api/harnesses/{id}/update", s.updateHarness)
	mux.HandleFunc("POST /api/harnesses/{id}/repair", s.repairHarness)
	mux.HandleFunc("POST /api/harnesses/{id}/login", s.loginHarness)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sessions := make([]*liveSession, 0, len(s.sessions))
	for _, p := range s.sessions {
		sessions = append(sessions, p)
	}
	s.mu.Unlock()
	s.cancel()
	if s.observer != nil {
		<-s.observer.done
	}
	var wg sync.WaitGroup
	for _, p := range sessions {
		wg.Add(1)
		go func(p *liveSession) { defer wg.Done(); p.stop(true) }(p)
	}
	wg.Wait()
	<-s.flushDone
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.persistLocked(), s.flushHistoryLocked())
}

func (s *Server) load() error {
	bytes, err := readPrivateFile(filepath.Join(s.stateDir, "sessions.json"), 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session state: %w", err)
	}
	var stored storedState
	if err = json.Unmarshal(bytes, &stored); err != nil || stored.Protocol != 1 {
		return errors.New("invalid or unsupported session state; preserve sessions.json and restore a valid backup")
	}
	if len(stored.Sessions) > maxSessions {
		return errors.New("saved session limit exceeded")
	}
	for _, meta := range stored.Sessions {
		if !validID(meta.ID) || s.sessions[meta.ID] != nil || validateLabel(meta.Title, 120) != nil || validateLabel(meta.Workspace, 80) != nil || !validHarness(meta.Harness) {
			return errors.New("invalid saved session metadata")
		}
		if meta.Status != "running" && meta.Status != "exited" && meta.Status != "interrupted" {
			return errors.New("invalid saved session status")
		}
		recovered := meta.Status == "running"
		if recovered {
			meta.Status = "interrupted"
			meta.UpdatedAt = time.Now().UTC()
			meta.ExitCode = nil
			meta.Recovery = recoverInterruptedProcess(meta.ProcessIdentity)
		}
		p := newLiveSession(meta)
		p.finished = true
		close(p.done)
		var data []byte
		var readErr error
		if meta.Purpose != "login" {
			data, readErr = readPrivateFile(s.historyPath(meta.ID), maxHistory)
		} else {
			_ = os.Remove(s.historyPath(meta.ID))
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("read session history: %w", readErr)
		}
		p.history.append(data)
		if recovered && meta.Recovery != nil && meta.Purpose != "login" {
			p.history.append([]byte("\r\nRelay: " + meta.Recovery.Detail + "\r\n"))
			p.dirty = true
		}
		s.sessions[meta.ID] = p
	}
	return s.persistLocked()
}

func (s *Server) persistLocked() (result error) {
	defer func() {
		s.metadataDirty = result != nil
		if result != nil && result.Error() != s.storageError {
			log.Printf("runtime session metadata persistence failed: %v", result)
			s.storageError = result.Error()
		} else if result == nil {
			s.storageError = ""
		}
	}()
	metas := make([]Session, 0, len(s.sessions))
	for _, p := range s.sessions {
		metas = append(metas, p.snapshot())
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].CreatedAt.Before(metas[j].CreatedAt) })
	data, err := json.Marshal(storedState{Protocol: 1, Sessions: metas})
	if err != nil {
		return err
	}
	return atomicPrivateWrite(filepath.Join(s.stateDir, "sessions.json"), data)
}

func (s *Server) historyPath(id string) string {
	return filepath.Join(s.stateDir, "history", id+".bin")
}
func (s *Server) flushHistoryLocked() error {
	var errs []error
	for id, p := range s.sessions {
		p.mu.Lock()
		if p.meta.Purpose == "login" {
			p.dirty = false
			p.mu.Unlock()
			continue
		}
		if !p.dirty {
			p.mu.Unlock()
			continue
		}
		data := p.history.bytes()
		p.dirty = false
		p.mu.Unlock()
		if err := atomicPrivateWrite(s.historyPath(id), data); err != nil {
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (s *Server) flushLoop(ctx context.Context) {
	defer close(s.flushDone)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			if s.metadataDirty {
				_ = s.persistLocked()
			}
			if err := s.flushHistoryLocked(); err != nil {
				log.Printf("runtime history persistence failed: %v", err)
			}
			s.mu.Unlock()
		}
	}
}
func (s *Server) lookup(id string) (*liveSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.sessions[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return p, nil
}
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func validHarness(h string) bool { return h == "shell" || h == "claude" || h == "codex" }
func validateLabel(v string, max int) error {
	if len(v) == 0 || len(v) > max || !utf8.ValidString(v) {
		return errors.New("label is empty, invalid UTF-8, or too long")
	}
	for _, c := range v {
		if unicode.IsControl(c) {
			return errors.New("labels cannot contain control characters")
		}
	}
	return nil
}
func (s *Server) resolveCwd(value string) (string, error) {
	if value == "" || value == "~" {
		value = s.home
	} else if strings.HasPrefix(value, "~/") {
		value = filepath.Join(s.home, value[2:])
	}
	if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") || !filepath.IsAbs(value) {
		return "", errors.New("working directory must be an absolute path or ~/path")
	}
	path, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("working directory is not a directory")
	}
	return path, nil
}
