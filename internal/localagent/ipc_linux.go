//go:build linux

package localagent

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
)

func Endpoint(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "run", "local-agent.sock"), nil
}
func DialContext(ctx context.Context, dir string) (net.Conn, error) {
	endpoint, err := Endpoint(dir)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, err
	}
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err == nil {
		var peer *unix.Ucred
		var e error
		err = raw.Control(func(fd uintptr) { peer, e = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		if err == nil {
			err = e
		}
		if err == nil && (peer == nil || peer.Uid != uint32(os.Geteuid())) {
			err = errors.New("local agent socket belongs to another user")
		}
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
