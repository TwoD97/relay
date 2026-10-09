//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/controller"
)

func shortDesktopDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "relay-desktop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRuntimeCompatibilityPreservesOlderProtocolOneSessions(t *testing.T) {
	for _, runtimeVersion := range []string{"0.1.0-dev.a736abcdef01", "0.1.0", version} {
		if err := compatible(health{Version: runtimeVersion, Protocol: 1}); err != nil {
			t.Fatalf("compatible runtime %q rejected: %v", runtimeVersion, err)
		}
	}
	for _, h := range []health{{Version: version, Protocol: 2}, {Version: version, Protocol: 0}, {Version: "", Protocol: 1}, {Version: "invalid\nversion", Protocol: 1}, {Version: strings.Repeat("a", 102), Protocol: 1}} {
		if err := compatible(h); err == nil {
			t.Fatalf("unsupported runtime identity accepted: %+v", h)
		}
	}
}

// The child uses the real token issuer and private handler, with the token pool
// already occupied by other launch requests when the first health poll arrives.
func TestDesktopTokenCapacityHelper(t *testing.T) {
	if os.Getenv("RELAY_TEST_DESKTOP_CAPACITY") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) && os.Args[i+1] == "ui" {
			args = os.Args[i+2:]
			break
		}
	}
	opts, err := parseUIOptions("ui", args)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockControllerState(opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	tcp, err := net.Listen("tcp", opts.Address)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	app, err := controller.New(controller.Config{StateDir: opts.StateDir, Address: tcp.Addr().String(), Version: version})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for {
		if _, err := app.IssueLoginURL(); err != nil {
			break
		}
	}
	listener, err := net.Listen("unix", controllerSocket(opts.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(controllerSocket(opts.StateDir), 0600); err != nil {
		t.Fatal(err)
	}
	info := controllerInfo{Address: "http://" + tcp.Addr().String(), Version: version, Protocol: controllerProtocol, PID: os.Getpid()}
	server := &http.Server{Handler: controllerControlHandler(app, info, opts), ReadHeaderTimeout: time.Second}
	if err := server.Serve(listener); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopRejectedLaunchPreservesReadyChild(t *testing.T) {
	dir := shortDesktopDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_TEST_EXECUTABLE", executable)
	t.Setenv("RELAY_TEST_DESKTOP_CAPACITY", "1")
	wrapper := filepath.Join(dir, "controller-fixture")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$RELAY_TEST_EXECUTABLE\" -test.run=^TestDesktopTokenCapacityHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	opts := uiOptions{Address: "127.0.0.1:0", StateDir: filepath.Join(dir, "ui"), RuntimeDir: filepath.Join(dir, "unused"), BinaryDir: dir, Executable: wrapper}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = ensureController(ctx, opts)
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("expected exhausted one-time token pool: %v", err)
	}
	info, err := readControllerInfo(ctx, opts)
	if err != nil {
		t.Fatal("rejected login killed the already-ready controller:", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(info.PID, syscall.SIGTERM) })
	if err := syscall.Kill(info.PID, 0); err != nil {
		t.Fatal("ready controller exited after its owner's launch was rejected", err)
	}
	if _, err := lockControllerState(opts.StateDir); !errors.Is(err, errControllerRunning) {
		t.Fatal("ready controller released shared state after a token rejection")
	}
}

func TestControllerStateHasOnePrivateOwner(t *testing.T) {
	dir := shortDesktopDirectory(t)
	first, err := lockControllerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockControllerState(dir); !errors.Is(err, errControllerRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second writer acquired controller state: %v", err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "run")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory permissions: %s %v", path, err)
		}
	}
	first.Close()
	second, err := lockControllerState(dir)
	if err != nil {
		t.Fatal("owner exit did not release lock", err)
	}
	second.Close()
	lockPath := filepath.Join(dir, "run", "controller.lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "untouched")
	if err := os.WriteFile(target, []byte("private"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	if file, err := lockControllerState(dir); err == nil {
		file.Close()
		t.Fatal("controller followed a lock symlink")
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0644 {
		t.Fatal("lock symlink target was modified")
	}
}

func TestDesktopConcurrentLaunchersReuseControllerAndRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a disposable real executable and starts isolated local processes")
	}
	dir := shortDesktopDirectory(t)
	binary := filepath.Join(dir, "relay")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-race", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build desktop fixture: %v: %s", err, output)
	}
	runtimeDir, stateDir := filepath.Join(dir, "runtime"), filepath.Join(dir, "client")
	daemon := exec.Command(binary, "daemon", "--state-dir", runtimeDir)
	daemon.Env = append(os.Environ(), "SHELL=/bin/sh", "ENV=")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-daemonDone:
		case <-time.After(5 * time.Second):
			_ = daemon.Process.Kill()
			<-daemonDone
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if _, err := readHealth(ctx, runtimeDir); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("test-owned runtime did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	opts := uiOptions{Address: "127.0.0.1:0", StateDir: stateDir, RuntimeDir: runtimeDir, BinaryDir: dir, Executable: binary, Local: true}
	type result struct {
		launch desktopLaunch
		err    error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			launch, err := ensureController(ctx, opts)
			results <- result{launch, err}
		})
	}
	workers.Wait()
	close(results)
	var launches []desktopLaunch
	ownedPIDs := map[int]bool{}
	t.Cleanup(func() {
		for pid := range ownedPIDs {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Lstat(controllerSocket(stateDir)); errors.Is(err, os.ErrNotExist) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		for pid := range ownedPIDs {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	for result := range results {
		if result.err != nil {
			t.Error("concurrent desktop launch:", result.err)
			continue
		}
		ownedPIDs[result.launch.PID] = true
		launches = append(launches, result.launch)
	}
	if len(launches) != 8 || len(ownedPIDs) != 1 {
		t.Fatalf("launches=%d controllers=%d", len(launches), len(ownedPIDs))
	}
	first := launches[0]
	seenURLs := map[string]bool{}
	for _, launch := range launches {
		if launch.controllerInfo != first.controllerInfo || seenURLs[launch.URL] {
			t.Fatal("launcher changed identity or reused another launch capability")
		}
		seenURLs[launch.URL] = true
	}
	for _, name := range []string{"run/controller.sock", "run/controller.lock", "controller.log"} {
		info, err := os.Stat(filepath.Join(stateDir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("controller file permissions: %s %v", name, err)
		}
	}
	client := func(link string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, Timeout: 3 * time.Second}
		res, err := c.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("login returned %d", res.StatusCode)
		}
		return c
	}
	firstClient := client(first.URL)
	secondClient := client(launches[1].URL)
	for _, c := range []*http.Client{firstClient, secondClient} {
		res, err := c.Get(first.Address + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !bytes.Contains(body, []byte(`"id":"local"`)) {
			t.Fatal("browser clients did not share the existing local runtime")
		}
	}
	// The real short-lived CLI must emit only its JSON handshake, reuse the
	// existing controller, and leave both authenticated clients working.
	command := exec.Command(binary, "desktop", "--state-dir", stateDir, "--runtime-dir", runtimeDir, "--binaries", dir)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("desktop command: %v: %s", err, stderr.Bytes())
	}
	var cliLaunch desktopLaunch
	if err := json.Unmarshal(output, &cliLaunch); err != nil || cliLaunch.controllerInfo != first.controllerInfo || seenURLs[cliLaunch.URL] {
		t.Fatal("desktop command did not return one fresh JSON launch for the same controller")
	}
	if _, err := readHealth(ctx, runtimeDir); err != nil || daemon.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("desktop helper exit stopped the existing runtime")
	}
	if _, err := lockControllerState(stateDir); !errors.Is(err, errControllerRunning) {
		t.Fatal("controller stopped owning its state after desktop helper exited")
	}
	conflict := opts
	conflict.Local = false
	if _, err := ensureController(ctx, conflict); err == nil || !strings.Contains(err.Error(), "different --local") {
		t.Fatal("conflicting local runtime configuration silently reused another controller")
	}
	conflict = opts
	conflict.RuntimeDir = filepath.Join(dir, "wrong-runtime")
	if _, err := ensureController(ctx, conflict); err == nil || !strings.Contains(err.Error(), "different --local") {
		t.Fatal("conflicting runtime path silently reused another controller")
	}
	log, err := os.ReadFile(filepath.Join(stateDir, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, launch := range append(launches, cliLaunch) {
		u, _ := url.Parse(launch.URL)
		if bytes.Contains(log, []byte("token=")) || bytes.Contains(log, []byte(u.Query().Get("token"))) {
			t.Fatal("desktop controller log contains a login capability")
		}
	}
}
