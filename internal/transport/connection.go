//go:build !windows && !relay_ssh_native

// Package transport owns authenticated OpenSSH connections. Authentication and
// host-key decisions stay in OpenSSH's terminal; credentials never enter logs.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type Connection struct {
	cfg        Config
	sshPath    string
	ctx        context.Context
	cancel     context.CancelFunc
	cmd        *exec.Cmd
	terminal   *os.File
	privateDir string
	done       chan struct{}
	mu         sync.Mutex
	inputMu    sync.Mutex
	ready      bool
	ended      bool
	err        error
	history    []byte
	subs       map[chan []byte]struct{}
}

func Start(ctx context.Context, cfg Config) (*Connection, error) {
	if err := ValidateTarget(cfg.Target); err != nil {
		return nil, err
	}
	if err := ValidatePort(cfg.Port); err != nil {
		return nil, err
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return nil, errors.New("OpenSSH is required: install the openssh-client package")
	}
	privateDir := ""
	if cfg.ControlPath == "" {
		privateDir, err = os.MkdirTemp("", "relay-ssh-")
		if err != nil {
			return nil, fmt.Errorf("create private SSH directory: %w", err)
		}
		cfg.ControlPath = filepath.Join(privateDir, "control")
	}
	if err := validateControlPath(cfg.ControlPath); err != nil {
		if privateDir != "" {
			_ = os.RemoveAll(privateDir)
		}
		return nil, err
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	c := &Connection{cfg: cfg, sshPath: sshPath, ctx: connectionCtx, cancel: cancel,
		privateDir: privateDir, done: make(chan struct{}), subs: make(map[chan []byte]struct{})}
	c.cmd = exec.CommandContext(connectionCtx, sshPath, masterArgs(cfg)...)
	c.cmd.Env = append(os.Environ(), "SSH_ASKPASS_REQUIRE=never", "TERM=xterm-256color")
	c.cmd.Cancel = func() error { return killProcessGroup(c.cmd) }
	c.cmd.WaitDelay = 2 * time.Second
	c.terminal, err = pty.StartWithSize(c.cmd, &pty.Winsize{Cols: 100, Rows: 28})
	if err != nil {
		cancel()
		if privateDir != "" {
			_ = os.RemoveAll(privateDir)
		}
		return nil, fmt.Errorf("start SSH: %w", err)
	}
	c.terminal, err = pollableTerminal(c.terminal)
	if err != nil {
		cancel()
		_ = c.cmd.Wait()
		if privateDir != "" {
			_ = os.RemoveAll(privateDir)
		}
		return nil, fmt.Errorf("prepare SSH terminal: %w", err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 16<<10)
		for {
			n, err := c.terminal.Read(buf)
			if n > 0 {
				c.publish(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := c.cmd.Wait()
		_ = c.terminal.Close()
		<-readDone
		c.mu.Lock()
		c.err = err
		c.ready = false
		c.ended = true
		for sub := range c.subs {
			close(sub)
			delete(c.subs, sub)
		}
		c.mu.Unlock()
		cancel()
		if privateDir != "" {
			_ = os.RemoveAll(privateDir)
		}
		close(c.done)
	}()
	return c, nil
}

func validateControlPath(path string) error {
	if !filepath.IsAbs(path) || len(path) > 100 || strings.ContainsAny(path, "%\r\n\x00") {
		return errors.New("SSH control path must be an absolute path shorter than 101 bytes without expansion characters")
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("SSH control directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("SSH control directory must be a private, user-owned directory (mode 0700)")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("SSH control path already exists or cannot be checked")
	}
	return nil
}

func commonArgs(cfg Config) []string {
	args := []string{"-T", "-a", "-x", "-S", cfg.ControlPath,
		"-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
		"-o", "RemoteCommand=none",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=15", "-o", "LogLevel=ERROR"}
	if cfg.Port > 0 {
		args = append(args, "-p", strconv.Itoa(cfg.Port))
	}
	return args
}

func masterArgs(cfg Config) []string {
	args := append(commonArgs(cfg), "-M", "-N", "-o", "ControlMaster=yes",
		"-o", "ControlPersist=no", "-o", "StrictHostKeyChecking=ask",
		"-o", "BatchMode=no",
		"-o", "ForkAfterAuthentication=no", "-o", "NumberOfPasswordPrompts=3")
	return append(args, "--", cfg.Target)
}

func slaveArgs(cfg Config) []string {
	// OpenSSH otherwise falls back to a new connection if the control socket
	// disappears. ProxyCommand=false makes that fallback fail before any login.
	return append(commonArgs(cfg), "-o", "ControlMaster=no", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "ProxyCommand=false", "-o", "SessionType=default")
}

func commandArgs(cfg Config, script string) []string {
	return append(slaveArgs(cfg), "--", cfg.Target, "/bin/sh -c "+quoteShell(script))
}

// WaitReady waits for host verification/authentication in the setup terminal.
func (c *Connection) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.disconnectedError()
		default:
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		args := append(slaveArgs(c.cfg), "-O", "check", "--", c.cfg.Target)
		cmd := exec.CommandContext(checkCtx, c.sshPath, args...)
		cmd.WaitDelay = time.Second
		err := cmd.Run()
		cancel()
		if err == nil {
			c.mu.Lock()
			if c.ctx.Err() == nil && !c.ended {
				c.ready = true
			}
			ready := c.ready
			c.mu.Unlock()
			if ready {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.disconnectedError()
		case <-ticker.C:
		}
	}
}

func (c *Connection) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && !c.ended && c.ctx.Err() == nil
}
func (c *Connection) Done() <-chan struct{} { return c.done }
func (c *Connection) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

func (c *Connection) disconnectedError() error {
	if err := c.Err(); err != nil {
		return fmt.Errorf("SSH disconnected: %w", err)
	}
	return errors.New("SSH disconnected; reconnect explicitly to log in again")
}

func (c *Connection) Write(data []byte) (int, error) {
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	if c.ctx.Err() != nil {
		return 0, c.disconnectedError()
	}
	if len(data) > 64<<10 {
		return 0, errors.New("terminal input exceeds 64 KiB")
	}
	if c.Ready() {
		return 0, errors.New("SSH authentication is complete")
	}
	if err := c.terminal.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return 0, err
	}
	return c.terminal.Write(data)
}

func (c *Connection) Resize(cols, rows uint16) error {
	if cols < 2 || cols > 1000 || rows < 2 || rows > 1000 {
		return errors.New("terminal dimensions must be between 2 and 1000")
	}
	return resizeTerminal(c.terminal, cols, rows)
}

// Subscribe registers atomically with the replay snapshot so output is neither
// skipped nor duplicated. A slow subscriber is closed and must reattach.
func (c *Connection) Subscribe() ([]byte, <-chan []byte, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sub := make(chan []byte, 64)
	if c.ctx.Err() == nil && !c.ended && len(c.subs) < 8 {
		c.subs[sub] = struct{}{}
	} else {
		close(sub)
	}
	cancel := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.subs[sub]; ok {
			delete(c.subs, sub)
			close(sub)
		}
	}
	return bytes.Clone(c.history), sub, cancel
}

func (c *Connection) publish(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) >= maxReplay {
		c.history = bytes.Clone(data[len(data)-maxReplay:])
	} else {
		if excess := len(c.history) + len(data) - maxReplay; excess > 0 {
			copy(c.history, c.history[excess:])
			c.history = c.history[:len(c.history)-excess]
		}
		c.history = append(c.history, data...)
	}
	chunk := bytes.Clone(data)
	for sub := range c.subs {
		select {
		case sub <- chunk:
		default:
			delete(c.subs, sub)
			close(sub)
		}
	}
}

func (c *Connection) Close() error {
	c.cancel()
	<-c.done
	return nil
}

// Run executes a trusted script, passing payload separately over stdin. Both
// stdout and stderr are bounded; the supplied context may impose a shorter limit.
func (c *Connection) Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	if !c.Ready() {
		return nil, c.disconnectedError()
	}
	if strings.IndexByte(script, 0) >= 0 {
		return nil, errors.New("remote script contains a NUL byte")
	}
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	cmd := exec.CommandContext(runCtx, c.sshPath, commandArgs(c.cfg, script)...)
	configureProcess(cmd)
	cmd.Stdin = stdin
	out := &boundedBuffer{limit: maxCommandOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil {
		if runCtx.Err() != nil {
			return out.Bytes(), fmt.Errorf("remote command: %w", runCtx.Err())
		}
		return out.Bytes(), fmt.Errorf("remote command failed: %w: %s", err, strings.TrimSpace(string(out.Bytes())))
	}
	if out.truncated {
		return out.Bytes(), errors.New("remote command output exceeded 1 MiB")
	}
	return out.Bytes(), nil
}

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
