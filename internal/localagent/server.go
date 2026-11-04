// Package localagent owns the desktop's independently-lived local coordinator.
// Its handler is served only on a current-user private IPC endpoint.
package localagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TwoD97/relay/internal/viewrequests"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxHistory = 1 << 20
const maxSessions = 32
const Protocol = 1

type Health struct {
	Version      string          `json:"version"`
	Protocol     int             `json:"protocol"`
	PID          int             `json:"pid"`
	Home         string          `json:"home"`
	Hostname     string          `json:"hostname"`
	Platform     string          `json:"platform"`
	Capabilities map[string]bool `json:"capabilities"`
}
type Session struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Workspace    string            `json:"workspace"`
	Cwd          string            `json:"cwd"`
	Harness      string            `json:"harness"`
	AgentMode    string            `json:"agentMode,omitempty"`
	MastermindID string            `json:"mastermindId,omitempty"`
	Purpose      string            `json:"purpose,omitempty"`
	Status       string            `json:"status"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	ExitCode     *int              `json:"exitCode,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}
type storedSession struct {
	Session
	LaunchHash string          `json:"launchHash,omitempty"`
	Process    json.RawMessage `json:"process,omitempty"`
}
type Server struct {
	views                                         *viewrequests.Queue
	harnessMu                                     sync.Mutex
	harnessCache                                  []Harness
	harnessCacheAt                                time.Time
	mu                                            sync.Mutex
	stateDir, version, home, hostname, executable string
	sessions                                      map[string]*session
	closed                                        bool
	cancel                                        context.CancelFunc
	flushed                                       chan struct{}
	// Tests replace only discovery, never expose an executable override in HTTP.
	findHarness func(string) (string, error)
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
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	s := &Server{stateDir: dir, version: version, home: home, hostname: hostname, executable: exe, sessions: map[string]*session{}, flushed: make(chan struct{})}
	s.findHarness = s.discover
	if err = s.load(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		defer close(s.flushed)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.mu.Lock()
				_ = s.persistLocked()
				s.mu.Unlock()
			}
		}
	}()
	return s, nil
}
func (s *Server) Health() Health {
	return Health{Version: s.version, Protocol: Protocol, PID: os.Getpid(), Home: s.home, Hostname: s.hostname, Platform: runtime.GOOS, Capabilities: map[string]bool{"mastermindV1": true, "localMastermindV1": true, "viewRequestsV1": true, "sessionStopV1": true}}
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	s.registerViews(m)
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, s.Health()) })
	m.HandleFunc("GET /api/sessions", s.list)
	m.HandleFunc("POST /api/sessions", s.create)
	m.HandleFunc("PATCH /api/sessions/{id}", s.patchSession)
	m.HandleFunc("DELETE /api/sessions/{id}", s.remove)
	m.HandleFunc("POST /api/sessions/{id}/stop", s.stopSession)
	m.HandleFunc("GET /api/sessions/{id}/history", s.history)
	m.HandleFunc("POST /api/sessions/{id}/input", s.input)
	m.HandleFunc("GET /api/sessions/{id}/terminal", s.terminal)
	m.HandleFunc("GET /api/harnesses", s.harnesses)
	m.HandleFunc("POST /api/harnesses/{id}/{action}", s.harnessAction)
	m.HandleFunc("GET /api/directories", s.directories)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		m.ServeHTTP(w, r)
	})
}
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	all := make([]*session, 0, len(s.sessions))
	for _, p := range s.sessions {
		all = append(all, p)
	}
	s.mu.Unlock()
	s.cancel()
	<-s.flushed
	for _, p := range all {
		p.stop(true)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}
func validID(v string) bool {
	if len(v) != 32 || v != strings.ToLower(v) {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
func id() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func validLabel(v string, max int) bool {
	return v != "" && len(v) <= max && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}
func validHarness(v string) bool { return v == "claude" || v == "codex" }
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	jsonResponse(w, status, map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func (s *Server) lookup(id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.sessions[id]
	if p == nil {
		return nil, os.ErrNotExist
	}
	return p, nil
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	all := make([]Session, 0, len(s.sessions))
	for _, p := range s.sessions {
		all = append(all, p.snapshot())
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })
	jsonResponse(w, 200, all)
}
func (s *Server) load() error {
	data, err := readPrivate(filepath.Join(s.stateDir, "sessions.json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored struct {
		Protocol int             `json:"protocol"`
		Sessions []storedSession `json:"sessions"`
	}
	if json.Unmarshal(data, &stored) != nil || stored.Protocol != Protocol || len(stored.Sessions) > maxSessions {
		return errors.New("invalid local agent state; preserve sessions.json for recovery")
	}
	for _, meta := range stored.Sessions {
		if !validID(meta.ID) || s.sessions[meta.ID] != nil || !validHarness(meta.Harness) || !validLabel(meta.Title, 120) || !validLabel(meta.Workspace, 80) || !filepath.IsAbs(meta.Cwd) || (meta.MastermindID != "" && !validID(meta.MastermindID)) {
			return errors.New("invalid local agent session identity")
		}
		if meta.Status != "running" && meta.Status != "exited" && meta.Status != "interrupted" {
			return errors.New("invalid local agent session status")
		}
		p := newSession(meta.Session)
		p.launchHash = meta.LaunchHash
		p.evidence = meta.Process
		p.finished = true
		close(p.done)
		if meta.Purpose != "login" {
			history, e := readPrivate(filepath.Join(s.stateDir, "history", meta.ID), maxHistory)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			p.appendLocked(history)
		}
		if meta.Status == "running" {
			p.meta.Status = "interrupted"
			p.meta.ExitCode = nil
			p.meta.UpdatedAt = time.Now().UTC()
			p.appendLocked([]byte("\r\nRelay: Local agent service stopped. " + recoverProcess(meta.Process) + " No task or input was replayed.\r\n"))
		}
		s.sessions[meta.ID] = p
	}
	return s.persistLocked()
}
func (s *Server) persistLocked() error {
	all := make([]storedSession, 0, len(s.sessions))
	for _, p := range s.sessions {
		p.mu.Lock()
		all = append(all, storedSession{p.meta, p.launchHash, p.evidence})
		if p.meta.Purpose != "login" && p.dirty {
			if err := atomicWrite(filepath.Join(s.stateDir, "history", p.meta.ID), p.history); err != nil {
				p.mu.Unlock()
				return err
			}
			p.dirty = false
		}
		p.mu.Unlock()
	}
	data, err := json.Marshal(struct {
		Protocol int             `json:"protocol"`
		Sessions []storedSession `json:"sessions"`
	}{Protocol, all})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.stateDir, "sessions.json"), data)
}

type CreateRequest struct {
	Title        string            `json:"title"`
	Workspace    string            `json:"workspace"`
	Cwd          string            `json:"cwd"`
	Harness      string            `json:"harness"`
	AgentMode    string            `json:"agentMode"`
	MastermindID string            `json:"mastermindId"`
	Cols         int               `json:"cols"`
	Rows         int               `json:"rows"`
	Prompt       string            `json:"prompt"`
	Env          map[string]string `json:"env"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var q CreateRequest
	if err := decode(w, r, &q); err != nil {
		fail(w, 400, err)
		return
	}
	if !validHarness(q.Harness) || !validID(q.MastermindID) {
		fail(w, 400, errors.New("local sessions require a Claude Code or Codex Mastermind identity"))
		return
	}
	if q.AgentMode == "" {
		q.AgentMode = "manual"
	}
	if q.AgentMode != "manual" && q.AgentMode != "auto" {
		fail(w, 400, errors.New("agentMode must be manual or auto"))
		return
	}
	if q.Title == "" {
		q.Title = "Mastermind"
	}
	if q.Workspace == "" {
		q.Workspace = "Mastermind"
	}
	if !validLabel(q.Title, 120) || !validLabel(q.Workspace, 80) || len(q.Prompt) > 32<<10 || !utf8.ValidString(q.Prompt) || strings.ContainsRune(q.Prompt, 0) {
		fail(w, 400, errors.New("invalid local coordinator labels or instruction"))
		return
	}
	for key, value := range q.Env {
		if (key != "RELAY_CONTROLLER_STATE" && key != "RELAY_AGENT_CAPABILITY_FILE" && key != "RELAY_BIN") || !filepath.IsAbs(value) || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			fail(w, 400, errors.New("unsupported coordinator environment"))
			return
		}
	}
	cwd, err := s.resolveCwd(q.Cwd)
	if err != nil {
		fail(w, 400, err)
		return
	}
	q.Cwd = cwd
	body, _ := json.Marshal(q)
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	s.mu.Lock()
	for _, p := range s.sessions {
		if p.meta.MastermindID == q.MastermindID {
			same := p.launchHash == hash
			meta := p.snapshot()
			s.mu.Unlock()
			if !same {
				fail(w, 409, errors.New("Mastermind identity already belongs to another launch"))
			} else {
				jsonResponse(w, 200, meta)
			}
			return
		}
	}
	s.mu.Unlock()
	path, err := s.findHarness(q.Harness)
	if err != nil {
		fail(w, 409, errors.New("Install this local harness before starting Mastermind"))
		return
	}
	if err = s.validateProvider(r.Context(), q.Harness, path, q.AgentMode); err != nil {
		fail(w, 422, err)
		return
	}
	if r.Context().Err() != nil {
		fail(w, 408, errors.New("launch preparation cancelled; no process started"))
		return
	}
	prompt := q.Prompt
	if prompt == "" {
		prompt = "You are Relay Mastermind, the user's local coordinator. Use the installed Relay Mastermind skill. Wait for the user's task, discover actual fleet identities, and preserve other sessions and credentials."
	}
	args := providerArguments(q.Harness, q.AgentMode, prompt)
	env := s.providerEnvironment(q.Harness, path, q.Env)
	env = setEnv(env, "RELAY_AGENT_MODE", q.AgentMode)
	meta := Session{Title: q.Title, Workspace: q.Workspace, Cwd: cwd, Harness: q.Harness, AgentMode: q.AgentMode, MastermindID: q.MastermindID, Permissions: map[string]string{"support": "terminal-only", "detail": "Local provider permission prompts remain in the terminal."}}
	result, err := s.start(meta, hash, q.Cols, q.Rows, command{Path: path, Args: args, Dir: cwd, Env: env})
	if err != nil {
		fail(w, 409, err)
		return
	}
	jsonResponse(w, 201, result)
}
func (s *Server) start(meta Session, hash string, cols, rows int, cmd command) (Session, error) {
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	if cols < 2 || cols > 500 || rows < 2 || rows > 300 {
		return Session{}, errors.New("invalid terminal size")
	}
	now := time.Now().UTC()
	meta.ID = id()
	meta.CreatedAt = now
	meta.UpdatedAt = now
	meta.Status = "running"
	p := newSession(meta)
	p.launchHash = hash
	p.cols = uint16(cols)
	p.rows = uint16(rows)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Session{}, errors.New("local agent service is stopping")
	}
	if len(s.sessions) >= maxSessions {
		return Session{}, errors.New("local session history is full; remove a completed session")
	}
	for _, other := range s.sessions {
		m := other.snapshot()
		if meta.MastermindID != "" && m.MastermindID == meta.MastermindID {
			return Session{}, errors.New("Mastermind launch is already recorded")
		}
		if m.Harness == meta.Harness && m.Purpose != "" && m.Status == "running" {
			return Session{}, errors.New("this harness has a setup operation running")
		}
	}
	s.sessions[meta.ID] = p
	if err := s.persistLocked(); err != nil {
		delete(s.sessions, meta.ID)
		return Session{}, err
	}
	go s.run(p, cmd)
	return meta, nil
}
func (s *Server) run(p *session, cmd command) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		s.finish(p, 130, nil)
		return
	}
	proc, err := startProcess(cmd, int(p.cols), int(p.rows))
	if err != nil {
		p.mu.Unlock()
		s.finish(p, 127, err)
		return
	}
	p.process = proc
	p.evidence = proc.Identity()
	p.mu.Unlock()
	s.mu.Lock()
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		_ = proc.Stop()
	}
	p.mu.Lock()
	p.ready = err == nil && !p.stopped
	p.mu.Unlock()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			n, e := proc.Read(buf)
			if n > 0 {
				p.write(buf[:n])
			}
			if e != nil {
				return
			}
		}
	}()
	repliesDone := make(chan struct{})
	go p.replyLoop(repliesDone)
	code, waitErr := proc.Wait()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
	}
	_ = proc.Close()
	<-readDone
	close(repliesDone)
	s.finish(p, code, errors.Join(err, waitErr))
}
func (s *Server) finish(p *session, code int, err error) {
	if err != nil {
		p.write([]byte("\r\nRelay: " + err.Error() + "\r\n"))
	}
	p.mu.Lock()
	p.finished = true
	p.ready = false
	p.meta.Status = "exited"
	p.meta.ExitCode = &code
	p.meta.UpdatedAt = time.Now().UTC()
	if p.interrupted {
		p.meta.Status = "interrupted"
		p.meta.ExitCode = nil
	}
	if p.meta.Purpose == "login" {
		p.history = nil
		p.screen = nil
		p.dirty = false
	}
	p.owner = nil
	p.publishLocked("Session ended")
	for sub := range p.subs {
		p.enqueueLocked(sub, frame{Ended: true})
	}
	close(p.done)
	p.mu.Unlock()
	s.mu.Lock()
	_ = s.persistLocked()
	s.mu.Unlock()
}
func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	p.stop(false)
	s.mu.Lock()
	delete(s.sessions, p.meta.ID)
	err = s.persistLocked()
	if err != nil {
		s.sessions[p.meta.ID] = p
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	_ = os.Remove(filepath.Join(s.stateDir, "history", p.meta.ID))
	w.WriteHeader(204)
}
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	p.mu.Lock()
	data := append([]byte(nil), p.history...)
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Data string `json:"data"`
	}
	if err := decode(w, r, &q); err != nil {
		fail(w, 400, err)
		return
	}
	p, err := s.lookup(r.PathValue("id"))
	if err == nil {
		err = p.input(nil, q.Data)
	}
	if err != nil {
		fail(w, 409, err)
		return
	}
	w.WriteHeader(204)
}
