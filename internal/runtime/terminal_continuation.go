//go:build linux

package runtime

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	ansiparser "github.com/charmbracelet/x/ansi/parser"
)

// These fields supplement the public VT screen API for continued output after
// a snapshot. They cover delayed autowrap and the supported DEC character-set
// designations/shifts; they are not a serialization of every vendor extension.
func (m *terminalModel) writeText(printed []byte) {
	columns := m.emulator.Width()
	autowrap := m.modes[ansi.DECMode(7)]
	for len(printed) > 0 {
		if joined := m.joinPreviousGrapheme(printed); joined > 0 {
			printed = printed[joined:]
			continue
		}
		cluster, width := ansi.FirstGraphemeCluster(printed, ansi.GraphemeWidth)
		if len(cluster) == 0 {
			break
		}
		if m.wrapPending && autowrap {
			_, _ = m.emulator.Write([]byte("\r\n"))
		}
		x := m.emulator.CursorPosition().X
		// The pinned VT bases phantom state on a glyph's starting column,
		// unlike xterm's ending column. Handle the wide-glyph boundary here;
		// all actual scrolling remains the emulator's responsibility.
		if autowrap && width > 1 && x+width > columns {
			_, _ = m.emulator.Write([]byte("\x1b[K\r\n"))
			x = m.emulator.CursorPosition().X
		}
		_, _ = m.emulator.Write(cluster)
		m.wrapPending = autowrap && x+width >= columns
		m.singleShift = 0
		printed = printed[len(cluster):]
	}
}

func (m *terminalModel) joinPreviousGrapheme(printed []byte) int {
	// Printable ASCII cannot extend the previous cluster. Avoid allocating or
	// rescanning the remaining output for the overwhelmingly common path.
	if len(printed) == 0 || printed[0] >= 0x20 && printed[0] < 0x7f {
		return 0
	}
	first, _ := ansi.FirstGraphemeCluster(printed, ansi.GraphemeWidth)
	position := m.emulator.CursorPosition()
	if !m.wrapPending {
		position.X--
	}
	for position.X >= 0 {
		cell := m.emulator.CellAt(position.X, position.Y)
		if cell == nil {
			return 0
		}
		if cell.Width == 0 {
			position.X--
			continue
		}
		if cell.Content == "" {
			return 0
		}
		if len(cell.Content)+len(first) > 64<<10 {
			return 0
		}
		combined := append([]byte(cell.Content), first...)
		cluster, width := ansi.FirstGraphemeCluster(combined, ansi.GraphemeWidth)
		if len(cluster) <= len(cell.Content) || width != cell.Width {
			return 0
		}
		// A PTY read boundary must not turn a combining mark or same-width
		// ZWJ continuation into a new cell/line. Keep the original cell style
		// and cursor; width-changing clusters remain the VT's responsibility.
		updated := *cell
		updated.Content = string(cluster)
		consumed := len(cluster) - len(cell.Content)
		m.emulator.SetCell(position.X, position.Y, &updated)
		return consumed
	}
	return 0
}

func (m *terminalModel) trackControl(old, action, b byte) {
	if action == ansiparser.ExecuteAction {
		switch b {
		case '\b', '\n', '\v', '\f', '\r', 0x84:
			m.wrapPending = false
		case 0x0e:
			m.gl = 1
		case 0x0f:
			m.gl = 0
		case 0x8e:
			m.singleShift = 2
		case 0x8f:
			m.singleShift = 3
		}
		return
	}
	if action != ansiparser.DispatchAction {
		return
	}
	cmd := ansi.Cmd(m.syntax.Command())
	if old == ansiparser.EscapeState || old == ansiparser.EscapeIntermediateState {
		if set := cmd.Intermediate(); set >= '(' && set <= '+' {
			if final := cmd.Final(); final == 'A' || final == 'B' || final == '0' {
				m.charsets[set-'('] = final
			}
			return
		}
		if cmd.Intermediate() != 0 {
			return
		}
		switch cmd.Final() {
		case 'D', 'E':
			m.wrapPending = false
		case '8':
			m.normalizeRestoredCursor()
		case 'n':
			m.gl = 2
		case 'o':
			m.gl = 3
		case '|':
			m.gr = 3
		case '}':
			m.gr = 2
		case '~':
			m.gr = 1
		case 'N':
			m.singleShift = 2
		case 'O':
			m.singleShift = 3
		}
		return
	}
	if old != ansiparser.CsiEntryState && old != ansiparser.CsiParamState && old != ansiparser.CsiIntermediateState {
		return
	}
	if cmd.Prefix() == 0 && cmd.Intermediate() == 0 && strings.ContainsRune("ABCDEFGH`adefX", rune(cmd.Final())) {
		m.wrapPending = false
	}
	if cmd.Prefix() == 0 && cmd.Intermediate() == 0 && cmd.Final() == 'u' {
		m.normalizeRestoredCursor()
	}
}

func (m *terminalModel) normalizeRestoredCursor() {
	// xterm clamps restored cursor coordinates to the grid, clearing delayed
	// wrap. The pinned VT retains the unrelated pre-restore phantom flag.
	position := m.emulator.CursorPosition()
	row := position.Y + 1
	if m.modes[ansi.DECMode(6)] {
		row -= m.marginTop
	}
	_, _ = fmt.Fprintf(m.emulator, "\x1b[%d;%dH", row, position.X+1)
	m.wrapPending = false
}

func (m *terminalModel) restoreCharsets(out *strings.Builder) {
	for i, designation := range m.charsets {
		fmt.Fprintf(out, "\x1b%c%c", '('+i, designation)
	}
	// xterm treats the legacy GR selection aliases as active GL selection.
	// Restore GR first, then select GL last so both parsers finish on the same
	// active ASCII/graphics set before future provider output arrives.
	out.WriteString([]string{"", "\x1b~", "\x1b}", "\x1b|"}[m.gr])
	out.WriteString([]string{"\x0f", "\x0e", "\x1bn", "\x1bo"}[m.gl])
	if m.singleShift == 2 {
		out.WriteString("\x1bN")
	} else if m.singleShift == 3 {
		out.WriteString("\x1bO")
	}
}
