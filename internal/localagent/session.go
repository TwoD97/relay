package localagent

import (
	"encoding/json"
	"errors"
	terminal "github.com/TwoD97/relay/internal/runtime"
	"io"
	"sync"
	"time"
)

type command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// Wait owns final process reaping. Callers keep the output reader active through
// Wait, then Close; Stop may run concurrently and only signals owned handles.
type process interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
	Resize(int, int) error
	Stop() error
	Wait() (int, error)
	Identity() json.RawMessage
}
type controlMessage struct {
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
	Type      string `json:"type"`
	Owner     bool   `json:"owner"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}
type frame struct {
	Data    []byte
	Control *controlMessage
	Ended   bool
}
type subscriber struct {
	initial controlMessage
	frames  chan frame
	done    chan struct{}
	closed  bool
}
type session struct {
	mu                                    sync.Mutex
	inputMu                               sync.Mutex
	meta                                  Session
	launchHash                            string
	evidence                              json.RawMessage
	process                               process
	history                               []byte
	dirty                                 bool
	screen                                *terminal.Screen
	cols, rows                            uint16
	owner                                 *subscriber
	lastInput                             time.Time
	subs                                  map[*subscriber]struct{}
	replies                               chan []byte
	done                                  chan struct{}
	stopped, finished, interrupted, ready bool
	stopOnce                              sync.Once
}

func newSession(meta Session) *session {
	return &session{meta: meta, cols: 100, rows: 30, subs: map[*subscriber]struct{}{}, replies: make(chan []byte, 16), done: make(chan struct{})}
}
func (p *session) snapshot() Session { p.mu.Lock(); defer p.mu.Unlock(); return p.meta }
func (p *session) appendLocked(data []byte) {
	if len(data) == 0 {
		return
	}
	if len(data) >= maxHistory {
		p.history = append(p.history[:0], data[len(data)-maxHistory:]...)
	} else {
		excess := len(p.history) + len(data) - maxHistory
		if excess > 0 {
			copy(p.history, p.history[excess:])
			p.history = p.history[:len(p.history)-excess]
		}
		p.history = append(p.history, data...)
	}
	p.dirty = true
}
func (p *session) write(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendLocked(data)
	if p.screen == nil {
		p.screen = terminal.NewScreen(int(p.cols), int(p.rows))
	}
	p.screen.Write(data, p.owner == nil, func(b []byte) {
		if len(b) <= 4096 {
			select {
			case p.replies <- append([]byte(nil), b...):
			default:
			}
		}
	})
	data = append([]byte(nil), data...)
	for sub := range p.subs {
		p.enqueueLocked(sub, frame{Data: data})
	}
}
func (p *session) stateLocked(sub *subscriber, reason string) controlMessage {
	return controlMessage{p.cols, p.rows, "control", p.owner == sub && sub != nil, p.owner == nil && !p.finished && !p.stopped, reason}
}
func (p *session) enqueueLocked(sub *subscriber, value frame) {
	if sub.closed {
		return
	}
	select {
	case sub.frames <- value:
	default:
		p.dropLocked(sub)
	}
}
func (p *session) publishLocked(reason string) {
	for sub := range p.subs {
		state := p.stateLocked(sub, reason)
		p.enqueueLocked(sub, frame{Control: &state})
	}
}
func (p *session) dropLocked(sub *subscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	delete(p.subs, sub)
	close(sub.done)
	if p.owner == sub {
		p.owner = nil
		p.publishLocked("Control holder disconnected")
	}
}
func (p *session) subscribe() ([]byte, *subscriber, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.subs) >= 8 {
		return nil, nil, errors.New("too many terminal viewers")
	}
	history := append([]byte(nil), p.history...)
	var err error
	if p.screen != nil {
		history, err = p.screen.Snapshot(history)
		if err != nil {
			return nil, nil, err
		}
	}
	sub := &subscriber{frames: make(chan frame, 32), done: make(chan struct{})}
	sub.initial = p.stateLocked(sub, "")
	if p.finished {
		sub.frames <- frame{Ended: true}
	} else {
		p.subs[sub] = struct{}{}
	}
	return history, sub, nil
}
func (p *session) unsubscribe(sub *subscriber) { p.mu.Lock(); defer p.mu.Unlock(); p.dropLocked(sub) }
func (p *session) expireLocked() {
	if p.owner != nil && time.Since(p.lastInput) >= 60*time.Second {
		p.owner = nil
		p.publishLocked("Control released after 60 seconds without input")
	}
}
func (p *session) expireControl() { p.mu.Lock(); defer p.mu.Unlock(); p.expireLocked() }
func (p *session) claimControl(sub *subscriber) {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked()
	if sub.closed || p.finished || p.stopped {
		return
	}
	if p.owner != nil && p.owner != sub {
		state := p.stateLocked(sub, "Another viewer has control")
		p.enqueueLocked(sub, frame{Control: &state})
		return
	}
	p.owner = sub
	p.lastInput = time.Now()
	p.publishLocked("")
}
func (p *session) releaseControl(sub *subscriber) {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner == sub {
		p.owner = nil
		p.publishLocked("")
	}
}
func (p *session) controlError(sub *subscriber, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.stateLocked(sub, err.Error())
	p.enqueueLocked(sub, frame{Control: &state})
}
func (p *session) input(sub *subscriber, data string) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > 64<<10 {
		return errors.New("input exceeds 64 KiB")
	}
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	p.expireLocked()
	if p.owner != sub {
		p.mu.Unlock()
		return errors.New("another viewer has terminal control; take control before typing")
	}
	proc := p.process
	ready := p.ready && !p.finished && !p.stopped && proc != nil
	if sub != nil {
		p.lastInput = time.Now()
	}
	p.mu.Unlock()
	if !ready {
		return errors.New("session is not accepting input")
	}
	if err := proc.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	_, err := io.WriteString(proc, data)
	return err
}
func (p *session) writeInputControlled(sub *subscriber, data string) error { return p.input(sub, data) }
func (p *session) resizeControlled(sub *subscriber, cols, rows int) error {
	if cols < 2 || cols > 500 || rows < 2 || rows > 300 {
		return errors.New("invalid terminal size")
	}
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked()
	if p.owner != sub || sub == nil {
		return errors.New("take control before resizing")
	}
	if p.finished || p.stopped {
		return errors.New("session ended")
	}
	if p.process != nil {
		if err := p.process.Resize(cols, rows); err != nil {
			return err
		}
	}
	p.cols = uint16(cols)
	p.rows = uint16(rows)
	if p.screen != nil {
		p.screen.Resize(cols, rows)
	}
	p.publishLocked("")
	return nil
}
func (p *session) stop(interrupted bool) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		if p.finished {
			p.mu.Unlock()
			return
		}
		p.stopped = true
		p.interrupted = interrupted
		proc := p.process
		p.mu.Unlock()
		if proc != nil {
			_ = proc.Stop()
		}
		<-p.done
	})
}
func (p *session) replyLoop(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case b := <-p.replies:
			p.inputMu.Lock()
			p.mu.Lock()
			proc := p.process
			ready := p.ready && !p.finished && !p.stopped && proc != nil
			p.mu.Unlock()
			if ready {
				_ = proc.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = proc.Write(b)
			}
			p.inputMu.Unlock()
		}
	}
}
