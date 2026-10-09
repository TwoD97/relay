package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMissingControllerCannotStartDuringHandover(t *testing.T) {
	dir := shortDesktopDirectory(t)
	marker := filepath.Join(dir, "unexpected-start")
	binary := filepath.Join(dir, "fixture")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf touched > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	opts := uiOptions{StateDir: filepath.Join(dir, "state"), RuntimeDir: filepath.Join(dir, "runtime"), Executable: binary, BinaryDir: dir, Address: "127.0.0.1:0"}
	lock, err := acquireHandoverLock(context.Background(), opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := ensureController(ctx, opts); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("missing-controller launcher did not wait for in-progress handover", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launcher started a competing controller during handover")
	}
}

func TestControllerProbeRejectsFalsePIDWithoutStoppingPeer(t *testing.T) {
	dir := shortDesktopDirectory(t)
	if err := prepareControllerDirectory(dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", controllerSocket(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(controllerHealth{controllerInfo: controllerInfo{PID: os.Getpid() + 1, Version: "0.1.0-old", Address: "http://127.0.0.1:43210", Protocol: 1}})
	})}
	defer server.Close()
	go server.Serve(listener)
	if _, err := probeController(context.Background(), uiOptions{StateDir: dir}); err == nil || !strings.Contains(err.Error(), "PID") {
		t.Fatal("false private PID accepted", err)
	}
	// The peer is this test process. A successful follow-up read proves the
	// identity rejection did not attempt a process or connection-tree stop.
	var h controllerHealth
	if err := controlRequest(context.Background(), dir, http.MethodGet, "/health", &h); err != nil {
		t.Fatal("sentinel was disturbed", err)
	}
}

func TestControllerUpgradePreservesLocalDaemonAndSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two disposable controller versions")
	}
	dir := shortDesktopDirectory(t)
	build := func(name, v string) string {
		t.Helper()
		target := filepath.Join(dir, name)
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-ldflags", "-X main.version="+v, "-o", target, ".")
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build controller: %v %s", err, data)
		}
		return target
	}
	oldBinary := build("old", "0.1.0-handover-old")
	newBinary := build("new", version)
	runtimeDir, stateDir := filepath.Join(dir, "runtime"), filepath.Join(dir, "state")
	daemon := exec.Command(oldBinary, "daemon", "--state-dir", runtimeDir)
	daemon.Env = append(os.Environ(), "SHELL=/bin/sh", "ENV=")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = daemon.Process.Kill()
			<-done
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for {
		if _, err := readHealth(ctx, runtimeDir); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(20 * time.Millisecond)
	}
	opts := uiOptions{Address: "127.0.0.1:0", StateDir: stateDir, RuntimeDir: runtimeDir, BinaryDir: dir, Executable: newBinary, Local: true}
	oldCommand := exec.CommandContext(ctx, oldBinary, "desktop", "--state-dir", stateDir, "--runtime-dir", runtimeDir, "--binaries", dir)
	data, err := oldCommand.Output()
	if err != nil {
		t.Fatal("old desktop", err)
	}
	var old desktopLaunch
	if err = json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p, e := probeController(context.Background(), opts); e == nil {
			defer p.Close()
			if err := stopHandoverTestController(p, opts); err != nil {
				t.Error("test controller cleanup", err)
			}
		}
	})
	callSession := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, newBinary, append([]string{"session"}, args...)...)
		cmd.Env = append(os.Environ(), "RELAY_SOCKET="+socketPath(runtimeDir))
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("session CLI: %v %s", err, data)
		}
		return data
	}
	var session struct{ ID string }
	if err := json.Unmarshal(callSession("start", "--cwd", dir, "--title", "Upgrade sentinel"), &session); err != nil {
		t.Fatal(err)
	}
	callSession("send", "--id", session.ID, "--text", "export RELAY_HANDOVER_SENTINEL=preserved", "--enter")
	probe, err := probeController(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*controllerHealth, *uiOptions){
		func(h *controllerHealth, o *uiOptions) { h.PID++ },
		func(h *controllerHealth, o *uiOptions) { h.Executable = newBinary },
		func(h *controllerHealth, o *uiOptions) { h.Version = "0.1.0-wrong" },
		func(h *controllerHealth, o *uiOptions) { o.StateDir = dir },
		func(h *controllerHealth, o *uiOptions) { h.RuntimeDir = dir },
	} {
		h, o := probe.health, opts
		mutate(&h, &o)
		if err := probe.process.Verify(ctx, h, o); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	probe.Close()
	var workers sync.WaitGroup
	type result struct {
		launch desktopLaunch
		err    error
	}
	results := make(chan result, 8)
	for range 8 {
		workers.Go(func() { l, e := ensureController(ctx, opts); results <- result{l, e} })
	}
	workers.Wait()
	close(results)
	pid := 0
	for result := range results {
		if result.err != nil {
			t.Fatal("upgrade", result.err)
		}
		if pid == 0 {
			pid = result.launch.PID
		}
		if result.launch.PID != pid || pid == old.PID || result.launch.Address != old.Address || result.launch.Version != version {
			t.Fatal("concurrent upgrade changed identity or origin")
		}
	}
	if err := daemon.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("upgrade stopped local daemon", err)
	}
	health, err := readHealth(ctx, runtimeDir)
	if err != nil || health.Version != "0.1.0-handover-old" {
		t.Fatal("upgrade replaced local daemon", health, err)
	}
	if !strings.Contains(string(callSession("list")), session.ID) {
		t.Fatal("upgrade lost original session")
	}
	callSession("send", "--id", session.ID, "--text", "printf 'UPGRADE_%s\\n' \"$RELAY_HANDOVER_SENTINEL\"", "--enter")
	for {
		if strings.Contains(string(callSession("read", "--id", session.ID)), "UPGRADE_preserved") {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("original shell state did not survive")
		}
		time.Sleep(20 * time.Millisecond)
	}
	callSession("stop", "--id", session.ID)
}
