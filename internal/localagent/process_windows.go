//go:build windows

package localagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// ConPTY uses Microsoft's public pseudoconsole API, x/sys for handles, and
// go-winio for cancellable overlapped pipes. These are the same native building
// blocks used by Wave Terminal. No shell parses provider arguments.
type windowsProcess struct {
	input, output       net.Conn
	console, job, child windows.Handle
	identity            json.RawMessage
	mu                  sync.Mutex
	waited              bool
	closeOnce           sync.Once
}

// The ConPTY side of each channel must be synchronous; the service side is an
// overlapped go-winio pipe so bounded input deadlines and Close cancel pending IO.
func consolePipe() (net.Conn, windows.Handle, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, 0, err
	}
	path := `\\.\pipe\Relay-conpty-` + id()
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + sid.String() + ")", InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		return nil, 0, err
	}
	defer listener.Close()
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() { c, e := listener.Accept(); ch <- accepted{c, e} }()
	name, _ := windows.UTF16PtrFromString(path)
	var handle windows.Handle
	deadline := time.Now().Add(2 * time.Second)
	for {
		handle, err = windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		_ = listener.Close()
		got := <-ch
		if got.conn != nil {
			got.conn.Close()
		}
		return nil, 0, err
	}
	got := <-ch
	if got.err != nil {
		windows.CloseHandle(handle)
		return nil, 0, got.err
	}
	return got.conn, handle, nil
}
func startProcess(c command, cols, rows int) (_ process, err error) {
	p := &windowsProcess{}
	var in, out windows.Handle
	p.input, in, err = consolePipe()
	if err != nil {
		return nil, err
	}
	p.output, out, err = consolePipe()
	if err != nil {
		p.input.Close()
		windows.CloseHandle(in)
		return nil, err
	}
	defer windows.CloseHandle(in)
	defer windows.CloseHandle(out)
	defer func() {
		if err != nil {
			if p.job != 0 {
				_ = windows.TerminateJobObject(p.job, 127)
				windows.CloseHandle(p.job)
			}
			if p.child != 0 {
				windows.CloseHandle(p.child)
			}
			p.input.Close()
			p.output.Close()
			if p.console != 0 {
				windows.ClosePseudoConsole(p.console)
			}
		}
	}()
	err = windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, in, out, 0, &p.console)
	if err != nil {
		return nil, fmt.Errorf("create native Windows terminal: %w", err)
	}
	p.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(p.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, err
	}
	attrs, e := windows.NewProcThreadAttributeList(1)
	if e != nil {
		return nil, e
	}
	defer attrs.Delete()
	// Pseudoconsole attributes take the HPCON value, not a pointer to it.
	update := windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")
	result, _, callErr := update.Call(uintptr(unsafe.Pointer(attrs.List())), 0, windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(p.console), unsafe.Sizeof(p.console), 0, 0)
	if result == 0 {
		return nil, callErr
	}

	si := windows.StartupInfoEx{}
	si.Flags = windows.STARTF_USESTDHANDLES
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrs.List()
	app, err := windows.UTF16PtrFromString(c.Path)
	if err != nil {
		return nil, err
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{c.Path}, c.Args...)))
	if err != nil {
		return nil, err
	}
	dir, err := windows.UTF16PtrFromString(c.Dir)
	if err != nil {
		return nil, err
	}
	env := append([]string(nil), c.Env...)
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	for _, v := range env {
		if strings.ContainsRune(v, 0) {
			return nil, errors.New("environment contains NUL")
		}
	}
	block := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	pi := windows.ProcessInformation{}
	err = windows.CreateProcess(app, line, nil, nil, false, windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT, &block[0], dir, &si.StartupInfo, &pi)
	if err != nil {
		return nil, err
	}
	p.child = pi.Process
	defer windows.CloseHandle(pi.Thread)
	// No instruction executes before assignment to the owned job. Descendants
	// inherit the job; no breakaway flag is enabled.
	if err = windows.AssignProcessToJobObject(p.job, p.child); err != nil {
		_ = windows.TerminateProcess(p.child, 127)
		return nil, err
	}
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(p.child, &created, &exited, &kernel, &user); err != nil {
		return nil, err
	}
	p.identity, _ = json.Marshal(struct {
		PID     uint32 `json:"pid"`
		Created uint64 `json:"created"`
	}{pi.ProcessId, uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)})
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *windowsProcess) Read(b []byte) (int, error)         { return p.output.Read(b) }
func (p *windowsProcess) Write(b []byte) (int, error)        { return p.input.Write(b) }
func (p *windowsProcess) SetWriteDeadline(t time.Time) error { return p.input.SetWriteDeadline(t) }
func (p *windowsProcess) Identity() json.RawMessage          { return p.identity }
func (p *windowsProcess) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.console == 0 {
		return io.ErrClosedPipe
	}
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}
func (p *windowsProcess) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(p.job, 130)
}
func (p *windowsProcess) Wait() (int, error) {
	_, err := windows.WaitForSingleObject(p.child, windows.INFINITE)
	var code uint32
	if e := windows.GetExitCodeProcess(p.child, &code); err == nil {
		err = e
	}
	p.mu.Lock()
	p.waited = true
	if p.job != 0 {
		e := windows.TerminateJobObject(p.job, 130)
		if err == nil {
			err = e
		}
		windows.CloseHandle(p.job)
		p.job = 0
	}
	console := p.console
	p.console = 0
	p.mu.Unlock()
	// The service reader continues draining while ClosePseudoConsole emits its
	// last frame (required to avoid deadlock on Windows releases before 24H2).
	if console != 0 {
		windows.ClosePseudoConsole(console)
	}
	return int(code), err
}
func (p *windowsProcess) Close() error {
	p.closeOnce.Do(func() {
		_ = p.Stop()
		p.input.Close()
		p.output.Close()
		p.mu.Lock()
		console := p.console
		p.console = 0
		if p.job != 0 {
			windows.CloseHandle(p.job)
			p.job = 0
		}
		if p.child != 0 {
			windows.CloseHandle(p.child)
			p.child = 0
		}
		p.mu.Unlock()
		if console != 0 {
			windows.ClosePseudoConsole(console)
		}
	})
	return nil
}
func recoverProcess(_ json.RawMessage) string {
	return "The native process job closed with the service; saved output remains available."
}
