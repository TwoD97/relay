//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

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
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{"version": s.version, "protocol": 1, "home": s.home, "hostname": s.hostname})
}
func sortSessions(metas []Session) {
	sort.Slice(metas, func(i, j int) bool { return metas[i].CreatedAt.Before(metas[j].CreatedAt) })
}
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	metas := make([]Session, 0, len(s.sessions))
	for _, p := range s.sessions {
		metas = append(metas, p.snapshot())
	}
	s.mu.Unlock()
	sortSessions(metas)
	jsonResponse(w, http.StatusOK, metas)
}

type createRequest struct {
	Title     string `json:"title"`
	Workspace string `json:"workspace"`
	Cwd       string `json:"cwd"`
	Harness   string `json:"harness"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decode(w, r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	if req.Title == "" {
		req.Title = "Terminal"
	}
	if req.Workspace == "" {
		req.Workspace = "Default"
	}
	if req.Harness == "" {
		req.Harness = "shell"
	}
	if err := validateLabel(req.Title, 120); err != nil {
		fail(w, 400, err)
		return
	}
	if err := validateLabel(req.Workspace, 80); err != nil {
		fail(w, 400, err)
		return
	}
	if !validHarness(req.Harness) {
		fail(w, 400, errors.New("harness must be shell, claude, or codex"))
		return
	}
	cwd, err := s.resolveCwd(req.Cwd)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var path string
	var permissions *Permissions
	var args []string
	if req.Harness == "shell" {
		path = os.Getenv("SHELL")
		if !strings.HasPrefix(path, "/") {
			path = "/bin/bash"
		}
		if _, err = os.Stat(path); err != nil {
			path = "/bin/sh"
		}
		args = []string{"-i"}
	} else {
		path, err = s.findCommand(req.Harness)
		if err != nil {
			fail(w, 409, errors.New("harness is not installed; install it first"))
			return
		}
	}
	if req.Harness != "shell" {
		executable, exeErr := os.Executable()
		if exeErr != nil {
			fail(w, 500, exeErr)
			return
		}
		permissions = s.permissionCapability(r.Context(), req.Harness, path)
		args = permissionHarnessArguments(req.Harness, executable, permissions.Support != "terminal-only")
	}
	meta, err := s.start(Session{Title: req.Title, Workspace: req.Workspace, Cwd: cwd, Harness: req.Harness, Permissions: permissions}, req.Cols, req.Rows, func(_ context.Context, _ io.Writer) (*exec.Cmd, error) { return exec.Command(path, args...), nil })
	if err != nil {
		fail(w, 409, err)
		return
	}
	jsonResponse(w, 201, meta)
}
func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Title     *string `json:"title"`
		Workspace *string `json:"workspace"`
	}
	if err := decode(w, r, &patch); err != nil {
		fail(w, 400, err)
		return
	}
	if patch.Title != nil {
		if err := validateLabel(*patch.Title, 120); err != nil {
			fail(w, 400, err)
			return
		}
	}
	if patch.Workspace != nil {
		if err := validateLabel(*patch.Workspace, 80); err != nil {
			fail(w, 400, err)
			return
		}
	}
	s.mu.Lock()
	p, ok := s.sessions[r.PathValue("id")]
	if !ok {
		s.mu.Unlock()
		fail(w, 404, os.ErrNotExist)
		return
	}
	p.mu.Lock()
	previous := p.meta
	if patch.Title != nil {
		p.meta.Title = *patch.Title
	}
	if patch.Workspace != nil {
		p.meta.Workspace = *patch.Workspace
	}
	p.meta.UpdatedAt = time.Now().UTC()
	meta := p.meta
	p.mu.Unlock()
	err := s.persistLocked()
	if err != nil {
		p.mu.Lock()
		p.meta = previous
		p.mu.Unlock()
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	jsonResponse(w, 200, meta)
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.lookup(id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	p.stop(false)
	s.mu.Lock()
	delete(s.sessions, id)
	err = s.persistLocked()
	if err != nil {
		s.sessions[id] = p
	} else {
		err = os.Remove(s.historyPath(id))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if s.observer != nil {
		s.observer.removeSession(id)
	}
	w.WriteHeader(204)
}
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	p.mu.Lock()
	data := p.history.bytes()
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data string `json:"data"`
	}
	if err := decode(w, r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	if err = p.writeInput(req.Data); err != nil {
		fail(w, 409, err)
		return
	}
	w.WriteHeader(204)
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

var upgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 32 << 10, CheckOrigin: sameOrigin, HandshakeTimeout: 10 * time.Second}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	history, sub, err := p.subscribe()
	if err != nil {
		fail(w, 429, err)
		return
	}
	defer p.unsubscribe(sub)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(96 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var message struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if err := conn.ReadJSON(&message); err != nil {
				return
			}
			var inputErr error
			switch message.Type {
			case "claim":
				p.claimControl(sub)
			case "release":
				p.releaseControl(sub)
			case "input":
				inputErr = p.writeInputControlled(sub, message.Data)
			case "resize":
				inputErr = p.resizeControlled(sub, message.Cols, message.Rows)
			default:
				inputErr = errors.New("unsupported terminal message")
			}
			if inputErr != nil {
				p.controlError(sub, inputErr)
			}
		}
	}()
	write := func(data []byte) error {
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.BinaryMessage, data)
	}
	if err = conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	if err = conn.WriteJSON(sub.initial); err != nil {
		return
	}
	if len(history) > 0 {
		if err = write(history); err != nil {
			return
		}
	}
	tick := time.NewTicker(20 * time.Second)
	leaseTick := time.NewTicker(5 * time.Second)
	defer leaseTick.Stop()
	defer tick.Stop()
	for {
		select {
		case <-leaseTick.C:
			p.expireControl()
		case <-readDone:
			return
		case <-sub.done:
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "viewer is too slow; reconnect to replay output"), time.Now().Add(time.Second))
			return
		case frame := <-sub.frames:
			if frame.Ended {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"), time.Now().Add(time.Second))
				return
			}
			if frame.Control != nil {
				if err = conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
					return
				}
				if err = conn.WriteJSON(frame.Control); err != nil {
					return
				}
			} else if err = write(frame.Data); err != nil {
				return
			}
		case <-tick.C:
			if err = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}
