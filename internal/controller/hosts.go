package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TwoD97/relay/internal/transport"
	"github.com/gorilla/websocket"
)

func (s *Server) addHost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Target string `json:"target"`
		Port   int    `json:"port"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Target = strings.TrimSpace(body.Target)
	if body.Name == "" {
		body.Name = body.Target
	}
	if len(body.Name) > 80 || strings.IndexFunc(body.Name, unicode.IsControl) >= 0 {
		writeError(w, 400, "Name must contain 1–80 printable characters")
		return
	}
	if err := transport.ValidateTarget(body.Target); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := transport.ValidatePort(body.Port); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	h := Host{ID: randomID()[:16], Name: body.Name, Target: body.Target, Port: body.Port, Status: "disconnected", Stage: "Ready to connect", CreatedAt: time.Now().UTC()}
	if err := s.store.add(h); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	if err := s.beginConnect(h); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h, _ = s.store.get(h.ID)
	writeJSON(w, 201, h)
}

func (s *Server) connectHost(w http.ResponseWriter, r *http.Request) {
	h, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, 404, "Host not found")
		return
	}
	if err := s.beginConnect(h); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	h, _ = s.store.get(h.ID)
	writeJSON(w, 202, h)
}

func (s *Server) beginConnect(h Host) error {
	_, err := s.queueConnection(h)
	return err
}

func (s *Server) queueConnection(h Host) (bool, error) {
	s.mu.Lock()
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return false, err
	}
	latest, exists := s.store.get(h.ID)
	if !exists {
		s.mu.Unlock()
		return false, errors.New("host was forgotten before the connection started")
	}
	h = latest
	if l := s.links[h.ID]; l != nil {
		s.mu.Unlock()
		return false, nil
	}
	ctx, cancel := context.WithCancel(s.ctx)
	l := &link{cancel: cancel}
	s.links[h.ID] = l
	s.store.update(h.ID, "connecting", "Waiting for another host setup", "")
	s.store.runtimeOperation(h.ID, nil)
	s.store.setupWarning(h.ID, "")
	s.connectWG.Add(1)
	s.mu.Unlock()
	go s.runConnection(ctx, h, l)
	return true, nil
}

// ReconnectSaved is intentionally idempotent and only attempts disconnected
// hosts once per launch. Authentication failures need user attention; there is
// no background loop that repeatedly prompts or retries credentials.
func (s *Server) ReconnectSaved() int {
	queued := 0
	for _, host := range s.store.list() {
		if started, err := s.queueConnection(host); err == nil && started {
			queued++
		}
	}
	return queued
}

func (s *Server) reconnectHosts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": s.ReconnectSaved()})
}

func (s *Server) runConnection(ctx context.Context, h Host, l *link) {
	defer s.connectWG.Done()
	select {
	case s.connectSlots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { <-s.connectSlots }) }
	defer release()
	if ctx.Err() != nil {
		return
	}
	s.connectionStage(h.ID, l, "connecting", "Log in through the SSH terminal")
	conn, err := s.startConnection(ctx, transport.Config{Target: h.Target, Port: h.Port})
	if err != nil {
		s.connectionFailed(h.ID, l, err)
		return
	}
	s.mu.Lock()
	if s.links[h.ID] != l {
		s.mu.Unlock()
		conn.Close()
		return
	}
	l.conn = conn
	s.mu.Unlock()
	func() {
		readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Minute)
		defer readyCancel()
		if err := conn.WaitReady(readyCtx); err != nil {
			s.connectionFailed(h.ID, l, err)
			return
		}
		s.connectionStage(h.ID, l, "installing", "Checking remote host")
		installCtx, installCancel := context.WithTimeout(ctx, 3*time.Minute)
		defer installCancel()
		if err := conn.Bootstrap(installCtx, s.config.BinaryDir, s.config.Version, func(stage string) { s.connectionStage(h.ID, l, "installing", stage) }); err != nil {
			s.connectionFailed(h.ID, l, err)
			return
		}
		var setupWarnings []string
		if err := conn.RefreshCLI(installCtx, s.config.BinaryDir, s.config.Version); err != nil {
			setupWarnings = append(setupWarnings, "Relay agent CLI update needs attention: "+err.Error())
		}
		if err := conn.InstallAgentSkill(installCtx); err != nil {
			setupWarnings = append(setupWarnings, err.Error())
		}
		s.connectionWarning(h.ID, l, strings.Join(setupWarnings, "; "))
		p := s.makeProxy(conn.DialContext)
		s.mu.Lock()
		if s.links[h.ID] != l {
			s.mu.Unlock()
			p.httpTransport.CloseIdleConnections()
			return
		}
		l.proxy = p.proxy
		l.httpTransport = p.httpTransport
		s.store.update(h.ID, "online", "Connected", "")
		s.mu.Unlock()
		release()
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			s.connectionFailed(h.ID, l, errors.New("SSH connection closed. Reconnect to reattach to your running sessions"))
		}
	}()
}

func (s *Server) connectionWarning(id string, l *link, warning string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links[id] == l {
		s.store.setupWarning(id, warning)
	}
}

func (s *Server) connectionFailed(id string, l *link, err error) {
	s.mu.Lock()
	current := s.links[id] == l
	if current {
		delete(s.links, id)
		s.store.update(id, "error", "Connection needs attention", err.Error())
	}
	s.mu.Unlock()
	if current {
		l.cancel()
		if l.conn != nil {
			l.conn.Close()
		}
		if l.httpTransport != nil {
			l.httpTransport.CloseIdleConnections()
		}
	}
}

func (s *Server) connectionStage(id string, l *link, status, stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links[id] == l {
		s.store.update(id, status, stage, "")
	}
}

func (s *Server) disconnect(id string) {
	s.mu.Lock()
	l := s.links[id]
	delete(s.links, id)
	s.store.update(id, "disconnected", "Remote sessions keep running", "")
	s.mu.Unlock()
	if l != nil {
		if l.cancel != nil {
			l.cancel()
		}
		if l.conn != nil {
			l.conn.Close()
		}
		if l.httpTransport != nil {
			l.httpTransport.CloseIdleConnections()
		}
	}
}

func (s *Server) disconnectHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.get(id); !ok {
		writeError(w, 404, "Host not found")
		return
	}
	s.disconnect(id)
	w.WriteHeader(204)
}
func (s *Server) forgetHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	if _, ok := s.store.get(id); !ok {
		s.mu.Unlock()
		writeError(w, 404, "Host not found")
		return
	}
	if err := s.store.remove(id); err != nil {
		s.mu.Unlock()
		writeError(w, 500, "Could not save host list")
		return
	}
	l := s.links[id]
	delete(s.links, id)
	s.mu.Unlock()
	if l != nil {
		if l.cancel != nil {
			l.cancel()
		}
		if l.conn != nil {
			l.conn.Close()
		}
		if l.httpTransport != nil {
			l.httpTransport.CloseIdleConnections()
		}
	}
	w.WriteHeader(204)
}

func (s *Server) setupTerminal(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	l := s.links[r.PathValue("id")]
	var conn *transport.Connection
	if l != nil {
		conn = l.conn
	}
	s.mu.Unlock()
	if conn == nil {
		writeError(w, 409, "SSH login is not running")
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: sameOrigin, ReadBufferSize: 4096, WriteBufferSize: 4096}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(64 << 10)
	history, output, unsubscribe := conn.Subscribe()
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ws.Close()
		for {
			var msg struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if err := ws.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "input":
				if _, err := conn.Write([]byte(msg.Data)); err != nil {
					return
				}
			case "resize":
				if msg.Cols >= 10 && msg.Cols <= 500 && msg.Rows >= 2 && msg.Rows <= 250 {
					_ = conn.Resize(msg.Cols, msg.Rows)
				}
			}
		}
	}()
	write := func(data []byte) error {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteMessage(websocket.BinaryMessage, data)
	}
	if len(history) > 0 {
		if err := write(history); err != nil {
			return
		}
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case data, ok := <-output:
			if !ok {
				return
			}
			if err := write(data); err != nil {
				return
			}
		case <-done:
			return
		case <-s.ctx.Done():
			return
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}
