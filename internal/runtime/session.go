//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type byteRing struct {
	data        []byte
	start, size int
}

func (b *byteRing) append(v []byte) {
	if len(v) == 0 {
		return
	}
	if b.data == nil {
		b.data = make([]byte, maxHistory)
	}
	if len(v) >= maxHistory {
		copy(b.data, v[len(v)-maxHistory:])
		b.start = 0
		b.size = maxHistory
		return
	}
	end := (b.start + b.size) % maxHistory
	n := copy(b.data[end:], v)
	copy(b.data, v[n:])
	if excess := b.size + len(v) - maxHistory; excess > 0 {
		b.start = (b.start + excess) % maxHistory
		b.size = maxHistory
	} else {
		b.size += len(v)
	}
}
func (b *byteRing) bytes() []byte {
	v := make([]byte, b.size)
	if b.size == 0 {
		return v
	}
	n := copy(v, b.data[b.start:min(len(b.data), b.start+b.size)])
	copy(v[n:], b.data[:b.size-n])
	return v
}

type terminalFrame struct {
	Data    []byte
	Control *controlMessage
	Ended   bool
}
type subscriber struct {
	initial controlMessage
	frames  chan terminalFrame
	done    chan struct{}
	session *liveSession
	closed  bool
}
type liveSession struct {
	replies                      chan []byte
	screen                       *terminalModel
	owner                        *subscriber
	lastInput                    time.Time
	eventToken                   string
	mu                           sync.Mutex
	inputMu                      sync.Mutex
	processMu                    sync.Mutex
	processReaped                bool
	meta                         Session
	history                      byteRing
	dirty                        bool
	cmd                          *exec.Cmd
	pty                          *os.File
	subscribers                  map[*subscriber]struct{}
	cancel                       context.CancelFunc
	done                         chan struct{}
	finished, stopped, interrupt bool
	inputReady                   bool
	cols, rows                   uint16
	stopOnce                     sync.Once
}

func newLiveSession(meta Session) *liveSession {
	return &liveSession{replies: make(chan []byte, 16), meta: meta, subscribers: make(map[*subscriber]struct{}), done: make(chan struct{}), cols: 100, rows: 30}
}
func (p *liveSession) snapshot() Session { p.mu.Lock(); defer p.mu.Unlock(); return p.meta }
func (p *liveSession) Write(v []byte) (int, error) {
	data := append([]byte(nil), v...)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history.append(data)
	if p.screen == nil {
		p.screen = newTerminalModel(int(p.cols), int(p.rows))
		p.screen.reply = p.queueTerminalReplyLocked
	}
	p.screen.respond = p.owner == nil
	p.screen.write(data)
	p.dirty = true
	for sub := range p.subscribers {
		p.enqueueFrameLocked(sub, terminalFrame{Data: data})
	}

	return len(v), nil
}
func (p *liveSession) subscribe() ([]byte, *subscriber, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.subscribers) >= maxSubscribers {
		return nil, nil, errors.New("too many terminal viewers")
	}
	sub := &subscriber{frames: make(chan terminalFrame, 32), done: make(chan struct{}), session: p}
	if p.screen != nil && p.screen.pendingOversized {
		return nil, nil, errors.New("terminal is receiving a control sequence larger than 64 KiB; retry after it completes")
	}
	snapshot := p.history.bytes()
	if p.screen != nil {
		snapshot = p.screen.snapshot(snapshot)
	}
	p.expireControlLocked(time.Now())
	sub.initial = p.controlStateLocked(sub, "")
	enqueueControl(sub, sub.initial)
	if p.finished {
		p.enqueueFrameLocked(sub, terminalFrame{Ended: true})
		sub.closed = true
	} else {
		p.subscribers[sub] = struct{}{}
	}
	return snapshot, sub, nil
}
func (p *liveSession) dropSubscriberLocked(sub *subscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.done)
	delete(p.subscribers, sub)
	if p.owner == sub {
		p.owner = nil
		p.publishControlLocked("Control holder disconnected")
	}
}
func (p *liveSession) enqueueFrameLocked(sub *subscriber, frame terminalFrame) {
	if sub.closed {
		return
	}
	select {
	case sub.frames <- frame:
	default:
		p.dropSubscriberLocked(sub)
	}
}
func (p *liveSession) unsubscribe(sub *subscriber) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropSubscriberLocked(sub)
}
func (p *liveSession) writeInput(data string) error { return p.writeInputControlled(nil, data) }
func (p *liveSession) writeInputControlled(sub *subscriber, data string) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxInput {
		return errors.New("input exceeds 64 KiB")
	}
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.mu.Lock()
	if err := p.inputPermissionLocked(sub); err != nil {
		p.mu.Unlock()
		return err
	}
	f := p.pty
	ready := p.inputReady && !p.finished && !p.stopped && f != nil
	p.mu.Unlock()
	if !ready {
		return errors.New("session is not accepting input")
	}
	if err := f.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return fmt.Errorf("terminal write deadline: %w", err)
	}
	_, err := io.WriteString(f, data)
	return err
}
func (p *liveSession) resize(cols, rows int) error {
	if cols < 2 || cols > 500 || rows < 2 || rows > 300 {
		return errors.New("terminal size must be 2–500 columns and 2–300 rows")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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
func (p *liveSession) stop(interrupted bool) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.interrupt = interrupted
		if p.cancel != nil {
			p.cancel()
		}
		p.mu.Unlock()
		_ = p.signalProcess(syscall.SIGTERM)
		select {
		case <-p.done:
			return
		case <-time.After(1500 * time.Millisecond):
		}
		_ = p.signalProcess(syscall.SIGKILL)
		p.mu.Lock()
		f := p.pty
		p.mu.Unlock()
		if f != nil {
			_ = f.Close()
		}
		<-p.done
	})
}

// Final reaping and every ordinary session signal share this lock. Keeping the
// original leader alive (including as a zombie) reserves its session ID; no
// signalling path is allowed to use that ID once cmd.Wait has reaped it.
func (p *liveSession) signalProcess(signal syscall.Signal) error {
	p.processMu.Lock()
	defer p.processMu.Unlock()
	if p.processReaped {
		return nil
	}
	p.mu.Lock()
	identity := p.meta.ProcessIdentity
	p.mu.Unlock()
	if identity == nil {
		return nil
	}
	err := signalSession(identity, signal)
	if err != nil && signal == syscall.SIGKILL {
		// Failure to verify descendants must not leave our own child running
		// forever. Go's Process handle targets this unreaped child only.
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_, _ = fmt.Fprintf(p, "\r\nRelay: Session cleanup could not verify all processes; inspect this host before starting duplicate work: %s\r\n", err)
	}
	return err
}

func (p *liveSession) waitCommand() error {
	p.processMu.Lock()
	defer p.processMu.Unlock()
	err := p.cmd.Wait()
	p.processReaped = true
	return err
}

// Wait for exit without releasing the leader PID. Unlike cmd.Wait this leaves
// a zombie available while remaining same-session children are cleaned up.
func waitCommandExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

type commandFactory func(context.Context, io.Writer) (*exec.Cmd, error)

func (s *Server) start(meta Session, cols, rows int, factory commandFactory) (Session, error) {
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	meta.ID = id
	meta.Status = "running"
	meta.CreatedAt = now
	meta.UpdatedAt = now
	p := newLiveSession(meta)
	token, err := newID()
	if err != nil {
		return Session{}, err
	}
	p.eventToken = token
	if cols != 0 || rows != 0 {
		if cols == 0 {
			cols = 100
		}
		if rows == 0 {
			rows = 30
		}
		if err = p.resize(cols, rows); err != nil {
			return Session{}, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return Session{}, errors.New("runtime is shutting down")
	}
	if !maintenancePurpose(meta.Purpose) && s.installing[meta.Harness] {
		s.mu.Unlock()
		cancel()
		return Session{}, errors.New("harness installation is in progress")
	}
	if len(s.sessions) >= maxSessions {
		s.mu.Unlock()
		cancel()
		return Session{}, errors.New("session limit reached (32); remove completed sessions first")
	}
	s.sessions[id] = p
	if err = s.persistLocked(); err != nil {
		delete(s.sessions, id)
		s.mu.Unlock()
		cancel()
		return Session{}, err
	}
	go s.run(ctx, p, factory)
	s.mu.Unlock()
	return meta, nil
}
func (s *Server) run(ctx context.Context, p *liveSession, factory commandFactory) {
	defer p.cancel()
	cmd, err := factory(ctx, p)
	if err != nil {
		s.finish(p, 127, err)
		return
	}
	p.mu.Lock()
	if ctx.Err() != nil || p.stopped {
		p.mu.Unlock()
		s.finish(p, 130, nil)
		return
	}
	cmd.Dir = p.meta.Cwd
	if cmd.Env == nil {
		cmd.Env = s.environment()
	}
	cmd.Env = setEnv(cmd.Env, "RELAY_SOCKET", filepath.Join(s.stateDir, "run", "daemon.sock"))
	cmd.Env = setEnv(cmd.Env, "RELAY_SESSION_ID", p.meta.ID)
	cmd.Env = setEnv(cmd.Env, "RELAY_EVENT_TOKEN", p.eventToken)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: p.cols, Rows: p.rows})
	if err != nil {
		p.mu.Unlock()
		s.finish(p, 127, err)
		return
	}
	p.cmd = cmd
	identity, identityErr := identifyProcess(cmd.Process.Pid)
	if identityErr != nil {
		p.mu.Unlock()
		// This exact, unreaped child is ours. Without a verified identity, do
		// not infer ownership of any other process from its numeric PID.
		_ = cmd.Process.Kill()
		_ = f.Close()
		_ = p.waitCommand()
		s.finish(p, 127, fmt.Errorf("identify session process: %w", identityErr))
		return
	}
	p.meta.ProcessIdentity = identity
	// NewFile sees O_NONBLOCK and registers this PTY with Go's poller, so writes
	// have deadlines and Close unblocks readers even while child PTYs remain open.
	rawfd := int(f.Fd())
	dup, dupErr := syscall.Dup(rawfd)
	if dupErr == nil {
		syscall.CloseOnExec(dup)
		dupErr = syscall.SetNonblock(dup, true)
	}
	if dupErr != nil {
		if dup >= 0 {
			_ = syscall.Close(dup)
		}
		p.mu.Unlock()
		_ = p.signalProcess(syscall.SIGKILL)
		_ = f.Close()
		_ = p.waitCommand()
		s.finish(p, 127, dupErr)
		return
	}
	pollable := os.NewFile(uintptr(dup), "relay-pty")
	_ = f.Close()
	f = pollable
	p.pty = f
	p.mu.Unlock()
	s.mu.Lock()
	identityErr = s.persistLocked()
	s.mu.Unlock()
	if identityErr != nil {
		// A running session must not knowingly continue without durable identity.
		// This child has not been reaped, so its session ID cannot be reused here.
		_ = p.signalProcess(syscall.SIGKILL)
		_ = f.Close()
		_ = p.waitCommand()
		s.finish(p, 127, fmt.Errorf("save session process identity: %w", identityErr))
		return
	}
	p.mu.Lock()
	p.inputReady = !p.stopped && ctx.Err() == nil
	p.mu.Unlock()
	go p.replyLoop(ctx.Done())
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			n, readErr := f.Read(buf)
			if n > 0 {
				_, _ = p.Write(buf[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()
	exitErr := waitCommandExit(cmd.Process.Pid)
	// A shell can exit with background children holding the slave open. They
	// belong to this session. Keep the unreaped leader reserving the session ID
	// until all scoped signalling is done; the helper pins each target PID.
	if exitErr == nil {
		_ = p.signalProcess(syscall.SIGTERM)
		select {
		case <-readDone:
		case <-time.After(150 * time.Millisecond):
		}
		_ = p.signalProcess(syscall.SIGKILL)
	} else {
		// If a non-reaping wait cannot establish exit, fail closed for
		// descendants. The exact own Process handle remains safe to stop.
		_ = cmd.Process.Kill()
	}
	waitErr := p.waitCommand()
	_ = f.Close()
	<-readDone
	code := 0
	if waitErr != nil {
		code = 1
		var ex *exec.ExitError
		if errors.As(waitErr, &ex) {
			code = ex.ExitCode()
			if code < 0 {
				code = 128
			}
		}
	}
	s.finish(p, code, exitErr)
}
func (s *Server) finish(p *liveSession, code int, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(p, "\r\nRelay: %s\r\n", err)
	}
	p.mu.Lock()
	p.finished = true
	p.meta.Status = "exited"
	p.meta.ExitCode = &code
	if p.interrupt {
		p.meta.Status = "interrupted"
		p.meta.ExitCode = nil
	}
	p.meta.UpdatedAt = time.Now().UTC()
	p.pty = nil
	if p.meta.Purpose == "login" {
		p.history = byteRing{}
		p.screen = nil
		p.dirty = false
	}
	p.owner = nil
	p.publishControlLocked("Session ended")
	for sub := range p.subscribers {
		p.enqueueFrameLocked(sub, terminalFrame{Ended: true})
		sub.closed = true
		delete(p.subscribers, sub)
	}
	p.mu.Unlock()
	s.mu.Lock()
	_ = s.persistLocked()
	_ = s.flushHistoryLocked()
	s.mu.Unlock()
	close(p.done)
}
