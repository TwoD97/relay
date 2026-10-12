//go:build linux && !relay_ssh_native

package transport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFleetEndpointResolvesAliasWithoutExportingAuthConfiguration(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "ssh")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' 'hostname 10.42.0.8' 'user operator' 'port 2222' 'identityfile /private/do-not-publish' 'identityagent secret-socket'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := &Connection{sshPath: executable, cfg: Config{Target: "desktop-only-alias"}}
	target, port, err := c.FleetEndpoint(context.Background())
	if err != nil || target != "operator@10.42.0.8" || port != 2222 {
		t.Fatal("SSH alias not resolved", target, port, err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' 'hostname 10.42.0.8' 'port 22'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.FleetEndpoint(context.Background()); err == nil {
		t.Fatal("missing SSH user was guessed")
	}
}
