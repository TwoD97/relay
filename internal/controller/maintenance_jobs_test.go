package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const maintenanceTestSessionID = "0123456789abcdef0123456789abcdef"

func maintenanceUnitServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	hosts, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := openMaintenanceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Server{config: Config{StateDir: dir, LocalSocket: "fixture"}, ctx: ctx, cancel: cancel, store: hosts, maintenance: jobs, links: map[string]*link{}}
}
func maintenanceTestJob(t *testing.T, s *Server) MaintenanceJob {
	t.Helper()
	job, err := s.maintenance.add(Host{ID: "local", Target: "fixture"}, "codex", "update")
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func maintenanceRecordFor(t *testing.T, s *Server, job MaintenanceJob) maintenanceRecord {
	t.Helper()
	rec, ok := s.maintenance.get(job.ID)
	if !ok {
		t.Fatal("job missing")
	}
	return rec
}
func maintenanceResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestMaintenanceDurablePhasesPrecedeMutationsAndUnknownInputNeverReplays(t *testing.T) {
	for _, scenario := range []string{"success", "create-unknown", "input-unknown", "bad-created-at"} {
		t.Run(scenario, func(t *testing.T) {
			s := maintenanceUnitServer(t)
			job := maintenanceTestJob(t, s)
			creates, inputs := 0, 0
			started := time.Now().UTC()
			client := &http.Client{Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) {
				data, err := os.ReadFile(s.maintenance.path)
				if err != nil {
					t.Fatal(err)
				}
				var disk struct {
					Jobs []maintenanceRecord `json:"jobs"`
				}
				if err := json.Unmarshal(data, &disk); err != nil {
					t.Fatal(err)
				}
				rec := disk.Jobs[0]
				switch r.URL.Path {
				case "/api/sessions":
					creates++
					if rec.Phase != "creating" || rec.Job.SessionID != "" {
						t.Fatalf("create preceded intent: %+v", rec)
					}
					if scenario == "create-unknown" {
						return nil, io.ErrUnexpectedEOF
					}
					session := maintenanceSession{ID: maintenanceTestSessionID, CreatedAt: started, Status: "running"}
					if scenario == "bad-created-at" {
						session.CreatedAt = time.Time{}
					}
					data, _ := json.Marshal(session)
					return maintenanceResponse(r, 201, string(data)), nil
				case "/api/sessions/" + maintenanceTestSessionID + "/input":
					inputs++
					if rec.Phase != "input" || rec.Job.SessionID != maintenanceTestSessionID || rec.Job.SessionCreatedAt == nil || !rec.Job.SessionCreatedAt.Equal(started) {
						t.Fatalf("input preceded exact ownership: %+v", rec)
					}
					if scenario == "input-unknown" {
						return nil, io.ErrUnexpectedEOF
					}
					return maintenanceResponse(r, 204, ""), nil
				default:
					t.Fatal("unexpected request", r.Method, r.URL.Path)
					return nil, errors.New("unexpected")
				}
			})}
			s.runMaintenanceJob(job, client, false, func(context.Context) error {
				rec := maintenanceRecordFor(t, s, job)
				if rec.Phase != "accepted" {
					t.Fatal("preparation preceded intent")
				}
				return nil
			})
			rec := maintenanceRecordFor(t, s, job)
			if creates != 1 || inputs > 1 {
				t.Fatal("mutation repeated", creates, inputs)
			}
			if scenario == "success" {
				if rec.Phase != "observing" || rec.Job.Status != "running" || inputs != 1 {
					t.Fatalf("not observing: %+v", rec)
				}
			} else if rec.Job.Status != "uncertain" || rec.Phase != "done" {
				t.Fatalf("uncertain outcome retried or hidden: %+v", rec)
			}
			recovered, err := openMaintenanceStore(s.config.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := recovered.get(job.ID)
			if r.Job.Status != rec.Job.Status {
				t.Fatal("status was not durable", r, rec)
			}
		})
	}
}

func TestMaintenanceRecoveryDoesNotRepeatUncertainCreateInputOrDelete(t *testing.T) {
	for _, phase := range []string{"accepted", "creating", "input", "observing", "deleting"} {
		t.Run(phase, func(t *testing.T) {
			s := maintenanceUnitServer(t)
			job := maintenanceTestJob(t, s)
			at := time.Now().UTC()
			if err := s.maintenance.update(job.ID, func(rec *maintenanceRecord) {
				rec.Phase = phase
				rec.Job.Status = "running"
				if phase == "accepted" {
					rec.Job.Status = "preparing"
				}
				if phase == "creating" || phase == "input" {
					rec.Job.Status = "starting"
				}
				if phase == "input" || phase == "observing" || phase == "deleting" {
					rec.Job.SessionID = maintenanceTestSessionID
					rec.Job.SessionCreatedAt = &at
				}
			}); err != nil {
				t.Fatal(err)
			}
			recovered, err := openMaintenanceStore(s.config.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			rec, _ := recovered.get(job.ID)
			switch phase {
			case "accepted":
				if rec.Job.Status != "failed" || rec.Phase != "done" {
					t.Fatal(rec)
				}
			case "creating", "input":
				if rec.Job.Status != "uncertain" || rec.Phase != "done" {
					t.Fatal(rec)
				}
			default:
				if rec.Phase != phase {
					t.Fatal("lost safe observation phase", rec)
				}
			}
		})
	}
}

// The fake runtime deliberately includes an unrelated setup session;
// only the explicit persisted identity may be removed.
type maintenanceRuntimeFixture struct {
	mu                       sync.Mutex
	sessions                 []maintenanceSession
	creates, inputs, deletes int
	deleteMode               string
	gate                     chan struct{}
}

func (f *maintenanceRuntimeFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/harnesses":
		writeJSON(w, 200, []map[string]any{{"id": "codex", "management": map[string]any{"supportedActions": []string{"update", "repair"}}}})
	case r.Method == "POST" && r.URL.Path == "/api/harnesses/codex/update":
		f.creates++
		if f.gate != nil {
			f.mu.Unlock()
			<-f.gate
			f.mu.Lock()
		}
		session := maintenanceSession{ID: maintenanceTestSessionID, CreatedAt: time.Now().UTC(), Status: "running"}
		f.sessions = append(f.sessions, session)
		writeJSON(w, 201, session)
	case r.Method == "GET" && r.URL.Path == "/api/sessions":
		writeJSON(w, 200, f.sessions)
	case r.Method == "DELETE":
		f.deletes++
		if f.deleteMode != "retain" {
			for i, session := range f.sessions {
				if r.URL.Path == "/api/sessions/"+session.ID {
					f.sessions = append(f.sessions[:i], f.sessions[i+1:]...)
					break
				}
			}
		}
		if f.deleteMode != "" {
			writeError(w, 500, "uncertain removal")
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 404, "unexpected fixture request")
	}
}
func (f *maintenanceRuntimeFixture) finish(code *int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.sessions {
		if f.sessions[i].ID == maintenanceTestSessionID {
			f.sessions[i].Status = status
			f.sessions[i].ExitCode = code
		}
	}
}
func attachMaintenanceFixture(s *Server, endpoint string) {
	address := strings.TrimPrefix(endpoint, "http://")
	s.mu.Lock()
	s.links["local"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
	s.mu.Unlock()
}
func maintenanceHTTPServer(t *testing.T, dir, endpoint string) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{StateDir: dir, Version: "fixture", Address: ln.Addr().String(), LocalSocket: "fixture"})
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	attachMaintenanceFixture(s, endpoint)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = ln
	ts.Start()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	// The fixture has no asset handler: redeem still creates the session cookie
	// even though following its final '/' redirect ends at a 404.
	response, err := client.Get(s.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	t.Cleanup(func() { s.Close(); ts.Close() })
	return s, ts, client
}
func waitMaintenance(t *testing.T, s *Server, id, status string) MaintenanceJob {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := s.maintenance.get(id); ok && rec.Job.Status == status {
			return rec.Job
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, _ := s.maintenance.get(id)
	t.Fatalf("wanted %s: %+v", status, rec)
	return MaintenanceJob{}
}
func TestMaintenanceBackgroundJobSurvivesBrowserAndControllerRestartAndRemovesOnlyOwnedSuccess(t *testing.T) {
	fixture := &maintenanceRuntimeFixture{gate: make(chan struct{}), sessions: []maintenanceSession{{ID: "ffffffffffffffffffffffffffffffff", CreatedAt: time.Now().UTC(), Status: "exited", ExitCode: new(int)}}}
	runtime := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer runtime.Close()
	dir := t.TempDir()
	s, ts, client := maintenanceHTTPServer(t, dir, runtime.URL)
	response := request(t, client, "POST", ts.URL+"/api/hosts/local/harnesses/codex/update", "", s.csrf, ts.URL)
	if response.StatusCode != 202 {
		t.Fatal("acceptance waited for worker", response.StatusCode)
	}
	var job MaintenanceJob
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// The request has ended and its context is cancelled before the worker returns.
	close(fixture.gate)
	running := waitMaintenance(t, s, job.ID, "running")
	if running.SessionID != maintenanceTestSessionID || running.SessionCreatedAt == nil {
		t.Fatal(running)
	}
	duplicate := request(t, client, "POST", ts.URL+"/api/hosts/local/harnesses/codex/update", "", s.csrf, ts.URL)
	if duplicate.StatusCode != 409 {
		t.Fatal("duplicate accepted", duplicate.StatusCode)
	}
	s.Close()
	ts.Close()
	fixture.finish(new(int), "exited") // PTY finishes while no controller or browser exists.
	restarted, ts2, client2 := maintenanceHTTPServer(t, dir, runtime.URL)
	done := waitMaintenance(t, restarted, job.ID, "succeeded")
	if done.CleanupStatus != "removed" {
		t.Fatal(done)
	}
	fixture.mu.Lock()
	if fixture.creates != 1 || fixture.deletes != 1 || len(fixture.sessions) != 1 || fixture.sessions[0].ID != strings.Repeat("f", 32) {
		t.Fatal("wrong session mutated", fixture.creates, fixture.deletes, fixture.sessions)
	}
	fixture.mu.Unlock()
	state := request(t, client2, "GET", ts2.URL+"/api/state", "", "", ts2.URL)
	var payload struct {
		Jobs []MaintenanceJob `json:"maintenanceJobs"`
	}
	if err := json.NewDecoder(state.Body).Decode(&payload); err != nil || len(payload.Jobs) != 1 || payload.Jobs[0].Status != "succeeded" {
		t.Fatal("durable state missing from new client", payload, err)
	}
}

func TestMaintenanceCleanupRejectsFailedUnknownReplacedAndUncertainSessions(t *testing.T) {
	for _, scenario := range []string{"nonzero", "unknown-exit", "interrupted", "replaced", "absent", "running", "delete-lost-removed", "delete-lost-retained", "restart-deleting-retained"} {
		t.Run(scenario, func(t *testing.T) {
			s := maintenanceUnitServer(t)
			job := maintenanceTestJob(t, s)
			at := time.Now().UTC()
			zero := 0
			session := maintenanceSession{ID: maintenanceTestSessionID, CreatedAt: at, Status: "exited", ExitCode: &zero}
			switch scenario {
			case "nonzero":
				code := 42
				session.ExitCode = &code
			case "unknown-exit":
				session.ExitCode = nil
			case "interrupted":
				session.Status = "interrupted"
			case "replaced":
				session.CreatedAt = at.Add(time.Second)
			case "running":
				session.Status = "running"
			}
			fixture := &maintenanceRuntimeFixture{sessions: []maintenanceSession{session}}
			if scenario == "absent" {
				fixture.sessions = nil
			}
			if scenario == "delete-lost-removed" {
				fixture.deleteMode = "remove"
			}
			if scenario == "delete-lost-retained" {
				fixture.deleteMode = "retain"
			}
			runtime := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer runtime.Close()
			attachMaintenanceFixture(s, runtime.URL)
			if err := s.maintenance.update(job.ID, func(rec *maintenanceRecord) {
				rec.Phase = "observing"
				rec.Job.Status = "running"
				rec.Job.SessionID = maintenanceTestSessionID
				rec.Job.SessionCreatedAt = &at
				if scenario == "restart-deleting-retained" {
					rec.Phase = "deleting"
				}
			}); err != nil {
				t.Fatal(err)
			}
			s.reconcileMaintenance(maintenanceRecordFor(t, s, job))
			rec := maintenanceRecordFor(t, s, job)
			if strings.HasPrefix(scenario, "delete-lost-") {
				if rec.Phase != "deleting" {
					t.Fatal("unknown deletion wasn't retained", rec)
				}
				// Crash and recover before observing the uncertain deletion result.
				reopened, err := openMaintenanceStore(s.config.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				s.maintenance = reopened
				s.reconcileMaintenance(maintenanceRecordFor(t, s, job))
				rec = maintenanceRecordFor(t, s, job)
				if fixture.deletes != 1 {
					t.Fatal("repeated uncertain removal", fixture.deletes)
				}
				if scenario == "delete-lost-removed" && rec.Job.Status != "succeeded" {
					t.Fatal(rec)
				}
				if scenario == "delete-lost-retained" && rec.Job.CleanupStatus != "uncertain" {
					t.Fatal(rec)
				}
			} else if fixture.deletes != 0 {
				t.Fatal("unproven cleanup", scenario)
			}
			if scenario == "nonzero" || scenario == "interrupted" {
				if rec.Job.Status != "failed" {
					t.Fatal(rec)
				}
			}
			if scenario == "running" && rec.Job.Status != "running" {
				t.Fatal(rec)
			}
		})
	}
}

func TestMaintenanceOwnershipIncludesSavedHostIdentityAndStorageFailureStopsMutation(t *testing.T) {
	s := maintenanceUnitServer(t)
	host := Host{ID: "host", Target: "original", Port: 22, CreatedAt: time.Now().UTC(), Status: "online"}
	if err := s.store.add(host); err != nil {
		t.Fatal(err)
	}
	job, err := s.maintenance.add(host, "claude", "repair")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := s.maintenance.update(job.ID, func(rec *maintenanceRecord) {
		rec.Phase = "observing"
		rec.Job.Status = "running"
		rec.Job.SessionID = maintenanceTestSessionID
		rec.Job.SessionCreatedAt = &at
	}); err != nil {
		t.Fatal(err)
	}
	s.store.mu.Lock()
	s.store.hosts[0].Target = "replacement"
	s.store.mu.Unlock()
	s.reconcileMaintenance(maintenanceRecordFor(t, s, job))
	if rec := maintenanceRecordFor(t, s, job); rec.Job.Status != "uncertain" {
		t.Fatal(rec)
	}
	// A durable transition failure must prevent even the session creation POST.
	localJob := maintenanceTestJob(t, s)
	s.maintenance.path = filepath.Join(s.config.StateDir, "missing-parent", "jobs.json")
	calls := 0
	client := &http.Client{Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not dispatch") })}
	s.runMaintenanceJob(localJob, client, false, func(context.Context) error { return nil })
	if calls != 0 {
		t.Fatal("mutation dispatched without durable intent")
	}
	if rec := maintenanceRecordFor(t, s, localJob); rec.Job.Status != "uncertain" {
		t.Fatal(rec)
	}
}

func TestMaintenanceHistoryCapNeverPrunesUnresolvedJobs(t *testing.T) {
	s := maintenanceUnitServer(t)
	for i := 0; i < maxMaintenanceJobs; i++ {
		job, err := s.maintenance.add(Host{ID: "local", Target: "fixture"}, "codex", "repair")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.maintenance.update(job.ID, func(rec *maintenanceRecord) {
			rec.Phase = "done"
			rec.Job.Status = "uncertain"
			rec.Job.CleanupStatus = "retained"
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.maintenance.add(Host{ID: "local", Target: "fixture"}, "codex", "repair"); err == nil {
		t.Fatal("discarded unresolved history")
	}
	first := s.maintenance.list()[0]
	if err := s.maintenance.update(first.ID, func(rec *maintenanceRecord) { rec.Job.Status = "succeeded"; rec.Job.CleanupStatus = "removed" }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.maintenance.add(Host{ID: "local", Target: "fixture"}, "codex", "repair"); err != nil {
		t.Fatal("couldn't prune resolved history", err)
	}
	if _, ok := s.maintenance.get(first.ID); ok {
		t.Fatal("didn't prune oldest safe job")
	}
}

func TestMaintenancePreparationCannotReviveAfterTransientPersistenceFailure(t *testing.T) {
	s := maintenanceUnitServer(t)
	job := maintenanceTestJob(t, s)
	calls := 0
	client := &http.Client{Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not dispatch") })}
	s.runMaintenanceJob(job, client, false, func(context.Context) error {
		original := s.maintenance.path
		s.maintenance.path = filepath.Join(s.config.StateDir, "missing-directory", "jobs.json")
		if s.maintenanceChange(job.ID, func(rec *maintenanceRecord) { rec.Job.Stage = "Downloading" }) {
			t.Fatal("expected persistence failure")
		}
		s.maintenance.path = original
		return nil
	})
	if calls != 0 || maintenanceRecordFor(t, s, job).Job.Status != "uncertain" {
		t.Fatal("revived failed preparation", calls, maintenanceRecordFor(t, s, job))
	}
}
func TestMaintenanceSessionListLimitAccommodatesValidLargeLists(t *testing.T) {
	s := maintenanceUnitServer(t)
	job := maintenanceTestJob(t, s)
	at := time.Now().UTC()
	sessions := []map[string]any{}
	for i := 0; i < 24; i++ {
		sessions = append(sessions, map[string]any{"id": fmt.Sprintf("%032x", i), "createdAt": at, "cwd": "/" + strings.Repeat("a/", 1500), "status": "running"})
	}
	sessions[0]["id"], sessions[0]["status"], sessions[0]["exitCode"] = maintenanceTestSessionID, "exited", 0
	body, _ := json.Marshal(sessions)
	if len(body) <= 64<<10 {
		t.Fatal("fixture too small")
	}
	deleted := 0
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
			return
		}
		if r.Method != "DELETE" || r.URL.Path != "/api/sessions/"+maintenanceTestSessionID {
			t.Error("unexpected mutation", r.Method, r.URL.Path)
		}
		deleted++
		w.WriteHeader(204)
	}))
	defer runtime.Close()
	attachMaintenanceFixture(s, runtime.URL)
	if err := s.maintenance.update(job.ID, func(rec *maintenanceRecord) {
		rec.Phase = "observing"
		rec.Job.Status = "running"
		rec.Job.SessionID = maintenanceTestSessionID
		rec.Job.SessionCreatedAt = &at
	}); err != nil {
		t.Fatal(err)
	}
	s.reconcileMaintenance(maintenanceRecordFor(t, s, job))
	if deleted != 1 || maintenanceRecordFor(t, s, job).Job.Status != "succeeded" {
		t.Fatal("valid large list prevented cleanup")
	}
	client := &http.Client{Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) { return maintenanceResponse(r, 200, string(body)), nil })}
	if _, err := runtimeJSON(context.Background(), client, "POST", "/api/sessions", map[string]string{}); err == nil {
		t.Fatal("mutation response cap expanded")
	}
}

func TestMaintenanceActiveJobsAreBoundedAndOldLocalRuntimeDoesNotStartAWorker(t *testing.T) {
	s := maintenanceUnitServer(t)
	for i := 0; i < maxActiveMaintenanceJobs; i++ {
		if _, err := s.maintenance.add(Host{ID: fmt.Sprintf("host-%d", i), Target: "fixture"}, "codex", "update"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.maintenance.add(Host{ID: "another-host", Target: "fixture"}, "codex", "update"); err == nil {
		t.Fatal("active job limit ignored")
	}
	first := s.maintenance.list()[0]
	s.maintenanceFinish(first.ID, "failed", "Stopped", "fixture", "retained")
	job := maintenanceTestJob(t, s)
	posts := 0
	client := &http.Client{Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			posts++
		}
		return maintenanceResponse(r, 200, `[{"id":"codex","installed":true}]`), nil
	})}
	s.runMaintenanceJob(job, client, true, func(ctx context.Context) error { return checkLocalMaintenance(ctx, client, "codex", "update") })
	rec := maintenanceRecordFor(t, s, job)
	if posts != 0 || rec.Job.Status != "failed" || rec.Job.SessionID != "" || !strings.Contains(rec.Job.Error, "local runtime") {
		t.Fatal("unsupported local worker started", posts, rec)
	}
}
