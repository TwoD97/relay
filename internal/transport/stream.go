//go:build !windows && !relay_ssh_native

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

const bridgeScript = `exec "$HOME/.local/share/relay/bin/relay" bridge`

// DialContext implements http.Transport.DialContext. Network/address are the
// HTTP transport's bookkeeping only: traffic always reaches the fixed private
// Relay socket over this authenticated SSH connection, never an arbitrary host.
func (c *Connection) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !c.Ready() {
		return nil, c.disconnectedError()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" && network != "unix" {
		return nil, fmt.Errorf("unsupported transport network %q", network)
	}
	processCtx, cancel := context.WithCancel(c.ctx)
	stopDial := context.AfterFunc(ctx, cancel)
	defer stopDial()
	cmd := exec.CommandContext(processCtx, c.sshPath, commandArgs(c.cfg, bridgeScript)...)
	configureProcess(cmd)
	childIn, input, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, childOut, err := os.Pipe()
	if err != nil {
		cancel()
		_ = childIn.Close()
		_ = input.Close()
		return nil, err
	}
	cmd.Stdin = childIn
	cmd.Stdout = childOut
	cmd.Stderr = &boundedBuffer{limit: 16 << 10}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = childIn.Close()
		_ = input.Close()
		_ = output.Close()
		_ = childOut.Close()
		return nil, fmt.Errorf("start SSH bridge: %w", err)
	}
	_ = childIn.Close()
	_ = childOut.Close()
	stream := &processConn{input: input, output: output, cancel: cancel, done: make(chan struct{}), target: c.cfg.Target}
	go func() {
		_ = cmd.Wait()
		_ = input.Close()
		close(stream.done)
		cancel()
	}()
	// A dial context ends after dialing, not after the connection's lifetime.
	// The established stream remains attached to the master connection context.
	if !stopDial() && ctx.Err() != nil {
		_ = stream.Close()
		return nil, ctx.Err()
	}
	return stream, nil
}

type processConn struct {
	input     *os.File
	output    *os.File
	cancel    context.CancelFunc
	done      chan struct{}
	target    string
	closeOnce sync.Once
	closeErr  error
}

func (c *processConn) Read(p []byte) (int, error)  { return c.output.Read(p) }
func (c *processConn) Write(p []byte) (int, error) { return c.input.Write(p) }
func (c *processConn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		inErr := c.input.Close()
		outErr := c.output.Close()
		if inErr != nil && !errors.Is(inErr, os.ErrClosed) {
			c.closeErr = inErr
		}
		if outErr != nil && !errors.Is(outErr, os.ErrClosed) {
			c.closeErr = outErr
		}
		<-c.done
	})
	return c.closeErr
}

type sshAddr string

func (a sshAddr) Network() string           { return "ssh" }
func (a sshAddr) String() string            { return string(a) }
func (c *processConn) LocalAddr() net.Addr  { return sshAddr("relay-controller") }
func (c *processConn) RemoteAddr() net.Addr { return sshAddr(c.target) }
func (c *processConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
func (c *processConn) SetReadDeadline(t time.Time) error  { return c.output.SetReadDeadline(t) }
func (c *processConn) SetWriteDeadline(t time.Time) error { return c.input.SetWriteDeadline(t) }

var _ net.Conn = (*processConn)(nil)
