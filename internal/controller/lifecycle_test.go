package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

func TestSavedHostStartupReconnectIsBoundedIdempotentAndCancellable(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := store.add(Host{ID: fmt.Sprintf("host-%d", i), Target: fmt.Sprintf("host-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 10)
	var active, maximum atomic.Int32
	s, err := New(Config{StateDir: dir, Address: "127.0.0.1:7340", startConnection: func(ctx context.Context, cfg transport.Config) (*transport.Connection, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- cfg.Target
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("saved hosts did not reconnect on startup")
		}
	}
	var callers sync.WaitGroup
	for i := 0; i < 8; i++ {
		callers.Go(func() {
			if queued := s.ReconnectSaved(); queued != 0 {
				t.Errorf("duplicate launch queued %d hosts", queued)
			}
		})
	}
	callers.Wait()
	if len(s.store.list()) != 10 || maximum.Load() != 4 || len(started) != 0 {
		t.Fatal("unbounded or duplicate connect attempts", maximum.Load(), len(started))
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("controller shutdown did not cancel queued authentication")
	}
	if active.Load() != 0 || s.ReconnectSaved() != 0 {
		t.Fatal("shutdown left connecting workers or restarted hosts")
	}
}

func TestReconnectAndRepairBrowserGuardsAndStaleOperation(t *testing.T) {
	s, ts, client := testController(t, "")
	login(t, s, client)
	if response := request(t, client, "POST", ts.URL+"/api/reconnect", "", "", ts.URL); response.StatusCode != http.StatusForbidden {
		t.Fatal("reconnect accepted without CSRF", response.StatusCode)
	}
	if response := request(t, client, "POST", ts.URL+"/api/reconnect", "", s.csrf, ts.URL); response.StatusCode != http.StatusAccepted {
		t.Fatal("reconnect endpoint", response.StatusCode)
	}
	if err := s.store.add(Host{ID: "test", Target: "unused", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	current := &link{}
	s.mu.Lock()
	s.links["test"] = current
	s.mu.Unlock()
	if err := s.beginConnect(Host{ID: "test"}); err != nil {
		t.Fatal("connect not idempotent", err)
	}
	for _, check := range []struct {
		id, csrf string
		status   int
	}{
		{"test", "", 403}, {"missing", s.csrf, 404}, {"local", s.csrf, 400}, {"test", s.csrf, 503},
	} {
		response := request(t, client, "POST", ts.URL+"/api/hosts/"+check.id+"/repair-runtime", "", check.csrf, ts.URL)
		if response.StatusCode != check.status {
			t.Fatalf("repair %s: %d", check.id, response.StatusCode)
		}
	}
	s.repairStage("test", &link{}, &RuntimeOperation{Status: "completed"})
	host, _ := s.store.get("test")
	if host.RuntimeOperation != nil {
		t.Fatal("stale repair changed new connection")
	}
	s.repairStage("test", current, &RuntimeOperation{Status: "completed", InstalledVersion: "new", RunningVersion: "old", RestartRequired: true})
	host, _ = s.store.get("test")
	if host.Status != "online" || host.RuntimeOperation == nil || !host.RuntimeOperation.RestartRequired {
		t.Fatal("repair hid online sessions or lost deferred update status", host)
	}
	s.store.update("test", "disconnected", "Disconnected", "")
	if host, _ = s.store.get("test"); host.RuntimeOperation != nil {
		t.Fatal("disconnected host retained active repair")
	}
	if _, err := s.queueConnection(Host{ID: "missing"}); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("missing host was reconnected", err)
	}
}
