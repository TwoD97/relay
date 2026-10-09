package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

func validateSessionSSH(host string, port int) error {
	if err := transport.ValidatePort(port); err != nil {
		return err
	}
	if host == "" {
		if port != 0 {
			return errors.New("--ssh-port requires --ssh")
		}
		return nil
	}
	return transport.ValidateTarget(host)
}

func sessionSSHArgs(host string, port int) []string {
	args := []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectionAttempts=1", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=2", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
		"-o", "RemoteCommand=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no"}
	if port != 0 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	// The remote command is constant. Session IDs, prompts and paths travel as
	// HTTP JSON on stdin, never as interpolated remote shell commands.
	return append(args, "--", host, `exec "$HOME/.local/share/relay/bin/relay" bridge`)
}

func dialSessionSSH(ctx context.Context, host string, port int) (net.Conn, error) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return nil, errors.New("cross-machine session commands require an OpenSSH client on this machine")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, ssh, sessionSSHArgs(host, port)...)
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, output, err := os.Pipe()
	if err != nil {
		cancel()
		_ = in.Close()
		return nil, err
	}
	cmd.Stdout = output
	if err := cmd.Start(); err != nil {
		cancel()
		_ = in.Close()
		_ = out.Close()
		_ = output.Close()
		return nil, err
	}
	_ = output.Close()
	c := &sshSessionConn{in: in, out: out, cancel: cancel, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(c.done) }()
	return c, nil
}

type sshSessionConn struct {
	in     io.WriteCloser
	out    io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
}

func (c *sshSessionConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *sshSessionConn) Write(p []byte) (int, error) { return c.in.Write(p) }
func (c *sshSessionConn) Close() error {
	c.once.Do(func() {
		_ = c.in.Close()
		_ = c.out.Close()
		c.cancel()
		<-c.done
	})
	return nil
}
func (c *sshSessionConn) LocalAddr() net.Addr  { return sshSessionAddr("local") }
func (c *sshSessionConn) RemoteAddr() net.Addr { return sshSessionAddr("remote") }

// The request context owns the bounded lifetime of this one-request stream.
func (c *sshSessionConn) SetDeadline(time.Time) error      { return nil }
func (c *sshSessionConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshSessionConn) SetWriteDeadline(time.Time) error { return nil }

type sshSessionAddr string

func (a sshSessionAddr) Network() string { return "ssh" }
func (a sshSessionAddr) String() string  { return string(a) }
