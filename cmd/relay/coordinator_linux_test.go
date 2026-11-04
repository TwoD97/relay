package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCoordinatorRealPrepareBridgeReusesOwnedRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("disposable real controller/runtime lifecycle")
	}
	home, err := os.MkdirTemp("", "rc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	binary := filepath.Join(home, "relay")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-buildvcs=false", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v %s", err, output)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "HOME" && key != "ENV" && key != "BASH_ENV" && !strings.HasPrefix(key, "RELAY_") {
			env = append(env, entry)
		}
	}
	env = append(env, "HOME="+home, "SHELL=/bin/sh", "ENV=")
	runtimeDir := filepath.Join(home, ".local/share/relay")
	daemon := exec.Command(binary, "daemon", "--state-dir", runtimeDir)
	daemon.Env = env
	var daemonLog bytes.Buffer
	daemon.Stderr = &daemonLog
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- daemon.Wait() }()
	defer func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-daemonDone:
		case <-time.After(5 * time.Second):
			_ = daemon.Process.Kill()
			<-daemonDone
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for {
		if _, err = readHealth(ctx, runtimeDir); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("fixture runtime readiness failed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	id := strings.Repeat("a", 32)
	stateDir := filepath.Join(runtimeDir, "coordinators", id)
	prepare := func() {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "coordinator", "prepare", "--id", id, "--binaries", home)
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("prepare: %v %s", err, stderr.Bytes())
		}
		var value map[string]any
		if json.Unmarshal(output, &value) != nil || len(value) != 3 || value["protocol"] != float64(1) || value["id"] != id || value["version"] != version || bytes.Contains(output, []byte("token")) {
			t.Fatal("prepare leaked or changed its bounded public handshake")
		}
	}
	prepare()
	connection, err := dialControllerControl(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := pinControllerProcess(connection)
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.PidfdSendSignal(owned.handle, unix.SIGTERM, nil, 0)
		wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if owned.Wait(wait) != nil {
			_ = unix.PidfdSendSignal(owned.handle, unix.SIGKILL, nil, 0)
		}
		owned.Close()
	}()
	prepare()
	var health controllerHealth
	if err = controlRequest(ctx, stateDir, "GET", "/health", &health); err != nil || health.PID != owned.PID {
		t.Fatal("prepare replaced dedicated controller", err)
	}
	if daemon.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("prepare replaced original runtime")
	}
	command := exec.CommandContext(ctx, binary, "coordinator", "bridge", "--id", id)
	command.Env = env
	command.Stdin = strings.NewReader("GET /coordinator/status HTTP/1.1\r\nHost: relay-control\r\nConnection: close\r\n\r\n")
	output, err := command.Output()
	if err != nil {
		t.Fatal("private bridge", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(output)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"receipt"`)) {
		t.Fatal("private coordinator route unavailable")
	}
	log, _ := os.ReadFile(filepath.Join(stateDir, "controller.log"))
	if bytes.Contains(log, []byte("token=")) {
		t.Fatal("dedicated controller logged desktop login capability")
	}
}
