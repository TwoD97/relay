package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type migrationFixture struct {
	s, d                      *Server
	destination               *httptest.Server
	host                      Host
	session                   MastermindSession
	stops, activations, seeds atomic.Int32
	mu                        sync.Mutex
	events                    []string
	lostStop, lostActivation  bool
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	f := &migrationFixture{s: peerFixture(t), d: peerFixture(t)}
	for _, server := range []*Server{f.s, f.d} {
		server.mastermind, _ = openMastermindStore(server.config.StateDir)
		server.mastermindWake = make(chan struct{}, 1)
		server.migration = openMigrationStore(server.config.StateDir)
		server.coordinator = openCoordinatorStore(server.config.StateDir)
		server.config.LocalSocket = "/fixture/runtime/run/daemon.sock"
	}
	f.session = MastermindSession{ID: strings.Repeat("1", 32), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Status: "running", Harness: "claude", AgentMode: "manual", MastermindID: strings.Repeat("2", 32), Title: "Coordinator", Workspace: "Mastermind", Cwd: "/fixture/project"}
	sourceSession := f.session
	f.s.mastermind.record = mastermindRecord{CanonicalCwd: f.session.Cwd, Format: 1, Phase: "attached", Token: f.session.MastermindID, HostTarget: f.s.config.LocalSocket, MastermindState: MastermindState{Revision: 1, Enabled: true, Status: "running", Config: &MastermindConfig{HostID: "local", Harness: "claude", AgentMode: "manual", Cwd: f.session.Cwd}, Session: &sourceSession, UpdatedAt: time.Now().UTC()}}
	sourceRuntime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			writeJSON(w, 200, map[string]any{"capabilities": map[string]bool{"sessionStopV1": true}})
		case "/api/sessions":
			f.mu.Lock()
			copy := f.session
			f.mu.Unlock()
			writeJSON(w, 200, []MastermindSession{copy})
		case "/api/sessions/" + f.session.ID + "/stop":
			var body struct {
				CreatedAt time.Time `json:"createdAt"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if !body.CreatedAt.Equal(f.session.CreatedAt) {
				t.Error("wrong source identity")
			}
			f.stops.Add(1)
			f.mu.Lock()
			f.session.Status = "exited"
			f.events = append(f.events, "stop")
			copy := f.session
			lost := f.lostStop
			f.mu.Unlock()
			if lost {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			writeJSON(w, 200, copy)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(sourceRuntime.Close)
	target, _ := url.Parse(sourceRuntime.URL)
	f.s.links["local"] = f.s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	})
	f.host = Host{ID: "destination", Name: "Destination", Target: "fixture.invalid", Port: 22, CreatedAt: time.Now().UTC(), Status: "online"}
	if err := f.s.store.add(f.host); err != nil {
		t.Fatal(err)
	}
	f.s.links[f.host.ID] = f.s.links["local"]
	_, err := f.s.control.enqueue(strings.Repeat("3", 32), "desktop", "Continue the saved task", "")
	if err != nil {
		t.Fatal(err)
	}
	handler := f.d.CoordinatorHandler()
	f.destination = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Origin") != "" {
			t.Error("source credentials forwarded")
		}
		switch r.URL.Path {
		case "/coordinator/standby":
			writeJSON(w, 200, coordinatorStandby{Protocol: 1, Blockers: []string{}, Warnings: []string{}})
			return
		case "/coordinator/seed":
			f.seeds.Add(1)
			if !f.s.coordinatorDelegated() {
				t.Error("seed preceded source capability fence")
			}
			if _, e := f.s.control.enqueue(strings.Repeat("4", 32), "desktop", "must not race seed", ""); e == nil {
				t.Error("source inbox accepted late instruction")
			}
		case "/coordinator/activate":
			f.activations.Add(1)
			if f.stops.Load() != 1 {
				t.Error("destination activated before exact source stop")
			}
			f.mu.Lock()
			f.events = append(f.events, "activate")
			lost := f.lostActivation
			f.mu.Unlock()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != 202 {
				w.WriteHeader(response.Code)
				w.Write(response.Body.Bytes())
				return
			}
			f.d.mastermind.mu.Lock()
			f.d.mastermind.record.Status = "running"
			f.d.mastermind.record.Phase = "attached"
			f.d.mastermind.record.Token = strings.Repeat("5", 32)
			f.d.mastermind.record.Session = &MastermindSession{ID: strings.Repeat("6", 32), CreatedAt: time.Now().UTC(), Status: "running", Harness: "claude", AgentMode: "manual", MastermindID: strings.Repeat("5", 32), Cwd: f.d.mastermindDirectory()}
			f.d.mastermind.mu.Unlock()
			if lost {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			w.WriteHeader(202)
			w.Write(response.Body.Bytes())
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.destination.Close)
	f.s.migration.client = func(context.Context, Host, *link, string, bool) (*http.Client, error) {
		target, _ := url.Parse(f.destination.URL)
		return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
		}, DisableKeepAlives: true}}, nil
	}
	return f
}
func migrationAPI(s *Server, handler http.HandlerFunc, value any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(value)
	w := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "http://fixture/", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler(w, request)
	return w
}
func migrationStart(t *testing.T, f *migrationFixture) {
	t.Helper()
	response := migrationAPI(f.s, f.s.preflightMigration, map[string]string{"hostId": f.host.ID})
	var plan migrationPlan
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &plan) != nil || len(plan.Blockers) > 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = migrationAPI(f.s, f.s.startMigration, map[string]string{"hostId": f.host.ID, "id": strings.Repeat("a", 32), "fingerprint": plan.Fingerprint})
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
}
func awaitMigration(t *testing.T, f *migrationFixture, status string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := f.s.migration.snapshot()
		if rec.Operation != nil && rec.Operation.Status == status {
			f.s.connectWG.Wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, err := f.s.migration.snapshot()
	t.Fatalf("migration did not reach %s: %+v %s", status, rec.Operation, err)
}
func TestMigrationStopsExactSourceBeforeActivatingAndProxiesAuthoritativeWorkspace(t *testing.T) {
	f := newMigrationFixture(t)
	migrationStart(t, f)
	awaitMigration(t, f, "active")
	if f.stops.Load() != 1 || f.activations.Load() != 1 || f.seeds.Load() != 1 {
		t.Fatal("unexpected replay")
	}
	if f.s.mastermind.current().Enabled || f.s.workspace.record.FrozenBy == "" || f.s.control.record.FrozenBy == "" {
		t.Fatal("source fence absent")
	}
	if len(f.d.control.record.Messages) != 1 || f.d.control.record.Messages[0].Text != "Continue the saved task" {
		t.Fatal("queued instructions lost")
	}
	state, hosted := f.s.hostedMastermindState()
	if state.Revision <= f.s.mastermind.record.Revision {
		t.Fatal("destination revision could be discarded as stale", state.Revision, f.s.mastermind.record.Revision)
	}
	if !hosted || state.ControllerHostID != f.host.ID || state.Config.HostID != f.host.ID || state.Session.ID != strings.Repeat("6", 32) {
		t.Fatal("hosted identity mapping", state)
	}
	r := httptest.NewRequest("POST", "http://fixture/api/mastermind/control/messages", strings.NewReader(`{"id":"`+strings.Repeat("b", 32)+`","text":"After handoff"}`))
	r.Header.Set("Authorization", "Bearer browser-private")
	r.Header.Set("Cookie", "private-cookie")
	w := httptest.NewRecorder()
	f.s.routeCoordinator(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("fell back to frozen local API") })).ServeHTTP(w, r)
	if w.Code != 201 || len(f.d.control.record.Messages) != 2 || len(f.s.control.record.Messages) != 1 {
		t.Fatal("authoritative proxy failed", w.Code, w.Body.String())
	}
	again := openMigrationStore(f.s.config.StateDir)
	if again.record.Binding == nil || !again.record.SourceStopped {
		t.Fatal("hosted authority lost on restart")
	}
}
func TestMigrationLostActivationUsesOnlyExactReceiptAndDoesNotRepeat(t *testing.T) {
	f := newMigrationFixture(t)
	f.lostActivation = true
	migrationStart(t, f)
	awaitMigration(t, f, "active")
	for i := 0; i < 3; i++ {
		f.s.reconcileMigration(context.Background())
	}
	if f.activations.Load() != 1 || f.stops.Load() != 1 {
		t.Fatal("lost activation replayed")
	}
}
func TestMigrationUnknownSourceStopNeverActivatesOrReplays(t *testing.T) {
	f := newMigrationFixture(t)
	f.lostStop = true
	migrationStart(t, f)
	awaitMigration(t, f, "uncertain")
	if f.activations.Load() != 0 || f.stops.Load() != 1 {
		t.Fatal("unknown stop activated/replayed")
	}
	for i := 0; i < 3; i++ {
		f.s.reconcileMigration(context.Background())
	}
	if f.activations.Load() != 0 || f.stops.Load() != 1 {
		t.Fatal("status check mutated")
	}
}
func TestMigrationProcessingInboxAndChangedSourceRejectBeforeStop(t *testing.T) {
	f := newMigrationFixture(t)
	f.s.control.mu.Lock()
	f.s.control.record.Messages[0].Status = "processing"
	f.s.control.record.Messages[0].Claim = "active"
	f.s.control.mu.Unlock()
	response := migrationAPI(f.s, f.s.preflightMigration, map[string]string{"hostId": f.host.ID})
	var plan migrationPlan
	json.Unmarshal(response.Body.Bytes(), &plan)
	if len(plan.Blockers) == 0 {
		t.Fatal("processing instruction allowed")
	}
	f.s.control.mu.Lock()
	f.s.control.record.Messages[0].Status = "queued"
	f.s.control.record.Messages[0].Claim = ""
	f.s.control.mu.Unlock()
	response = migrationAPI(f.s, f.s.preflightMigration, map[string]string{"hostId": f.host.ID})
	json.Unmarshal(response.Body.Bytes(), &plan)
	f.s.control.enqueue(strings.Repeat("f", 32), "desktop", "A newer instruction", "")
	response = migrationAPI(f.s, f.s.startMigration, map[string]string{"id": strings.Repeat("a", 32), "hostId": f.host.ID, "fingerprint": plan.Fingerprint})
	if response.Code != 409 || f.stops.Load() != 0 || f.seeds.Load() != 0 {
		t.Fatal("stale plan acted", response.Code)
	}
}
func TestCoordinatorSeedAndActivationRejectReplayAndBrowserHeaders(t *testing.T) {
	f := newMigrationFixture(t)
	h := f.d.CoordinatorHandler()
	r := httptest.NewRequest("GET", "/coordinator/status", nil)
	r.Header.Set("Origin", "http://127.0.0.1:1234")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("browser reached private admin")
	}
	migrationStart(t, f)
	awaitMigration(t, f, "active")
	receipt := f.d.coordinator.receipt
	r = httptest.NewRequest("POST", "/coordinator/activate", strings.NewReader(`{"id":"`+receipt.ID+`","digest":"`+receipt.Digest+`"}`))
	w = httptest.NewRecorder()
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatal("activation replay accepted")
	}
}
func TestMigrationFailedDestinationLeavesSourceUntouched(t *testing.T) {
	f := newMigrationFixture(t)
	f.s.migration.client = func(context.Context, Host, *link, string, bool) (*http.Client, error) {
		return nil, errors.New("fixture unavailable")
	}
	w := migrationAPI(f.s, f.s.preflightMigration, map[string]string{"hostId": f.host.ID})
	var plan migrationPlan
	json.Unmarshal(w.Body.Bytes(), &plan)
	if len(plan.Blockers) == 0 || f.s.coordinatorDelegated() || !f.s.mastermind.record.Enabled || f.stops.Load() != 0 {
		t.Fatal("failed standby changed source")
	}
}

func TestHostedAvailabilityUsesRealRevisionAndObservationTimeAcrossReconnect(t *testing.T) {
	f := newMigrationFixture(t)
	migrationStart(t, f)
	awaitMigration(t, f, "active")
	live, _ := f.s.hostedMastermindState()
	if live.Status != "running" {
		t.Fatal(live)
	}
	factory := f.s.migration.client
	f.s.migration.client = func(context.Context, Host, *link, string, bool) (*http.Client, error) {
		return nil, errors.New("fixture offline")
	}
	offline, _ := f.s.hostedMastermindState()
	if offline.Status != "waiting" || offline.Revision != live.Revision || offline.UpdatedAt.Before(live.UpdatedAt) || offline.Session.ID != live.Session.ID {
		t.Fatal("offline fallback hidden as stale", live, offline)
	}
	f.s.migration.client = factory
	online, _ := f.s.hostedMastermindState()
	if online.Status != "running" || online.Revision != live.Revision || online.UpdatedAt.Before(offline.UpdatedAt) {
		t.Fatal("reconnect remained stale", online)
	}
	reopened := openMigrationStore(f.s.config.StateDir)
	if reopened.record.LastHosted == nil || reopened.record.LastHosted.Revision != live.Revision {
		t.Fatal("authority revision lost across controller restart")
	}
}

func TestCoordinatorLargeValidHTMLSeedDoesNotExpandBeyondWireBound(t *testing.T) {
	f := newMigrationFixture(t)
	server := httptest.NewServer(f.d.CoordinatorHandler())
	defer server.Close()
	target, _ := url.Parse(server.URL)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	}}}
	seed := coordinatorSeed{ID: strings.Repeat("a", 32), SourceID: strings.Repeat("b", 48), SourceRevision: 1, Harness: "claude", AgentMode: "manual", Workspace: workspaceRecord{Format: 1, Revision: 1, UpdatedAt: time.Now().UTC()}, Control: controlRecord{Format: 1}, Hosts: []Host{}}
	for i := 0; i < 16; i++ {
		seed.Workspace.Pages = append(seed.Workspace.Pages, WorkspacePage{WorkspacePageSummary: WorkspacePageSummary{ID: fmt.Sprintf("%032x", i+1), Title: fmt.Sprintf("Page %d", i), UpdatedAt: time.Now().UTC()}, HTML: strings.Repeat("<", 128<<10), Revision: 1})
	}
	data, err := peerRequest(context.Background(), client, "POST", "/coordinator/seed", seed, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	var receipt coordinatorReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.Phase != "ready" || len(f.d.workspace.record.Pages) != 16 {
		t.Fatal("valid bounded seed failed", string(data))
	}
}

func TestMigrationLateKnownEditDoesNotFreezeSourceBeforeAnyDispatch(t *testing.T) {
	f := newMigrationFixture(t)
	plan, err := f.s.migrationSnapshot(f.host)
	if err != nil {
		t.Fatal(err)
	}
	record := migrationRecord{Format: 1, Host: f.host, CoordinatorID: plan.CoordinatorID, Source: plan.Source, Operation: &MigrationOperation{ID: strings.Repeat("a", 32), HostID: f.host.ID, Status: "preparing"}}
	f.s.migration.mu.Lock()
	err = f.s.migration.saveLocked(record)
	f.s.migration.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.control.enqueue(strings.Repeat("e", 32), "desktop", "Arrived while destination was preparing", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.freezeMigration(record, plan); err == nil {
		t.Fatal("stale plan froze source")
	}
	saved, _ := f.s.migration.snapshot()
	if saved.SourceFrozen || f.s.control.record.FrozenBy != "" || f.s.workspace.record.FrozenBy != "" || !f.s.mastermind.record.Enabled || f.stops.Load() != 0 || f.activations.Load() != 0 {
		t.Fatal("known pre-dispatch rejection changed source")
	}
}
