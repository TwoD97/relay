//go:build linux

package runtime

import (
	"io"
	"strings"
	"time"
)

func (p *liveSession) queueTerminalReplyLocked(data []byte) {
	if len(data) > 4096 {
		return
	}
	select {
	case p.replies <- data:
	default:
	}
}
func (p *liveSession) replyLoop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case data := <-p.replies:
			p.inputMu.Lock()
			p.mu.Lock()
			f := p.pty
			ready := !p.finished && !p.stopped && f != nil
			p.mu.Unlock()
			if ready {
				_ = f.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.Copy(f, strings.NewReader(string(data)))
			}
			p.inputMu.Unlock()
		}
	}
}
