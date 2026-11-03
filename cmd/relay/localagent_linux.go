package main

import (
	"github.com/TwoD97/relay/internal/localagent"
	"net"
	"os"
)

func listenLocalAgent(dir string) (net.Listener, func(), error) {
	path, err := localagent.Endpoint(dir)
	if err != nil {
		return nil, nil, err
	}
	if err = removeStaleSocket(path); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(path) }
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		cleanup()
		return nil, nil, err
	}
	return listener, cleanup, nil
}
