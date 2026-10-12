//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/fleetregistry"
)

func sessionFleetFixture(t *testing.T, self bool, now time.Time) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "relay-fleet-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	snapshot := fleetregistry.Snapshot{
		Format: 1, ControllerID: strings.Repeat("a", 48), Revision: 1,
		IssuedAt: now, ExpiresAt: now.Add(fleetregistry.Lifetime),
		Hosts: []fleetregistry.Host{{ID: "machine-1", Name: "Named deployment host", ConfiguredTarget: "desktop-only-alias", Target: "deploy@fixture.example", Port: 2222}},
	}
	if self {
		snapshot.SelfHostID = "machine-1"
		snapshot.Hosts[0].Target, snapshot.Hosts[0].Port = "", 0
		snapshot.Hosts[0].Unavailable = "Desktop loopback is not portable"
	}
	if err := fleetregistry.Import(context.Background(), filepath.Join(dir, "fleet"), snapshot, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "run", "daemon.sock")
}

func sessionFleetSSHFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec "+quoted+" -test.run=TestSessionSSHHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELAY_TEST_SSH_HELPER", "1")
}

func TestSessionFleetResolvesNameAndSendsExplicitModeLiterally(t *testing.T) {
	socket := sessionFleetFixture(t, false, time.Now().UTC())
	sessionFleetSSHFixture(t)
	// --socket chooses the source fleet registry, never the target's runtime.
	t.Setenv("RELAY_SOCKET", "/unrelated/runtime.sock")
	literal := "/project with 'quotes' and $(false)"
	output, err := runSessionCLI(t, "start", "--socket", socket, "--host", "Named deployment host", "--harness", "claude", "--mode", "auto", "--cwd", literal)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Method, Path, Body string
		Args               []string
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.Path != "/api/sessions" || body["agentMode"] != "auto" || body["harness"] != "claude" || body["cwd"] != literal {
		t.Fatalf("launch request changed: %+v", got)
	}
	args := strings.Join(got.Args, " ")
	if !strings.Contains(args, "-p 2222 -- deploy@fixture.example") || !strings.Contains(args, "StrictHostKeyChecking=yes") || !strings.Contains(args, "BatchMode=yes") {
		t.Fatalf("canonical endpoint or trust policy lost: %s", args)
	}
	for _, forbidden := range []string{literal, socket, "desktop-only-alias", "Named deployment host", "--mode", "--dangerously"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("data leaked into remote command: %q", forbidden)
		}
	}
}

func TestSessionFleetSelfUsesOwningSocketWithoutSSH(t *testing.T) {
	socket := sessionFleetFixture(t, true, time.Now().UTC())
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/sessions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"self":true}`))
	}), ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("PATH", t.TempDir()) // SSH is deliberately unavailable.
	t.Setenv("RELAY_SOCKET", socket)
	data, err := runSessionCLI(t, "list", "--host", "machine-1")
	if err != nil || string(data) != `{"self":true}` {
		t.Fatalf("self host did not use owning runtime: %s, %v", data, err)
	}
}

func TestSessionFleetMissingOrStaleHostNeverMutates(t *testing.T) {
	sessionFleetSSHFixture(t)
	for _, stale := range []bool{false, true} {
		now := time.Now().UTC()
		name := "unknown host"
		if stale {
			now = now.Add(-2 * fleetregistry.Lifetime)
			name = "Named deployment host"
		}
		socket := sessionFleetFixture(t, false, now)
		calls := filepath.Join(t.TempDir(), "ssh-calls")
		t.Setenv("RELAY_TEST_SSH_CALLS", calls)
		t.Setenv("RELAY_TEST_SSH_RESPONSE", "drop")
		if _, err := runSessionCLI(t, "start", "--socket", socket, "--host", name, "--harness", "codex", "--mode", "auto"); err == nil {
			t.Fatalf("stale=%v: unresolved destination was accepted", stale)
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatalf("stale=%v: attempted SSH despite unresolved destination: %v", stale, err)
		}
	}
}
