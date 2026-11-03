package localagent

import (
	"errors"
	"github.com/gorilla/websocket"
	"net/http"
	"net/url"
	"time"
)

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
