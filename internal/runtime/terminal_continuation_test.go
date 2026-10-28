//go:build linux

package runtime

import (
	"os"
	"strings"
	"testing"
)

func TestSnapshotContinuesDelayedWrap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []string
		next  string
	}{
		{"final-column", []string{"12345678"}, "Z"},
		{"not-yet-final-column", []string{"1234567"}, "8Z"},
		{"sgr-keeps-wrap", []string{"12345678\x1b[31m"}, "Z"},
		{"carriage-return", []string{"12345678\r"}, "Z"},
		{"backspace", []string{"12345678\b"}, "Z"},
		{"cursor-position-same-column", []string{"12345678\x1b[1;8H"}, "ZQ"},
		{"cursor-forward-at-edge", []string{"12345678\x1b[C"}, "ZQ"},
		{"erase-character", []string{"12345678\x1b[X"}, "ZQ"},
		{"linefeed", []string{"12345678\n"}, "ZQ"},
		{"combining-grapheme", []string{"1234567e\u0301"}, "Z"},
		{"split-utf8", []string{"1234567\xc3", "\xa9"}, "Z"},
		{"wide-at-right-edge", []string{"123456界"}, "Z"},
		{"wide-on-final-column", []string{"1234567界"}, "Z"},
		{"split-wide", []string{"123456\xe7", "\x95\x8c"}, "Z"},
		{"saved-cursor", []string{"1234\x1b7\x1b[1;8Hx\x1b8"}, "Z"},
		{"restore-final-column", []string{"12345678\x1b7\r\x1b8"}, "Z"},
		{"scroll-region", []string{"\x1b[2;3r\x1b[3;1H12345678"}, "Z"},
		{"origin-mode", []string{"\x1b[2;3r\x1b[?6h\x1b[2;1H12345678"}, "Z"},
		{"alternate-buffer", []string{"main\x1b[?1049h12345678"}, "Z"},
		{"wrap-disabled", []string{"\x1b[?7l12345678"}, "Z"},
		{"tab-preserves-wrap", []string{"12345678\t"}, "Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := newTerminalModel(8, 4)
			for _, part := range tc.parts {
				original.write([]byte(part))
			}
			restored := newTerminalModel(8, 4)
			// No history: the authoritative snapshot must be sufficient even
			// after the byte ring has evicted the original setup sequence.
			restored.write(original.snapshot(nil))
			compareScreens(t, original, restored)
			original.write([]byte(tc.next))
			restored.write([]byte(tc.next))
			compareScreens(t, original, restored)
		})
	}
}

func TestSnapshotContinuesCharacterSets(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"graphics", "\x1b(0qq", "qq"},
		{"uk", "\x1b(A#", "#"},
		{"reset-ascii", "\x1b(0q\x1b(Bq", "q"},
		{"locking-g2", "\x1b*0\x1bnqq", "q"},
		{"locking-g3", "\x1b+0\x1boqq", "q"},
		{"charset-after-wrap", "\x1b(0qqqqqqqq", "q"},
		{"reset", "\x1b(0q\x1bc", "q"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := newTerminalModel(8, 4)
			original.write([]byte(tc.before))
			restored := newTerminalModel(8, 4)
			restored.write(original.snapshot(nil))
			compareScreens(t, original, restored)
			original.write([]byte(tc.after))
			restored.write([]byte(tc.after))
			compareScreens(t, original, restored)
		})
	}
}

func TestWideGlyphWrapMatchesVisibleGrid(t *testing.T) {
	for _, chunks := range [][]string{{"123456界Z"}, {"123456界", "Z"}, {"123456", "\xe7", "\x95\x8cZ"}} {
		m := newTerminalModel(8, 4)
		for _, chunk := range chunks {
			m.write([]byte(chunk))
		}
		if got := m.emulator.String(); got != "123456界\nZ\n\n" {
			t.Fatalf("wide glyph continuation overwrote the final cell: %q", got)
		}
	}
}

func TestSplitGraphemeDoesNotConsumeDelayedWrap(t *testing.T) {
	for _, tc := range []struct{ name, before, continuation, expected string }{
		{"accent", "1234567e", "\u0301", "1234567e\u0301\nZ\n\n"},
		{"zwj", "123456👩", "\u200d💻", "123456👩\u200d💻\nZ\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTerminalModel(8, 4)
			m.write([]byte(tc.before))
			restored := newTerminalModel(8, 4)
			restored.write(m.snapshot(nil))
			for _, part := range []string{tc.continuation, "Z"} {
				m.write([]byte(part))
				restored.write([]byte(part))
			}
			compareScreens(t, m, restored)
			if got := m.emulator.String(); got != tc.expected {
				t.Fatalf("split grapheme consumed delayed wrap: %q", got)
			}
		})
	}
}

func TestFailedResizePreservesCanonicalGeometry(t *testing.T) {
	for _, controlled := range []bool{false, true} {
		name := "startup"
		if controlled {
			name = "control-owner"
		}
		t.Run(name, func(t *testing.T) {
			p := newLiveSession(Session{})
			_, _ = p.Write([]byte("\x1b[5;10r\x1b[1;90Hkept"))
			before := p.screen.emulator.Render()
			file, err := os.Open("/dev/null")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			p.pty = file // deterministic ENOTTY, without altering a live PTY
			_, owner, _ := p.subscribe()
			_, viewer, _ := p.subscribe()
			defer p.unsubscribe(owner)
			defer p.unsubscribe(viewer)
			p.claimControl(owner)
			latestControl(t, owner)
			latestControl(t, viewer)
			if controlled {
				err = p.resizeControlled(owner, 80, 24)
			} else {
				err = p.resize(80, 24)
			}
			if err == nil {
				t.Fatal("expected PTY ioctl failure")
			}
			if p.cols != 100 || p.rows != 30 || p.screen.emulator.Render() != before || p.screen.marginTop != 4 || p.screen.marginBottom != 10 {
				t.Fatal("failed resize changed canonical screen or geometry")
			}
			select {
			case <-viewer.frames:
				t.Fatal("failed resize published false geometry")
			default:
			}
			p.controlError(owner, err)
			state := latestControl(t, owner)
			if state.Cols != 100 || state.Rows != 30 || !state.Owner || state.Reason == "" {
				t.Fatal("resize error did not report actual geometry and retained owner")
			}
		})
	}
}

func TestUnchangedResizePreservesTerminalState(t *testing.T) {
	p := newLiveSession(Session{})
	_, _ = p.Write([]byte("\x1b[5;10r\x1b[1;1H" + strings.Repeat("x", 100)))
	if !p.screen.wrapPending {
		t.Fatal("fixture did not reach delayed wrap")
	}
	if err := p.resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if !p.screen.wrapPending || p.screen.marginTop != 4 || p.screen.marginBottom != 10 {
		t.Fatal("duplicate resize reset cursor or scroll region")
	}
}
