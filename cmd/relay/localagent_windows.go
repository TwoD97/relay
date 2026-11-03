package main

import (
	"github.com/Microsoft/go-winio"
	"github.com/TwoD97/relay/internal/localagent"
	"net"
)

func listenLocalAgent(dir string) (net.Listener, func(), error) {
	path, err := localagent.Endpoint(dir)
	if err != nil {
		return nil, nil, err
	}
	sid, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + sid.String() + ")", InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	return listener, func() {}, err
}
