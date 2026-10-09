package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWindowsLegacyControllerUpgrade(t *testing.T) {
	oldBinary, newBinary := os.Getenv("RELAY_WINDOWS_LEGACY_BINARY"), os.Getenv("RELAY_WINDOWS_TEST_BINARY")
	if oldBinary == "" || newBinary == "" {
		t.Skip("set legacy and candidate controller executables for real migration")
	}
	dir := filepath.Join(t.TempDir(), "private upgrade state")
	t.Setenv("RELAY_SSH_KNOWN_HOSTS", filepath.Join(t.TempDir(), "known_hosts"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	launch := func(binary string) (desktopLaunch, error) {
		cmd := exec.CommandContext(ctx, binary, "desktop", "--state-dir", dir, "--local=false")
		data, err := cmd.Output()
		var result desktopLaunch
		if err == nil {
			err = json.Unmarshal(data, &result)
		}
		return result, err
	}
	old, err := launch(oldBinary)
	if err != nil {
		t.Fatal("legacy launch", err)
	}
	opts := uiOptions{StateDir: dir, RuntimeDir: defaultDir("relay"), Executable: newBinary, BinaryDir: filepath.Dir(newBinary), Address: "127.0.0.1:0"}
	t.Cleanup(func() {
		if probe, err := probeController(context.Background(), opts); err == nil {
			defer probe.Close()
			if err := stopHandoverTestController(probe, opts); err != nil {
				t.Error("test controller cleanup", err)
			}
		}
	})
	verifyRemote := prepareRemoteUpgradeSentinel(t, old)
	probe, err := probeController(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if probe.health.Handover != 0 || probe.health.Instance != "" {
		t.Fatal("fixture is not a legacy controller")
	}
	for _, mutate := range []func(*controllerHealth, *uiOptions){
		func(h *controllerHealth, o *uiOptions) { h.PID++ },
		func(h *controllerHealth, o *uiOptions) { h.Executable = newBinary },
		func(h *controllerHealth, o *uiOptions) { h.Version = "0.1.0-wrong" },
		func(h *controllerHealth, o *uiOptions) { o.StateDir = filepath.Dir(dir) },
		func(h *controllerHealth, o *uiOptions) { h.RuntimeDir = dir },
	} {
		h, o := probe.health, opts
		mutate(&h, &o)
		if err := probe.process.Verify(ctx, h, o); err == nil {
			t.Fatal("invalid legacy identity accepted")
		}
	}
	if err := probe.process.Verify(ctx, probe.health, opts); err != nil {
		t.Fatal("valid legacy identity rejected", err)
	}
	manifest := filepath.Join(filepath.Dir(probe.process.executable), "SHA256SUMS")
	if original, readErr := os.ReadFile(manifest); readErr == nil {
		if err := os.WriteFile(manifest, append(append([]byte(nil), original...), []byte("invalid\n")...), 0600); err != nil {
			t.Fatal(err)
		}
		if err := probe.process.Verify(ctx, probe.health, opts); err == nil {
			t.Fatal("live controller with corrupt private release was accepted")
		}
		if err := os.WriteFile(manifest, original, 0600); err != nil {
			t.Fatal(err)
		}
		var sentinel controllerHealth
		if err := controlRequest(ctx, dir, http.MethodGet, "/health", &sentinel); err != nil || sentinel.PID != old.PID {
			t.Fatal("bad release hash disturbed sentinel", err)
		}
	}
	probe.Close()
	// A malformed target must fail before the old controller is terminated.
	broken := opts
	broken.Executable = filepath.Join(t.TempDir(), "invalid.exe")
	if err := os.WriteFile(broken.Executable, []byte("invalid PE bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	observed, err := probeController(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upgradeController(ctx, broken, observed); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	observed.Close()
	var alive controllerHealth
	if err := controlRequest(ctx, dir, http.MethodGet, "/health", &alive); err != nil || alive.PID != old.PID {
		t.Fatal("invalid candidate disturbed legacy controller", err)
	}
	type result struct {
		launch desktopLaunch
		err    error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() { l, e := launch(newBinary); results <- result{l, e} })
	}
	workers.Wait()
	close(results)
	pid := 0
	var migrated desktopLaunch
	for result := range results {
		if result.err != nil {
			t.Fatal("legacy upgrade", result.err)
		}
		if pid == 0 {
			migrated = result.launch
			pid = result.launch.PID
		}
		if result.launch.PID != pid || pid == old.PID || result.launch.Address != old.Address || result.launch.Version == old.Version {
			t.Fatal("legacy handover changed origin or started multiple controllers")
		}
	}
	verifyRemote(migrated)
	newProbe, err := probeController(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer newProbe.Close()
	if newProbe.health.Handover != 1 || newProbe.health.Instance == "" {
		t.Fatal("replacement does not support cooperative handover")
	}
	if !sameControllerPath(filepath.Dir(filepath.Dir(newProbe.health.Executable)), filepath.Join(dir, "controller-releases")) {
		t.Fatal("replacement uses installed executable")
	}
}
