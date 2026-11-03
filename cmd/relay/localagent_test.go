package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/TwoD97/relay/internal/localagent"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalAgentConcurrentEnsureSurvivesLaunchers(t *testing.T) {
	source := os.Getenv("RELAY_WINDOWS_TEST_BINARY")
	if runtime.GOOS == "windows" && source == "" {
		t.Skip("set RELAY_WINDOWS_TEST_BINARY to the native Relay executable")
	}
	dir, err := os.MkdirTemp("", "relay-la-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if source == "" {
		source = filepath.Join(dir, "source-relay")
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", source, ".")
		if output, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("build: %v: %s", e, output)
		}
	}
	installed := filepath.Join(dir, "installed-relay")
	if runtime.GOOS == "windows" {
		installed += ".exe"
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(installed, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		in.Close()
		t.Fatal(err)
	}
	_, err = io.Copy(out, in)
	in.Close()
	out.Close()
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	var mu sync.Mutex
	var results []localagent.Health
	var failures []string
	var group sync.WaitGroup
	for range 6 {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, installed, "local-agent-ensure", "--state-dir", state)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			data, e := cmd.Output()
			var result localagent.Health
			if e == nil {
				e = json.Unmarshal(data, &result)
			}
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				failures = append(failures, e.Error()+": "+stderr.String())
			} else {
				results = append(results, result)
			}
		}()
	}
	group.Wait()
	// Every PID came from this test's private same-user endpoint. Pin before
	// cleanup so a recycled numeric PID can never authorize a later signal.
	processes := map[int]*os.Process{}
	for _, result := range results {
		if processes[result.PID] == nil {
			p, e := os.FindProcess(result.PID)
			if e == nil {
				processes[result.PID] = p
			}
		}
	}
	defer func() {
		for _, p := range processes {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	}()
	if len(failures) > 0 {
		t.Fatalf("ensure failures: %v", failures)
	}
	if len(processes) != 1 {
		t.Fatalf("started %d daemons", len(processes))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	health, err := readLocalAgentHealth(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if health.PID != results[0].PID || !health.Capabilities["localMastermindV1"] {
		t.Fatal("daemon did not survive launcher exit")
	}
	if err = os.Rename(installed, installed+".replaced"); err != nil {
		t.Fatalf("persistent service locks installer executable: %v", err)
	}
	health, err = readLocalAgentHealth(ctx, state)
	if err != nil || health.PID != results[0].PID {
		t.Fatalf("service lost after executable replacement: %v", err)
	}
}

func TestLocalAgentHealthRejectsRedirect(t *testing.T) {
	dir, err := os.MkdirTemp("", "relay-la-health-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err = prepareControllerDirectory(dir); err != nil {
		t.Fatal(err)
	}
	listener, cleanup, err := listenLocalAgent(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "http://other-controller/api/health", http.StatusFound)
	})}
	defer server.Close()
	go server.Serve(listener)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = readLocalAgentHealth(ctx, dir); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls.Load() != 1 {
		t.Fatalf("health followed redirect %d times", calls.Load())
	}
}
