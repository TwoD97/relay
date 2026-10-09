package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func stageFixture(t *testing.T) uiOptions {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "installed bundle with spaces")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bundle, "relay-controller.exe")
	if err := os.WriteFile(executable, []byte("test-controller-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := ""
	for _, arch := range []string{"amd64", "arm64"} {
		name := "relay-linux-" + arch
		data := []byte("linux runtime " + arch)
		if err := os.WriteFile(filepath.Join(bundle, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		manifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
	}
	if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	return uiOptions{Executable: executable, BinaryDir: bundle, StateDir: filepath.Join(root, "private state")}
}

func TestWindowsControllerStageConcurrentAndReadOnlyReuse(t *testing.T) {
	opts := stageFixture(t)
	var wait sync.WaitGroup
	results := make(chan uiOptions, 8)
	failures := make(chan error, 8)
	for range 8 {
		wait.Go(func() {
			staged, err := stageController(opts)
			if err != nil {
				failures <- err
			} else {
				results <- staged
			}
		})
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	var first uiOptions
	for got := range results {
		if first.Executable == "" {
			first = got
		}
		if got.Executable != first.Executable || got.BinaryDir != first.BinaryDir {
			t.Fatal("concurrent launchers activated different releases")
		}
	}
	if first.Executable == opts.Executable || first.BinaryDir == opts.BinaryDir {
		t.Fatal("persistent controller still uses installed resources")
	}
	for _, name := range []string{"relay-controller.exe", "relay-linux-amd64", "relay-linux-arm64", "SHA256SUMS"} {
		if _, err := os.Stat(filepath.Join(first.BinaryDir, name)); err != nil {
			t.Fatal(err)
		}
		assertPrivateControllerSecurity(t, filepath.Join(first.BinaryDir, name))
	}
	if err := os.Chmod(first.Executable, 0444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(first.Executable, 0600) })
	if again, err := stageController(opts); err != nil || again.Executable != first.Executable {
		t.Fatal("read-only existing controller was not reused", err)
	}
	entries, err := os.ReadDir(filepath.Join(opts.StateDir, "controller-releases"))
	if err != nil || len(entries) != 1 {
		t.Fatal("staging directories leaked or nested", len(entries), err)
	}
}

func TestWindowsControllerStageRejectsCorruptionWithoutReplacement(t *testing.T) {
	opts := stageFixture(t)
	staged, err := stageController(opts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(staged.BinaryDir, "relay-linux-arm64")
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageController(opts); err == nil {
		t.Fatal("corrupted existing release was accepted")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "tampered" {
		t.Fatal("existing mismatched release was overwritten")
	}
	if err := os.WriteFile(filepath.Join(opts.BinaryDir, "relay-linux-amd64"), []byte("source changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageController(opts); err == nil {
		t.Fatal("corrupted source bundle was accepted")
	}
}

func TestWindowsControllerStageRejectsIncompleteManifestAndLinkedFile(t *testing.T) {
	t.Run("incomplete manifest", func(t *testing.T) {
		opts := stageFixture(t)
		path := filepath.Join(opts.BinaryDir, "SHA256SUMS")
		data, _ := os.ReadFile(path)
		first := strings.SplitN(string(data), "\n", 2)[0] + "\n"
		_ = os.WriteFile(path, []byte(first), 0600)
		if _, err := stageController(opts); err == nil {
			t.Fatal("unverified architecture accepted")
		}
	})
	t.Run("linked source", func(t *testing.T) {
		opts := stageFixture(t)
		link := filepath.Join(opts.BinaryDir, "linked.exe")
		if err := os.Symlink(opts.Executable, link); err != nil {
			t.Skip("Windows user cannot create symbolic links:", err)
		}
		opts.Executable = link
		if _, err := stageController(opts); err == nil {
			t.Fatal("linked executable accepted")
		}
	})
}

func TestWindowsControllerStageCopiesRunningExecutableWithoutBundle(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := uiOptions{Executable: executable, BinaryDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "private state")}
	staged, err := stageController(opts)
	if err != nil {
		t.Fatal("could not read/copy the currently running PE", err)
	}
	originalHash, err := stageHashFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	stagedHash, err := stageHashFile(staged.Executable)
	if err != nil || originalHash != stagedHash {
		t.Fatal("running executable changed while copied", err)
	}
	if _, err := stageController(opts); err != nil {
		t.Fatal("could not reuse a verified executable-only controller release", err)
	}
}
