//go:build linux

package runtime

import (
	"errors"
	"time"
)

const controlLease = 60 * time.Second

type controlMessage struct {
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
	Type      string `json:"type"`
	Owner     bool   `json:"owner"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func (p *liveSession) controlStateLocked(sub *subscriber, reason string) controlMessage {
	return controlMessage{Cols: p.cols, Rows: p.rows, Type: "control", Owner: p.owner == sub && sub != nil, Available: p.owner == nil && !p.finished && !p.stopped, Reason: reason}
}
func enqueueControl(sub *subscriber, state controlMessage) {
	sub.session.enqueueFrameLocked(sub, terminalFrame{Control: &state})
}
func (p *liveSession) publishControlLocked(reason string) {
	for sub := range p.subscribers {
		enqueueControl(sub, p.controlStateLocked(sub, reason))
	}
}
func (p *liveSession) expireControlLocked(now time.Time) {
	if p.owner != nil && now.Sub(p.lastInput) >= controlLease {
		p.owner = nil
		p.publishControlLocked("Control released after 60 seconds without input")
	}
}
func (p *liveSession) expireControl() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireControlLocked(time.Now())
}
func (p *liveSession) claimControl(sub *subscriber) {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireControlLocked(time.Now())
	if _, attached := p.subscribers[sub]; !attached {
		enqueueControl(sub, p.controlStateLocked(sub, "Viewer is no longer attached"))
		return
	}
	if p.finished || p.stopped {
		enqueueControl(sub, p.controlStateLocked(sub, "Session ended"))
		return
	}
	if p.owner != nil && p.owner != sub {
		enqueueControl(sub, p.controlStateLocked(sub, "Another viewer has control"))
		return
	}
	p.owner = sub
	p.lastInput = time.Now()
	p.publishControlLocked("")
}
func (p *liveSession) releaseControl(sub *subscriber) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owner == sub {
		p.owner = nil
		p.publishControlLocked("")
	} else {
		enqueueControl(sub, p.controlStateLocked(sub, "You are watching"))
	}
}
func (p *liveSession) inputPermissionLocked(sub *subscriber) error {
	p.expireControlLocked(time.Now())
	if sub == nil {
		if p.owner != nil {
			return errors.New("another viewer has terminal control")
		}
		return nil
	}
	if p.owner != sub {
		return errors.New("take control before typing")
	}
	p.lastInput = time.Now()
	return nil
}
func (p *liveSession) resizeControlled(sub *subscriber, cols, rows int) error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireControlLocked(time.Now())
	if p.owner != sub {
		return errors.New("take control before resizing")
	}
	if cols < 2 || cols > 500 || rows < 2 || rows > 300 {
		return errors.New("invalid terminal size")
	}
	p.cols, p.rows = uint16(cols), uint16(rows)
	if p.screen != nil {
		p.screen.resize(cols, rows)
	}
	p.publishControlLocked("")
	if p.pty == nil || p.finished {
		return nil
	}
	return resizePTY(p.pty, p.cols, p.rows)
}
func (p *liveSession) controlError(sub *subscriber, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	enqueueControl(sub, p.controlStateLocked(sub, err.Error()))
}
