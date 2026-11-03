package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/machinestats"
	"github.com/TwoD97/relay/internal/transport"
)

func TestMachineStatsAuthorizationAndUnknownOfflineHosts(t *testing.T) {
	s, ts, client := testController(t, "")
	endpoint := ts.URL + "/api/hosts/missing/stats"
	if r := request(t, client, "GET", endpoint, "", "", ""); r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	login(t, s, client)
	if r := request(t, client, "GET", endpoint, "", "", "https://other.example"); r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if r := request(t, client, "GET", endpoint, "", "", ts.URL); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
	if r := request(t, client, "GET", ts.URL+"/api/hosts/local/stats", "", "", ts.URL); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
	h := Host{ID: randomID(), Name: "Offline fixture", Target: "fixture.invalid", Port: 22, Status: "disconnected"}
	if err := s.store.add(h); err != nil {
		t.Fatal(err)
	}
	if r := request(t, client, "GET", ts.URL+"/api/hosts/"+h.ID+"/stats", "", "", ts.URL); r.StatusCode != 503 {
		t.Fatal(r.StatusCode)
	}
	if r := request(t, client, "GET", endpoint+"?command=whoami", "", "", ts.URL); r.StatusCode != 400 {
		t.Fatal(r.StatusCode)
	}
}

func TestMachineStatsCacheDoesNotNeedRuntimeAndResetsOnConnectionChange(t *testing.T) {
	s, ts, client := testController(t, filepath.Join(t.TempDir(), "absent.sock"))
	login(t, s, client)
	var count atomic.Int32
	s.statsCollector = func(ctx context.Context, connection *transport.Connection) (machinestats.Sample, error) {
		if connection != nil {
			t.Error("local collector used SSH")
		}
		count.Add(1)
		sample := machinestats.NewSample("linux")
		sample.Counter = &machinestats.Counter{Total: 100, Idle: 50, Cores: 4}
		sample.Stats.Memory = &machinestats.Memory{TotalBytes: 100, AvailableBytes: 40, UsedBytes: 60}
		return sample, nil
	}
	endpoint := ts.URL + "/api/hosts/local/stats"
	for range 3 {
		r := request(t, client, "GET", endpoint, "", "", ts.URL)
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
		var result machinestats.Stats
		if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.CPU.UsagePercent != nil || result.Memory.UsedBytes != 60 || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("unexpected response", result)
		}
	}
	if count.Load() != 1 {
		t.Fatal("cache bypass", count.Load())
	}
	s.mu.Lock()
	s.links["local"] = &link{}
	s.mu.Unlock()
	if r := request(t, client, "GET", endpoint, "", "", ts.URL); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if count.Load() != 2 {
		t.Fatal("new connection reused prior identity cache")
	}
}

func TestMachineStatsTimeoutFailureAndChangedConnection(t *testing.T) {
	for _, kind := range []string{"timeout", "failure", "changed"} {
		t.Run(kind, func(t *testing.T) {
			s, ts, client := testController(t, filepath.Join(t.TempDir(), "absent.sock"))
			login(t, s, client)
			s.statsTimeout = 20 * time.Millisecond
			s.statsCollector = func(ctx context.Context, _ *transport.Connection) (machinestats.Sample, error) {
				switch kind {
				case "timeout":
					<-ctx.Done()
					return machinestats.Sample{}, ctx.Err()
				case "failure":
					return machinestats.Sample{}, errors.New("private diagnostic must not be exposed")
				default:
					s.mu.Lock()
					s.links["local"] = &link{}
					s.mu.Unlock()
					return machinestats.NewSample("linux"), nil
				}
			}
			want := map[string]int{"timeout": 504, "failure": 502, "changed": 503}[kind]
			if r := request(t, client, "GET", ts.URL+"/api/hosts/local/stats", "", "", ts.URL); r.StatusCode != want {
				t.Fatal(r.StatusCode)
			}
		})
	}
}
