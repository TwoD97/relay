package runtime

import "errors"

// Screen is the same bounded VT model used by remote Linux sessions. Its caller
// serializes all access; replies must be queued, never synchronously written.
type Screen struct{ model *terminalModel }

func NewScreen(cols, rows int) *Screen { return &Screen{newTerminalModel(cols, rows)} }
func (s *Screen) Write(data []byte, respond bool, reply func([]byte)) {
	s.model.respond = respond
	s.model.reply = reply
	s.model.write(data)
}
func (s *Screen) Resize(cols, rows int) { s.model.resize(cols, rows) }
func (s *Screen) Snapshot(history []byte) ([]byte, error) {
	if s.model.pendingOversized {
		return nil, errors.New("terminal control sequence exceeds 64 KiB; retry after it completes")
	}
	return s.model.snapshot(history), nil
}
