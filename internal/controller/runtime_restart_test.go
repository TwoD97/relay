package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restartFixture(t *testing.T, lost bool) (*Server, *httptest.Server, *http.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	s, ts, client := testController(t, "/isolated/restart/run/daemon.sock")
	var commits, ensures atomic.Int32
	var accepted atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Relay-CSRF") != "" {
			t.Error("forwarded browser authorization")
		}
		switch r.URL.Path {
		case "/api/recovery/preflight":
			writeJSON(w, 200, map[string]any{"runtimeId": strings.Repeat("a", 32), "instanceId": strings.Repeat("b", 32), "fingerprint": strings.Repeat("c", 64), "blockers": []string{}, "entries": []any{}})
		case "/api/recovery/start":
			commits.Add(1)
			accepted.Store(true)
			if lost {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			var request map[string]string
			json.NewDecoder(r.Body).Decode(&request)
			writeJSON(w, 202, map[string]string{"id": request["id"], "runtimeId": strings.Repeat("a", 32), "fromInstance": strings.Repeat("b", 32)})
		case "/api/health":
			if accepted.Load() {
				w.WriteHeader(503)
			} else {
				writeJSON(w, 200, map[string]string{"runtimeId": strings.Repeat("a", 32), "instanceId": strings.Repeat("b", 32)})
			}
		case "/api/recovery":
			writeJSON(w, 200, map[string]any{"operation": nil})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	s.mu.Lock()
	s.links["local"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	})
	s.config.EnsureLocalRuntime = func(context.Context) error { ensures.Add(1); return nil }
	s.mu.Unlock()
	login(t, s, client)
	return s, ts, client, &commits, &ensures
}
func TestRuntimeRestartStagesOneCommitAndDoesNotForwardBrowserCredentials(t *testing.T) {
	s, ts, client, commits, ensures := restartFixture(t, false)
	path := ts.URL + "/api/hosts/local/recovery/start"
	body := `{"id":"` + strings.Repeat("d", 32) + `","fingerprint":"` + strings.Repeat("c", 64) + `"}`
	bad := request(t, client, "POST", path, body, "", ts.URL)
	bad.Body.Close()
	if bad.StatusCode != 403 {
		t.Fatal(bad.StatusCode)
	}
	good := request(t, client, "POST", path, body, s.csrf, ts.URL)
	good.Body.Close()
	if good.StatusCode != 202 {
		t.Fatal(good.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ensures.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if commits.Load() != 1 || ensures.Load() != 1 {
		t.Fatalf("commit=%d ensure=%d", commits.Load(), ensures.Load())
	}
	repeated := request(t, client, "POST", path, body, s.csrf, ts.URL)
	repeated.Body.Close()
	if repeated.StatusCode != 409 {
		t.Fatal("repeated accepted mutation", repeated.StatusCode)
	}
	if commits.Load() != 1 {
		t.Fatal("shutdown was repeated")
	}
}
func TestRuntimeRestartLostAcceptanceNeverRetriesStopOrLaunch(t *testing.T) {
	s, ts, client, commits, ensures := restartFixture(t, true)
	response := request(t, client, "POST", ts.URL+"/api/hosts/local/recovery/start", `{"id":"`+strings.Repeat("e", 32)+`","fingerprint":"`+strings.Repeat("c", 64)+`"}`, s.csrf, ts.URL)
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatal(response.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := s.restarts.get("local")
		if rec.Job.Status == "uncertain" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, _ := s.restarts.get("local")
	if rec.Job.Status != "uncertain" || commits.Load() != 1 || ensures.Load() != 0 {
		t.Fatal("lost response repeated mutation", rec, commits.Load(), ensures.Load())
	}
	for i := 0; i < 3; i++ {
		resp := request(t, client, "GET", ts.URL+"/api/hosts/local/recovery", "", "", ts.URL)
		resp.Body.Close()
	}
	if commits.Load() != 1 || ensures.Load() != 0 {
		t.Fatal("read-only recovery inspection repeated mutation")
	}
	reopened := openRuntimeRestarts(s.config.StateDir)
	saved, err := reopened.get("local")
	if err != "" || saved.Job.Status != "uncertain" {
		t.Fatal("restart lost uncertain intent", saved, err)
	}
}
func TestRuntimeRestartRecoveryPhaseIsDurableAndDoesNotResumeMutations(t *testing.T) {
	dir := t.TempDir()
	store := openRuntimeRestarts(dir)
	for _, phase := range []string{"preparing", "committing", "observing"} {
		err := store.change(phase, func(rec *runtimeRestartRecord) {
			rec.Job = runtimeRestartJob{ID: strings.Repeat("a", 32), HostID: phase, RuntimeID: strings.Repeat("b", 32), FromInstance: strings.Repeat("c", 32), Status: phase}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	next := openRuntimeRestarts(dir)
	a, _ := next.get("preparing")
	b, _ := next.get("committing")
	c, _ := next.get("observing")
	if a.Job.Status != "failed" || b.Job.Status != "uncertain" || c.Job.Status != "observing" {
		t.Fatal("invalid phase recovery", a, b, c)
	}
}

func TestRuntimeRestartUncertainDiskCommitBlocksEveryLaterWrite(t *testing.T) {
	store := openRuntimeRestarts(t.TempDir())
	calls := 0
	store.write = func(path string, data []byte) error {
		calls++
		if err := writeRestartFile(path, data); err != nil {
			return err
		}
		return &restartStoreUncertainError{errors.New("simulated directory sync failure")}
	}
	err := store.change("host", func(rec *runtimeRestartRecord) {
		rec.Job = runtimeRestartJob{ID: strings.Repeat("a", 32), HostID: "host", RuntimeID: strings.Repeat("b", 32), FromInstance: strings.Repeat("c", 32), Status: "committing"}
	})
	if err == nil {
		t.Fatal("uncertain commit reported success")
	}
	if err = store.change("host", func(rec *runtimeRestartRecord) { rec.Job.Status = "preparing" }); err == nil || calls != 1 {
		t.Fatal("uncertain intent was overwritten/retried", err, calls)
	}
	reopened := openRuntimeRestarts(filepath.Dir(store.path))
	record, readErr := reopened.get("host")
	if readErr != "" || record.Job.Status != "uncertain" {
		t.Fatal("committed intent was forgotten", record, readErr)
	}
}
