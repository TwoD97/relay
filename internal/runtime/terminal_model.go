//go:build linux

package runtime

import (
	"fmt"
	"io"
	"sort"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ansiparser "github.com/charmbracelet/x/ansi/parser"
	"github.com/charmbracelet/x/vt"
)

// terminalModel is protected by liveSession.mu. Parse output once, independent
// of viewer count. Render only on attach or when saving the primary screen at
// an alternate-screen transition. History remains independently capped at 1 MiB.
type terminalModel struct {
	syntax                  *ansi.Parser
	pending                 []byte
	pendingOversized        bool
	reply                   func([]byte)
	respond                 bool
	emulator                *vt.Emulator
	modes                   map[ansi.Mode]bool
	pens                    [2]uv.Style
	savedPens               [2]uv.Style
	savedPositions          [2]uv.Position
	saved                   [2]bool
	mainScreen              string
	mainCursor              uv.Position
	cursorVisible           bool
	cursorStyle             int
	marginTop, marginBottom int
}

var replayDECModes = map[int]bool{1: true, 6: true, 7: true, 9: true, 25: true, 66: true, 69: true, 1000: true, 1001: true, 1002: true, 1003: true, 1004: true, 1005: true, 1006: true, 1015: true, 1016: true, 1047: true, 1048: true, 1049: true, 2004: true, 2026: true, 2027: true, 2048: true}

func newTerminalModel(cols, rows int) *terminalModel {
	e := vt.NewEmulator(cols, rows)
	// This emulator only observes output. Closing its response pipe prevents DSR,
	// DA and color queries from blocking Write or generating duplicate PTY input.
	if closer, ok := e.InputPipe().(io.Closer); ok {
		_ = closer.Close()
	}
	e.SetScrollbackSize(1)
	syntax := ansi.NewParser()
	syntax.SetDataSize(1)
	m := &terminalModel{syntax: syntax, emulator: e, modes: make(map[ansi.Mode]bool), cursorVisible: true, cursorStyle: 1, marginBottom: rows}
	for n := range replayDECModes {
		m.modes[ansi.DECMode(n)] = false
	}
	m.modes[ansi.ANSIMode(4)] = false
	m.modes[ansi.ANSIMode(20)] = false
	m.modes[ansi.DECMode(7)] = true
	m.modes[ansi.DECMode(25)] = true
	e.SetCallbacks(vt.Callbacks{
		EnableMode:       func(mode ansi.Mode) { m.modes[mode] = true },
		DisableMode:      func(mode ansi.Mode) { m.modes[mode] = false },
		CursorVisibility: func(v bool) { m.cursorVisible = v },
		CursorStyle: func(style vt.CursorStyle, blink bool) {
			m.cursorStyle = int(style)*2 + 1
			if !blink {
				m.cursorStyle++
			}
		},
		AltScreen: func(on bool) {
			if on {
				m.pens[1] = m.pens[0]
			}
		},
	})
	e.RegisterOscHandler(8, func(data []byte) bool { return len(data) > 2048 })
	e.RegisterCsiHandler('m', func(params ansi.Params) bool { uv.ReadStyle(params, &m.pens[m.index()]); return false })
	// Bound mode-map cardinality: unknown vendor modes remain in the live stream,
	// but cannot grow the server's model without limit.
	for _, final := range []byte{'h', 'l'} {
		for _, dec := range []bool{false, true} {
			command := int(final)
			if dec {
				command = ansi.Command('?', 0, final)
			}
			e.RegisterCsiHandler(command, func(params ansi.Params) bool {
				for index, p := range params {
					n := p.Param(-1)
					if dec {
						if !replayDECModes[n] {
							params[index] = ansi.Param(-1)
						}
					} else if n != 4 && n != 20 {
						params[index] = ansi.Param(-1)
					}
				}
				if dec && final == 'h' {
					for _, p := range params {
						n := p.Param(-1)
						if (n == 1047 || n == 1049) && !e.IsAltScreen() {
							m.mainScreen = e.Render()
							m.mainCursor = e.CursorPosition()
							if n == 1049 {
								m.saveCursor()
							}
						}
					}
				}
				return false
			})
		}
	}
	e.RegisterEscHandler('7', func() bool { m.saveCursor(); return false })
	e.RegisterEscHandler('8', func() bool { m.pens[m.index()] = m.savedPens[m.index()]; return false })
	e.RegisterCsiHandler('s', func(ansi.Params) bool { m.saveCursor(); return false })
	e.RegisterCsiHandler('u', func(ansi.Params) bool { m.pens[m.index()] = m.savedPens[m.index()]; return false })
	e.RegisterCsiHandler('r', func(params ansi.Params) bool {
		top, bottom := 1, e.Height()
		if len(params) > 0 {
			top = params[0].Param(1)
		}
		if len(params) > 1 {
			bottom = params[1].Param(e.Height())
		}
		if top > 0 && bottom > top && bottom <= e.Height() {
			m.marginTop = top - 1
			m.marginBottom = bottom
		}
		return false
	})
	e.RegisterEscHandler('c', func() bool {
		m.cursorStyle = 1
		m.pens = [2]uv.Style{}
		m.saved = [2]bool{}
		m.mainScreen = ""
		m.modes = make(map[ansi.Mode]bool)
		m.marginTop = 0
		m.marginBottom = e.Height()
		return false
	})
	m.registerQueries()
	return m
}
func (m *terminalModel) index() int {
	if m.emulator.IsAltScreen() {
		return 1
	}
	return 0
}
func (m *terminalModel) saveCursor() {
	i := m.index()
	m.savedPens[i] = m.pens[i]
	m.savedPositions[i] = m.emulator.CursorPosition()
	m.saved[i] = true
}
func (m *terminalModel) write(data []byte) {
	for _, b := range data {
		old := m.syntax.State()
		action := m.syntax.Advance(b)
		state := m.syntax.State()
		if state == ansiparser.GroundState {
			m.pending = m.pending[:0]
			m.pendingOversized = false
			continue
		}
		if old == ansiparser.GroundState || (b == 0x1b && state == ansiparser.EscapeState) {
			m.pending = m.pending[:0]
			m.pendingOversized = false
		}
		if action == ansiparser.ExecuteAction {
			continue
		}
		if len(m.pending) < 64<<10 {
			m.pending = append(m.pending, b)
		} else {
			m.pendingOversized = true
		}
	}
	_, _ = m.emulator.Write(data)
}
func (m *terminalModel) resize(cols, rows int) {
	if m.emulator.IsAltScreen() {
		lines := strings.Split(m.mainScreen, "\n")
		if len(lines) > rows {
			lines = lines[:rows]
		}
		for i := range lines {
			lines[i] = ansi.Truncate(lines[i], cols, "")
		}
		m.mainScreen = strings.Join(lines, "\n")
	}
	m.emulator.Resize(cols, rows)
	m.marginTop = 0
	m.marginBottom = rows
}
func paintScreen(out *strings.Builder, screen string, rows int) {
	out.WriteString("\x1b[0m\x1b[?6l\x1b[?7l\x1b[?69l\x1b[r\x1b[2J")
	for row, line := range strings.Split(screen, "\n") {
		if row >= rows {
			break
		}
		fmt.Fprintf(out, "\x1b[%d;1H%s", row+1, line)
	}
}
func (m *terminalModel) snapshot(history []byte) []byte {
	e := m.emulator
	var out strings.Builder
	out.Grow(len(history) + e.Width()*e.Height()*2 + 2048)
	// CAN terminates a partial historical escape; RIS supplies a known baseline.
	// Replaying history first retains bounded scrollback. The authoritative screen
	// then repairs truncation, alternate-buffer state, styles and cursor position.
	out.WriteString("\x18\x1bc")
	out.Write(history)
	out.WriteString("\x18\x1b[?2026l\x1b[?1049l\x1b[?1047l")
	if e.IsAltScreen() {
		paintScreen(&out, m.mainScreen, e.Height())
		fmt.Fprintf(&out, "\x1b[%d;%dH", m.mainCursor.Y+1, m.mainCursor.X+1)
		out.WriteString(m.pens[0].String())
		out.WriteString("\x1b[?1049h")
	}
	paintScreen(&out, e.Render(), e.Height())
	i := m.index()
	if m.saved[i] {
		pos := m.savedPositions[i]
		fmt.Fprintf(&out, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
		out.WriteString(m.savedPens[i].String())
		out.WriteString("\x1b7")
	}
	// Restore deterministic mode ordering. Buffer/save-cursor modes were rebuilt
	// explicitly; sync-output must stay off or an observer could remain frozen.
	keys := make([]ansi.Mode, 0, len(m.modes))
	for mode := range m.modes {
		keys = append(keys, mode)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Mode() < keys[j].Mode() })
	for _, mode := range keys {
		if dec, ok := mode.(ansi.DECMode); ok && (dec == 1047 || dec == 1048 || dec == 1049 || dec == 2026 || dec == 6 || dec == 69) {
			continue
		}
		if m.modes[mode] {
			out.WriteString(ansi.SetMode(mode))
		} else {
			out.WriteString(ansi.ResetMode(mode))
		}
	}
	fmt.Fprintf(&out, "\x1b[%d;%dr", m.marginTop+1, m.marginBottom)
	position := e.CursorPosition()
	if m.modes[ansi.DECMode(6)] {
		out.WriteString("\x1b[?6h")
		position.Y -= m.marginTop
	}
	fmt.Fprintf(&out, "\x1b[%d;%dH", position.Y+1, position.X+1)
	out.WriteString(m.pens[i].String())
	fmt.Fprintf(&out, "\x1b[%d q", m.cursorStyle)
	if m.cursorVisible {
		out.WriteString("\x1b[?25h")
	} else {
		out.WriteString("\x1b[?25l")
	}
	out.Write(m.pending)
	return []byte(out.String())
}
