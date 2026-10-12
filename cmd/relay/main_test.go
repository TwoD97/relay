package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureCommandOutput(t *testing.T, args []string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous; _ = writer.Close() }()
	err = run(args)
	_ = writer.Close()
	output, readErr := io.ReadAll(reader)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(output), err
}

func TestDefaultCommandShowsDesktopGuidanceWithoutStartingController(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		output, err := captureCommandOutput(t, args)
		if err != nil || !strings.Contains(output, "Open the Relay app") || !strings.Contains(output, "relay [session|") {
			t.Fatalf("help %v: output=%q err=%v", args, output, err)
		}
		for _, unwanted := range []string{"browser", "login", "[ui|", "desktop  ", "http://"} {
			if strings.Contains(output, unwanted) {
				t.Fatalf("help advertises a public controller launcher %q: %q", unwanted, output)
			}
		}
	}
}

func TestPublicUICommandDoesNotCreateStateOrPrintLogin(t *testing.T) {
	for _, extra := range [][]string{nil, {"--no-login-link=false"}} {
		stateDir := filepath.Join(t.TempDir(), "must-not-exist")
		args := append([]string{"ui", "--state-dir", stateDir, "--local=false"}, extra...)
		output, err := captureCommandOutput(t, args)
		if err == nil || !strings.Contains(err.Error(), "open the Relay app") {
			t.Fatalf("public ui returned %v instead of desktop guidance", err)
		}
		if output != "" {
			t.Fatalf("public ui printed output: %q", output)
		}
		if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("public ui touched controller state: %v", err)
		}
	}
}
