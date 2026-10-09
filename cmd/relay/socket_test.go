//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingLiveSocketAndNonSocketsArePreserved(t *testing.T) {
	dir, err := os.MkdirTemp("", "relay-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err = removeStaleSocket(socket); err == nil {
		t.Fatal("live endpoint was accepted as stale")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal("live endpoint removed", err)
	}
	conn.Close()
	file := filepath.Join(dir, "file")
	if err = os.WriteFile(file, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = removeStaleSocket(file); err == nil {
		t.Fatal("regular file removed")
	}
	link := filepath.Join(dir, "link")
	if err = os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err = removeStaleSocket(link); err == nil {
		t.Fatal("symlink removed")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "preserve me" {
		t.Fatal("existing data changed")
	}
}

func TestOnlyRefusedStaleSocketIsRemoved(t *testing.T) {
	dir, err := os.MkdirTemp("", "relay-stale-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "daemon.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err = removeStaleSocket(socket); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatal("stale socket not removed", err)
	}
}
