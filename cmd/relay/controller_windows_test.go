package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateControllerState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state with spaces")
	first, err := lockControllerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockControllerState(dir); !errors.Is(err, errControllerRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second state writer: %v", err)
	}
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "run"), filepath.Join(dir, "run", "controller.lock")} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || !windows.EqualSid(owner, sid) {
			t.Fatal("unexpected state owner", err)
		}
		dacl := sd.String()
		if !strings.Contains(dacl, "D:P") || strings.Contains(dacl, ";;;WD)") || strings.Contains(dacl, ";;;BU)") || strings.Contains(dacl, ";;;AU)") {
			t.Fatal("state permits inherited or other-user access", dacl)
		}
	}
	first.Close()
	second, err := lockControllerState(dir)
	if err != nil {
		t.Fatal("state lock was not released", err)
	}
	second.Close()
}

func TestWindowsControllerPipeIdentityAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	listener, cleanup, err := listenControllerControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer listener.Close()
	if other, _, err := listenControllerControl(dir); err == nil {
		other.Close()
		t.Fatal("a live pipe was replaced")
	}
	if controllerSocket(dir) != controllerSocket(strings.ToUpper(dir)) {
		t.Fatal("pipe names differ for case-equivalent Windows paths")
	}
	if controllerSocket(dir) == controllerSocket(dir+"-other") {
		t.Fatal("different state directories share a pipe")
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) })}
	defer server.Close()
	go server.Serve(listener)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var value struct {
		OK bool `json:"ok"`
	}
	if err := controlRequest(ctx, dir, http.MethodGet, "/health", &value); err != nil {
		t.Fatal(err)
	}
	if !value.OK {
		t.Fatal("private named-pipe response missing")
	}
}

func TestWindowsControllerRejectsLocalRuntime(t *testing.T) {
	opts, err := parseUIOptions("desktop", []string{"--state-dir", t.TempDir()})
	if err != nil || opts.Local {
		t.Fatalf("Windows default must use SSH hosts: %v", err)
	}
	if _, err := parseUIOptions("desktop", []string{"--local=true"}); err == nil {
		t.Fatal("unsupported Windows PTY silently enabled")
	}
}

func TestWindowsConcurrentDesktopLaunchers(t *testing.T) {
	binary := os.Getenv("RELAY_WINDOWS_TEST_BINARY")
	if binary == "" {
		t.Skip("set RELAY_WINDOWS_TEST_BINARY to the native controller executable")
	}
	dir := filepath.Join(t.TempDir(), "controller state")
	var mu sync.Mutex
	var launches []desktopLaunch
	var failures []error
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "desktop", "--state-dir", dir)
			data, err := cmd.Output()
			var launch desktopLaunch
			if err == nil {
				err = json.Unmarshal(data, &launch)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
			} else {
				launches = append(launches, launch)
			}
		}()
	}
	wait.Wait()
	seen := map[int]bool{}
	for _, launch := range launches {
		seen[launch.PID] = true
	}
	t.Cleanup(func() {
		for pid := range seen {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
				_, _ = proc.Wait()
			}
		}
	})
	if len(failures) > 0 {
		t.Fatal("launch failed", failures)
	}
	if len(seen) != 1 {
		t.Fatalf("expected one native controller, got %d", len(seen))
	}
	urls := map[string]bool{}
	var clients []*http.Client
	for _, launch := range launches {
		if urls[launch.URL] {
			t.Fatal("launch links were reused")
		}
		urls[launch.URL] = true
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
		res, err := client.Get(launch.URL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("sign-in status %d", res.StatusCode)
		}
		clients = append(clients, client)
	}
	for _, client := range clients {
		res, err := client.Get(launches[0].Address + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal("another launcher invalidated an existing browser")
		}
	}
}
