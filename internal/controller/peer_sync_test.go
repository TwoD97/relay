package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/peers"
)

func peerFixture(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{config: Config{StateDir: dir}, ctx: ctx, cancel: cancel, store: store, links: map[string]*link{}, fleet: &fleetPublisher{identity: fleetIdentity{ID: strings.Repeat("a", 48)}}}
	if err = s.initializeWorkspace(); err != nil {
		t.Fatal(err)
	}
	s.control = openControlStore(dir)
	s.initializePeerSync()
	if s.peerSync.err != "" {
		t.Fatal(s.peerSync.err)
	}
	return s
}
func peerDocumentFixture() peerDocument {
	now := time.Now().UTC()
	pageID, taskID, sessionID, runtimeID := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32), strings.Repeat("4", 32)
	return peerDocument{Format: 1, Pages: []WorkspacePage{{WorkspacePageSummary: WorkspacePageSummary{ID: pageID, Title: "Saved page", UpdatedAt: now}, HTML: "<h1>Shared work</h1>", Revision: 9}}, Tasks: []peerTask{{WorkspaceTask: WorkspaceTask{ID: taskID, Title: "Follow up", Detail: "Read-only evidence", Status: "todo", PageID: pageID, SessionID: sessionID, UpdatedAt: now}, RuntimeID: runtimeID, SessionCreatedAt: &now}}, Groups: []peerGroup{{ID: "11111111-1111-4111-8111-111111111111", Title: "Work", Workspace: "Project", Panes: []peerPane{{RuntimeID: runtimeID, Session: sessionID, CreatedAt: now, HostName: "Source label"}}}}, Lessons: []ControlLesson{{ID: strings.Repeat("5", 32), Problem: "A past error", Action: "A verified correction", Outcome: "success", Evidence: "Fixture proof", CreatedAt: now}}}
}
func cachePeerFixture(t *testing.T, s *Server, doc peerDocument) peers.Record {
	t.Helper()
	data, _ := json.Marshal(doc)
	record, err := s.peerSync.cache.Put("", peers.Snapshot{SourceID: strings.Repeat("b", 48), Name: "Other desktop", Revision: 9, UpdatedAt: time.Now().UTC(), Data: data})
	if err != nil {
		t.Fatal(err)
	}
	s.peerSync.records[record.Digest] = peerCached{Record: record}
	return record
}
func peerImportRequest(s *Server, record peers.Record, id string, revision uint64) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"sourceId": record.SourceID, "digest": record.Digest, "id": id, "revision": revision})
	w := httptest.NewRecorder()
	s.importPeerSync(w, httptest.NewRequest("POST", "/api/peer-sync/import", bytes.NewReader(body)))
	return w
}
func installPeerObservation(t *testing.T, s *Server, hostID, runtimeID, instanceID string, sessions map[string]time.Time) {
	t.Helper()
	host := Host{ID: hostID, Name: hostID, Target: hostID + ".fixture.invalid", Status: "online", CreatedAt: time.Now().UTC()}
	if err := s.store.add(host); err != nil {
		t.Fatal(err)
	}
	l := &link{}
	s.links[hostID] = l
	s.peerSync.observed[hostID] = peerObservation{Host: host, Link: l, RuntimeID: runtimeID, InstanceID: instanceID, Sessions: sessions, ObservedAt: time.Now()}
}
func TestPeerImportCopiesSeparateContentAndMapsOnlyExactRuntimeIdentity(t *testing.T) {
	s := peerFixture(t)
	doc := peerDocumentFixture()
	record := cachePeerFixture(t, s, doc)
	installPeerObservation(t, s, "new-host", strings.Repeat("4", 32), strings.Repeat("6", 32), map[string]time.Time{doc.Groups[0].Panes[0].Session: doc.Groups[0].Panes[0].CreatedAt})
	original := WorkspacePage{WorkspacePageSummary: WorkspacePageSummary{ID: strings.Repeat("7", 32), Title: "Local page", UpdatedAt: time.Now().UTC()}, Revision: 1, HTML: "Keep me"}
	s.workspace.record.Pages = []WorkspacePage{original}
	s.workspace.record.Revision = 1
	w := peerImportRequest(s, record, strings.Repeat("c", 32), 1)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Workspace WorkspaceState       `json:"workspace"`
		Groups    []localTerminalGroup `json:"groups"`
		Warnings  []string             `json:"warnings"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Body.String())
	}
	if response.Workspace.Revision != 2 || len(response.Workspace.Pages) != 2 || response.Workspace.Pages[0].ID != original.ID || response.Workspace.Pages[1].ID == doc.Pages[0].ID {
		t.Fatal("copy overwrote local identity", response)
	}
	task := response.Workspace.Tasks[0]
	if task.HostID != "new-host" || task.SessionID != doc.Tasks[0].SessionID || task.PageID != response.Workspace.Pages[1].ID {
		t.Fatal("task mapping", task)
	}
	if len(response.Groups) != 1 || response.Groups[0].Panes[0].Host != "new-host" || response.Groups[0].ID == doc.Groups[0].ID {
		t.Fatal("group mapping", response.Groups)
	}
	if len(s.control.record.Lessons) != 1 || s.control.record.Lessons[0].ID == doc.Lessons[0].ID {
		t.Fatal("lesson copy")
	}
	repeat := peerImportRequest(s, record, strings.Repeat("c", 32), 2)
	if repeat.Code != 409 || s.workspace.record.Revision != 2 {
		t.Fatal("import replayed", repeat.Code)
	}
	next := peerFixture(t)
	next.config.StateDir = s.config.StateDir
	next.initializePeerSync()
	next.workspace = s.workspace
	again := peerImportRequest(next, record, strings.Repeat("c", 32), 2)
	if again.Code != 409 {
		t.Fatal("import journal lost on reopen", again.Code)
	}
}
func TestPeerImportRejectsCASAndClonedRuntimeMapping(t *testing.T) {
	s := peerFixture(t)
	doc := peerDocumentFixture()
	record := cachePeerFixture(t, s, doc)
	if w := peerImportRequest(s, record, strings.Repeat("c", 32), 99); w.Code != 409 || len(s.peerSync.input.Imports) != 0 {
		t.Fatal("stale CAS mutated", w.Code)
	}
	for _, host := range []string{"one", "two"} {
		installPeerObservation(t, s, host, strings.Repeat("4", 32), host, map[string]time.Time{})
	}
	w := peerImportRequest(s, record, strings.Repeat("d", 32), 0)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Groups   []localTerminalGroup `json:"groups"`
		Warnings []string             `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Groups) != 0 || len(response.Warnings) < 2 || s.workspace.record.Tasks[0].HostID != "" || s.workspace.record.Tasks[0].SessionID != "" {
		t.Fatal("ambiguous runtime rebound", w.Body.String())
	}
}
func TestPeerImportDispatchJournalSurvivesUnknownWorkspaceWrite(t *testing.T) {
	s := peerFixture(t)
	record := cachePeerFixture(t, s, peerDocumentFixture())
	old := s.workspace.path
	s.workspace.path = filepath.Join(s.config.StateDir, "missing", "workspace.json")
	w := peerImportRequest(s, record, strings.Repeat("c", 32), 0)
	if w.Code != 500 || len(s.peerSync.input.Imports) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.workspace.path = old
	w = peerImportRequest(s, record, strings.Repeat("c", 32), 0)
	if w.Code != 409 || s.workspace.record.Revision != 0 {
		t.Fatal("uncertain mutation retried", w.Code)
	}
}
func TestPeerImportEmptySnapshotStillAdvancesCAS(t *testing.T) {
	s := peerFixture(t)
	record := cachePeerFixture(t, s, peerDocument{Format: 1})
	w := peerImportRequest(s, record, strings.Repeat("c", 32), 0)
	if w.Code != 201 || s.workspace.record.Revision != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestPeerDocumentRejectsExecutableInboxAndMalformedReferences(t *testing.T) {
	doc := peerDocumentFixture()
	data, _ := json.Marshal(doc)
	var object map[string]any
	json.Unmarshal(data, &object)
	object["messages"] = []any{map[string]string{"text": "must not replay"}}
	raw, _ := json.Marshal(object)
	var parsed peerDocument
	if decodePeerDocument(raw, &parsed) == nil {
		t.Fatal("inbox accepted")
	}
	doc.Groups = append(doc.Groups, doc.Groups[0])
	data, _ = json.Marshal(doc)
	if decodePeerDocument(data, &parsed) == nil {
		t.Fatal("duplicate group accepted")
	}
}
func TestPeerPublicationExcludesInstructionsAndCredentialsAndRemovesDeletedContent(t *testing.T) {
	s := peerFixture(t)
	s.control.record.Messages = []ControlMessage{{Text: "PRIVATE-INBOX", Claim: "PRIVATE-CLAIM"}}
	s.publishPeerSnapshot()
	first, err := s.peerSync.cache.Get(s.peerSourceID())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first.Data, []byte("PRIVATE")) {
		t.Fatal("private control content copied")
	}
	s.workspace.record = workspaceRecord{Format: 1, Revision: 3, UpdatedAt: time.Now().UTC(), Pages: peerDocumentFixture().Pages}
	s.publishPeerSnapshot() // page fixture revision9 is deliberately invalid against3
	s.workspace.record.Revision = 9
	s.publishPeerSnapshot()
	second, err := s.peerSync.cache.Get(s.peerSourceID())
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision <= first.Revision {
		t.Fatal("no source advance")
	}
	s.workspace.record.Pages = nil
	s.publishPeerSnapshot()
	third, _ := s.peerSync.cache.Get(s.peerSourceID())
	var doc peerDocument
	if decodePeerDocument(third.Data, &doc) != nil || len(doc.Pages) != 0 || third.Revision <= second.Revision {
		t.Fatal("deleted page did not update snapshot")
	}
	if len(s.peerSync.records) != 1 {
		t.Fatal("publication retained unbounded prior digests")
	}
}
func TestPeerDiscoveryValidatesConflictingDigestBeforeShowingOrImporting(t *testing.T) {
	s := peerFixture(t)
	doc := peerDocumentFixture()
	record := cachePeerFixture(t, s, doc)
	bad := record
	bad.Data = json.RawMessage(`{"format":1,"pages":[],"tasks":[],"groups":[],"lessons":[]}`)
	bad.Digest = strings.Repeat("f", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("authorization forwarded")
		}
		switch r.URL.Path {
		case "/api/health":
			writeJSON(w, 200, map[string]any{"runtimeId": strings.Repeat("4", 32), "instanceId": strings.Repeat("6", 32), "capabilities": map[string]bool{"workspaceSyncV1": true}})
		case "/api/sessions":
			writeJSON(w, 200, []any{})
		case "/api/peers":
			writeJSON(w, 200, peers.Index{Sources: []peers.Summary{{SourceID: bad.SourceID, Digest: bad.Digest, Revision: bad.Revision}}})
		default:
			writeJSON(w, 200, bad)
		}
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	l := s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	})
	s.links["fixture"] = l
	s.inspectPeer(Host{ID: "fixture", Name: "Fixture", Status: "online"}, l)
	if _, ok := s.peerSync.records[bad.Digest]; ok {
		t.Fatal("unverified conflicting record retained")
	}
	if len(s.peerSync.warnings) == 0 {
		t.Fatal("tamper not reported")
	}
}
func TestPeerInputPreservesSymlinkAndRejectsRepeatedGroupRevision(t *testing.T) {
	s := peerFixture(t)
	external := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(external, []byte("untouched"), 0600)
	if err := os.Symlink(external, s.peerSync.inputPath); err != nil {
		// ERROR_PRIVILEGE_NOT_HELD is expected on Windows accounts without
		// Developer Mode or the symbolic-link privilege. No link exists to test.
		if runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(1314)) || errors.Is(err, os.ErrPermission)) {
			t.Skip("Windows account cannot create the symlink fixture: ", err)
		}
		t.Fatal(err)
	}
	s.initializePeerSync()
	if s.peerSync.err == "" {
		t.Fatal("input symlink accepted")
	}
	data, _ := os.ReadFile(external)
	if string(data) != "untouched" {
		t.Fatal("external file changed")
	}
}
