//go:build linux

package runtime

import (
	"fmt"
	"image/color"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Browser owners answer terminal queries normally. With no owner, bounded
// server replies keep unattended programs from waiting for an absent terminal.
// Replies are queued while parsing, then written outside the screen lock.
func (m *terminalModel) registerQueries() {
	e := m.emulator
	reply := func(value string) {
		if m.respond && m.reply != nil {
			m.reply([]byte(value))
		}
	}
	e.RegisterCsiHandler('n', func(params ansi.Params) bool {
		n, _, _ := params.Param(0, 0)
		switch n {
		case 5:
			reply("\x1b[0n")
		case 6:
			p := e.CursorPosition()
			if m.modes[ansi.DECMode(6)] {
				p.Y -= m.marginTop
			}
			reply(fmt.Sprintf("\x1b[%d;%dR", p.Y+1, p.X+1))
		default:
			return false
		}
		return true
	})
	e.RegisterCsiHandler(ansi.Command('?', 0, 'n'), func(params ansi.Params) bool {
		n, _, _ := params.Param(0, 0)
		if n != 6 {
			return false
		}
		p := e.CursorPosition()
		if m.modes[ansi.DECMode(6)] {
			p.Y -= m.marginTop
		}
		reply(fmt.Sprintf("\x1b[?%d;%d;1R", p.Y+1, p.X+1))
		return true
	})
	e.RegisterCsiHandler('c', func(params ansi.Params) bool {
		n, _, _ := params.Param(0, 0)
		if n != 0 {
			return false
		}
		reply(ansi.PrimaryDeviceAttributes(62, 1, 6, 22))
		return true
	})
	e.RegisterCsiHandler(ansi.Command('>', 0, 'c'), func(params ansi.Params) bool {
		n, _, _ := params.Param(0, 0)
		if n != 0 {
			return false
		}
		reply(ansi.SecondaryDeviceAttributes(1, 10, 0))
		return true
	})
	e.RegisterCsiHandler('t', func(params ansi.Params) bool {
		n, _, _ := params.Param(0, 0)
		if n != 18 {
			return false
		}
		reply(fmt.Sprintf("\x1b[8;%d;%dt", e.Height(), e.Width()))
		return true
	})
	for _, dec := range []bool{false, true} {
		command := ansi.Command(0, '$', 'p')
		if dec {
			command = ansi.Command('?', '$', 'p')
		}
		e.RegisterCsiHandler(command, func(params ansi.Params) bool {
			n, _, _ := params.Param(0, 0)
			var mode ansi.Mode = ansi.ANSIMode(n)
			if dec {
				mode = ansi.DECMode(n)
			}
			setting := ansi.ModeNotRecognized
			if enabled, ok := m.modes[mode]; ok {
				setting = ansi.ModeReset
				if enabled {
					setting = ansi.ModeSet
				}
			}
			reply(ansi.ReportMode(mode, setting))
			return true
		})
	}
	for _, code := range []int{10, 11, 12} {
		e.RegisterOscHandler(code, func(data []byte) bool {
			parts := strings.Split(string(data), ";")
			if len(parts) != 2 || parts[1] != "?" {
				return false
			}
			var c color.Color
			switch code {
			case 10:
				c = e.ForegroundColor()
			case 11:
				c = e.BackgroundColor()
			case 12:
				c = e.CursorColor()
			}
			if c != nil {
				rgb := ansi.XRGBColor{Color: c}
				reply(fmt.Sprintf("\x1b]%d;%s\x1b\\", code, rgb.String()))
			}
			return true
		})
	}
}
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
