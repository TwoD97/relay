//go:build linux

package controller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	runtimeapi "github.com/TwoD97/relay/internal/runtime"
)

func TestPeerTwoControllersDiscoverAndImportThroughRealRuntimeWithoutSessionMutation(t *testing.T) {
	daemon, err := runtimeapi.New(t.TempDir(), "peer-test")
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	server := httptest.NewServer(daemon.Handler())
	defer server.Close()
	create, _ := http.NewRequest("POST", server.URL+"/api/sessions", strings.NewReader(`{"harness":"shell","cwd":"`+t.TempDir()+`","title":"Owned fixture","workspace":"Fixture"}`))
	create.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	var session runtimeapi.Session
	json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatal("fixture shell", response.StatusCode)
	}
	connect := func(s *Server, id string) {
		host := Host{ID: id, Name: id, Target: id + ".fixture.invalid", Status: "online", CreatedAt: time.Now().UTC()}
		if err := s.store.add(host); err != nil {
			t.Fatal(err)
		}
		target, _ := url.Parse(server.URL)
		s.links[id] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
		})
	}
	source := peerFixture(t)
	connect(source, "source-host")
	source.peerSync.input.Ready = true
	source.peerSync.input.Revision = 1
	source.peerSync.input.Groups = []localTerminalGroup{{ID: "11111111-1111-4111-8111-111111111111", Title: "Owned layout", HostName: "source-host", Workspace: "Fixture", Panes: []localGroupPane{{Host: "source-host", Session: session.ID, CreatedAt: session.CreatedAt}}}}
	now := time.Now().UTC()
	source.workspace.record = workspaceRecord{Format: 1, Revision: 1, UpdatedAt: now, Pages: []WorkspacePage{{WorkspacePageSummary: WorkspacePageSummary{ID: strings.Repeat("c", 32), Title: "Shared proof", UpdatedAt: now}, HTML: "<h1>Copied proof</h1>", Revision: 1}}, Tasks: []WorkspaceTask{{ID: strings.Repeat("d", 32), Title: "Check original terminal", Status: "todo", HostID: "source-host", SessionID: session.ID, UpdatedAt: now}}}
	source.control.record.Messages = []ControlMessage{{Text: "NEVER-REPLAY-THIS-INBOX", Claim: "DO-NOT-COPY"}}
	source.refreshPeers()
	record, err := source.peerSync.cache.Get(source.peerSourceID())
	if err != nil {
		t.Fatal(err)
	}
	destination := peerFixture(t)
	destination.fleet.identity.ID = strings.Repeat("e", 48)
	connect(destination, "destination-host")
	destination.refreshPeers()
	if _, found := destination.peerSync.records[record.Digest]; !found {
		t.Fatalf("shared runtime did not expose source snapshot: %v", destination.peerSync.warnings)
	}
	w := peerImportRequest(destination, record, strings.Repeat("f", 32), 0)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var imported struct {
		Groups   []localTerminalGroup `json:"groups"`
		Warnings []string             `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &imported)
	if len(imported.Groups) != 1 || imported.Groups[0].Panes[0].Host != "destination-host" || imported.Groups[0].Panes[0].Session != session.ID || destination.workspace.record.Tasks[0].HostID != "destination-host" {
		t.Fatal("cross-controller identity mapping failed", w.Body.String())
	}
	if len(destination.control.record.Messages) != 0 {
		t.Fatal("inbox copied")
	}
	response, err = http.Get(server.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var sessions []runtimeapi.Session
	json.Unmarshal(data, &sessions)
	if len(sessions) != 1 || sessions[0].ID != session.ID || sessions[0].Status != "running" {
		t.Fatal("metadata import mutated original PTY", string(data))
	}
	reopened := peerFixture(t)
	reopened.config.StateDir = destination.config.StateDir
	reopened.initializePeerSync()
	if reopened.peerSync.input.Imports[strings.Repeat("f", 32)].Status != "completed" {
		t.Fatal("durable import receipt absent")
	}
}
