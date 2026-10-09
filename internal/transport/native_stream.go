//go:build windows || relay_ssh_native

package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

const bridgeScript = `exec "$HOME/.local/share/relay/bin/relay" bridge`

func (c *Connection) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" && network != "unix" {
		return nil, fmt.Errorf("unsupported transport network %q", network)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stopConnection := context.AfterFunc(c.ctx, cancel)
	defer stopConnection()
	session, err := c.newSession(dialCtx)
	if err != nil {
		return nil, err
	}
	local, remote := net.Pipe()
	var once sync.Once
	closeStream := func() { once.Do(func() { _ = local.Close(); _ = remote.Close(); _ = session.Close() }) }
	stopLifetime := context.AfterFunc(c.ctx, closeStream)
	stopDial := context.AfterFunc(dialCtx, closeStream)
	session.Stdin = remote
	session.Stdout = remote
	session.Stderr = &lockedBuffer{b: boundedBuffer{limit: 16 << 10}}
	if err := session.Start("/bin/sh -c " + quoteShell(bridgeScript)); err != nil {
		stopDial()
		stopLifetime()
		closeStream()
		return nil, err
	}
	if !stopDial() && dialCtx.Err() != nil {
		stopLifetime()
		closeStream()
		return nil, dialCtx.Err()
	}
	go func() { _ = session.Wait(); stopLifetime(); closeStream() }()
	return &nativeStream{Conn: local, close: func() { stopLifetime(); closeStream() }}, nil
}

type nativeStream struct {
	net.Conn
	close func()
}

func (c *nativeStream) Close() error { c.close(); return nil }
