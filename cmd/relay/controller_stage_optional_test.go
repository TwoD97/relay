package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func optionalStageFixture(t *testing.T) uiOptions {
	t.Helper()
	base := t.TempDir()
	bundle := filepath.Join(base, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bundle, stagedControllerFilename)
	if err := os.WriteFile(executable, []byte("fixture-controller"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := ""
	for _, name := range []string{"relay-linux-amd64", "relay-linux-arm64"} {
		data := []byte("fixture-" + name)
		if err := os.WriteFile(filepath.Join(bundle, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		manifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
	}
	if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	return uiOptions{Executable: executable, BinaryDir: bundle, StateDir: filepath.Join(base, "private")}
}
func addOptionalStageFixture(t *testing.T, dir, variant string) {
	t.Helper()
	manifest := ""
	for _, name := range []string{"relay-whatsapp-personal-linux-amd64", "relay-whatsapp-personal-linux-arm64", "relay-whatsapp-personal-windows-amd64.exe"} {
		data := []byte("fixture-" + variant + name)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		manifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
	}
	if err := os.WriteFile(filepath.Join(dir, whatsappManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestControllerStageOptionalConnectorImmutableAndIndependent(t *testing.T) {
	opts := optionalStageFixture(t)
	without, err := stageController(opts)
	if err != nil {
		t.Fatal(err)
	}
	addOptionalStageFixture(t, opts.BinaryDir, "one")
	first, err := stageController(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.BinaryDir == without.BinaryDir {
		t.Fatal("optional dependency did not alter release identity")
	}
	for name := range optionalWhatsAppArtifacts {
		got, err := os.ReadFile(filepath.Join(first.BinaryDir, name))
		if err != nil || string(got) != "fixture-one"+name {
			t.Fatalf("missing pinned helper %s: %v", name, err)
		}
	}
	addOptionalStageFixture(t, opts.BinaryDir, "two")
	second, err := stageController(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.BinaryDir == second.BinaryDir {
		t.Fatal("helper upgrade changed a live release")
	}
	old, _ := os.ReadFile(filepath.Join(first.BinaryDir, "relay-whatsapp-personal-linux-amd64"))
	if !strings.Contains(string(old), "one") {
		t.Fatal("old helper bytes overwritten")
	}
	if same, err := stageController(opts); err != nil || same.BinaryDir != second.BinaryDir {
		t.Fatal("same immutable helper was not reused", err)
	}
}
func TestControllerStageOptionalConnectorRejectsTamperingAndIncompleteManifest(t *testing.T) {
	opts := optionalStageFixture(t)
	addOptionalStageFixture(t, opts.BinaryDir, "one")
	original, err := os.ReadFile(filepath.Join(opts.BinaryDir, whatsappManifestName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(original)), "\n")
	for _, bad := range []string{lines[0] + "\n", string(original) + lines[0] + "\n", strings.Replace(string(original), "relay-whatsapp-personal-linux-amd64", "../outside", 1)} {
		if err := os.WriteFile(filepath.Join(opts.BinaryDir, whatsappManifestName), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := stageController(opts); err == nil {
			t.Fatal("invalid optional manifest accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(opts.BinaryDir, whatsappManifestName), original, 0600); err != nil {
		t.Fatal(err)
	}
	staged, err := stageController(opts)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(staged.BinaryDir, "relay-whatsapp-personal-linux-amd64")
	if err := os.WriteFile(victim, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageController(opts); err == nil {
		t.Fatal("corrupt private helper silently replaced")
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "tampered" {
		t.Fatal("corruption evidence lost")
	}
}
