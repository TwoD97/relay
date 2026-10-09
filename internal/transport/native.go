//go:build windows || relay_ssh_native

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// Windows uses an in-process SSH connection, since Windows OpenSSH does not
// provide the Unix ControlMaster/PTY contract used by the Linux client.
type Connection struct {
	cfg             Config
	ctx             context.Context
	cancel          context.CancelFunc
	done, readyDone chan struct{}
	mu              sync.Mutex
	ready, ended    bool
	err             error
	client          *ssh.Client
	sessionSlots    chan struct{}
	history         []byte
	subs            map[chan []byte]struct{}
	prompt          chan string
	promptEcho      bool
	input           []byte
}

func Start(ctx context.Context, cfg Config) (*Connection, error) {
	if err := ValidateTarget(cfg.Target); err != nil {
		return nil, err
	}
	if err := ValidatePort(cfg.Port); err != nil {
		return nil, err
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	c := &Connection{cfg: cfg, ctx: connectionCtx, cancel: cancel, done: make(chan struct{}), readyDone: make(chan struct{}), subs: make(map[chan []byte]struct{})}
	go c.connect()
	return c, nil
}

type deadlineSSHConn struct{ net.Conn }

func (c deadlineSSHConn) Write(data []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.Conn.Write(data)
}

func (c *Connection) connect() {
	var connectionErr error
	becameReady := false
	defer func() {
		c.cancel()
		c.mu.Lock()
		c.ready, c.ended, c.err = false, true, connectionErr
		clear(c.input)
		c.input = nil
		c.prompt = nil
		for sub := range c.subs {
			close(sub)
			delete(c.subs, sub)
		}
		c.mu.Unlock()
		if !becameReady {
			close(c.readyDone)
		}
		close(c.done)
	}()
	resolved, err := resolveNativeConfig(c.ctx, c.cfg)
	if err != nil {
		connectionErr = err
		c.publish([]byte("\r\nRelay: " + err.Error() + "\r\n"))
		return
	}
	config, closeAuth, err := c.authConfig(resolved)
	if err != nil {
		connectionErr = err
		return
	}
	defer closeAuth()
	c.publish([]byte("Connecting to " + resolved.address() + " as " + resolved.user + "...\r\n"))
	raw, err := (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext(c.ctx, "tcp", resolved.address())
	if err != nil {
		connectionErr = err
		return
	}
	defer raw.Close()
	stop := context.AfterFunc(c.ctx, func() { _ = raw.Close() })
	defer stop()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Minute))
	conn, channels, requests, err := ssh.NewClientConn(deadlineSSHConn{raw}, resolved.address(), config)
	if err != nil {
		connectionErr = err
		c.publish([]byte("\r\nSSH connection failed: " + err.Error() + "\r\n"))
		return
	}
	_ = raw.SetDeadline(time.Time{})
	client := ssh.NewClient(conn, channels, requests)
	defer client.Close()
	c.mu.Lock()
	c.client = client
	c.ready = true
	c.mu.Unlock()
	becameReady = true
	close(c.readyDone)
	c.publish([]byte("\r\nSSH authentication complete.\r\n"))
	go c.keepAlive(client)
	connectionErr = client.Wait()
}

func (c *Connection) keepAlive(client *ssh.Client) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		}
		result := make(chan error, 1)
		go func() { _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); result <- err }()
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case err := <-result:
			timer.Stop()
			if err != nil {
				c.cancel()
				return
			}
		case <-timer.C:
			c.cancel()
			return
		}
	}
}

func (c *Connection) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.readyDone:
	}
	if c.Ready() {
		return nil
	}
	return c.disconnectedError()
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
func (c *Connection) Close() error { c.cancel(); <-c.done; return nil }
func (c *Connection) Resize(cols, rows uint16) error {
	if cols < 2 || cols > 1000 || rows < 2 || rows > 1000 {
		return errors.New("invalid terminal dimensions")
	}
	return nil
}

func (c *Connection) ask(message string, echo bool) (string, error) {
	answer := make(chan string, 1)
	c.mu.Lock()
	clear(c.input)
	c.input = nil
	c.prompt = answer
	c.promptEcho = echo
	c.mu.Unlock()
	c.publish([]byte(message))
	defer func() {
		c.mu.Lock()
		if c.prompt == answer {
			c.prompt = nil
			clear(c.input)
			c.input = nil
		}
		c.mu.Unlock()
	}()
	select {
	case <-c.ctx.Done():
		return "", c.ctx.Err()
	case value := <-answer:
		return value, nil
	}
}

func (c *Connection) Write(data []byte) (int, error) {
	if len(data) > 64<<10 {
		return 0, errors.New("terminal input exceeds 64 KiB")
	}
	c.mu.Lock()
	if c.ended || c.ctx.Err() != nil {
		c.mu.Unlock()
		return 0, c.disconnectedError()
	}
	if c.ready {
		c.mu.Unlock()
		return 0, errors.New("SSH authentication is complete")
	}
	if c.prompt == nil {
		c.mu.Unlock()
		return 0, errors.New("SSH is not waiting for input")
	}
	var display []byte
	for _, b := range data {
		switch b {
		case 3:
			clear(c.input)
			c.input = nil
			c.prompt = nil
			c.mu.Unlock()
			c.cancel()
			return len(data), nil
		case '\r', '\n':
			value := string(c.input)
			clear(c.input)
			c.input = nil
			c.prompt <- value
			c.prompt = nil
			display = append(display, '\r', '\n')
			c.mu.Unlock()
			c.publish(display)
			return len(data), nil // Never queue input for a later credential prompt.
		case 8, 127:
			if len(c.input) > 0 {
				_, size := utf8.DecodeLastRune(c.input)
				clear(c.input[len(c.input)-size:])
				c.input = c.input[:len(c.input)-size]
				if c.promptEcho {
					display = append(display, "\b \b"...)
				}
			}
		default:
			if b < 32 {
				continue
			}
			if len(c.input) >= 4096 {
				c.mu.Unlock()
				return 0, errors.New("SSH prompt input exceeds 4 KiB")
			}
			c.input = append(c.input, b)
			if c.promptEcho {
				display = append(display, b)
			}
		}
	}
	c.mu.Unlock()
	if len(display) > 0 {
		c.publish(display)
	}
	return len(data), nil
}

func (c *Connection) Subscribe() ([]byte, <-chan []byte, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sub := make(chan []byte, 64)
	if !c.ended && c.ctx.Err() == nil && len(c.subs) < 8 {
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

func (c *Connection) newSession(ctx context.Context) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	client := c.client
	ready := c.ready && !c.ended && c.ctx.Err() == nil
	if c.sessionSlots == nil {
		c.sessionSlots = make(chan struct{}, 16)
	}
	slots := c.sessionSlots
	c.mu.Unlock()
	if !ready || client == nil {
		return nil, c.disconnectedError()
	}
	// SSH channel opens have no per-request cancellation in x/crypto/ssh.
	// Bound outstanding opens; a cancelled HTTP lookup must not disconnect
	// unrelated terminal streams that share this authenticated connection.
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.disconnectedError()
	}
	type result struct {
		s   *ssh.Session
		err error
	}
	done := make(chan result)
	go func() {
		defer func() { <-slots }()
		s, err := client.NewSession()
		select {
		case done <- result{s, err}:
			return
		case <-ctx.Done():
		case <-c.ctx.Done():
		}
		if s != nil {
			_ = s.Close()
		}
	}()
	select {
	case got := <-done:
		return got.s, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.disconnectedError()
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  boundedBuffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) result() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.b.Bytes()), b.b.truncated
}

func (c *Connection) Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	if strings.IndexByte(script, 0) >= 0 {
		return nil, errors.New("remote script contains a NUL byte")
	}
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	session, err := c.newSession(runCtx)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	stopSession := context.AfterFunc(runCtx, func() { _ = session.Close() })
	defer stopSession()
	out := &lockedBuffer{b: boundedBuffer{limit: maxCommandOutput}}
	session.Stdin = stdin
	session.Stdout = out
	session.Stderr = out
	err = session.Run("/bin/sh -c " + quoteShell(script))
	data, truncated := out.result()
	if runCtx.Err() != nil {
		return data, fmt.Errorf("remote command: %w", runCtx.Err())
	}
	if err != nil {
		return data, fmt.Errorf("remote command failed: %w: %s", err, strings.TrimSpace(string(data)))
	}
	if truncated {
		return data, errors.New("remote command output exceeded 1 MiB")
	}
	return data, nil
}
