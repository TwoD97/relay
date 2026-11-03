package localagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessAuthenticationClassification(t *testing.T) {
	for _, tc := range []struct {
		name, provider, output, detail string
		err                            error
		want                           string
	}{
		{"claude signed in", "claude", `{"loggedIn":true,"email":"private-fixture-account"}`, "existing login", nil, "true"},
		{"claude signed out", "claude", `{"loggedIn":false,"email":"private-fixture-account"}`, "no active login", &probeExitError{1}, "false"},
		{"claude startup failure is not signed out", "claude", "missing dependency", "code 1", &probeExitError{1}, "unknown"},
		{"claude failed status with login field", "claude", `{"loggedIn":true}`, "code 1", &probeExitError{1}, "unknown"},
		{"claude unexpected failure", "claude", `{"loggedIn":false}`, "code 2", &probeExitError{2}, "unknown"},
		{"claude legacy success", "claude", "legacy status output", "existing login", nil, "true"},
		{"claude explicit no login beats success", "claude", `{"loggedIn":false}`, "no active login", nil, "false"},
		{"codex signed out", "codex", "private-fixture-account", "no active login", &probeExitError{1}, "false"},
		{"codex signed in", "codex", "private-fixture-account", "existing login", nil, "true"},
		{"timeout", "claude", "private-fixture-account", "timed out", context.DeadlineExceeded, "unknown"},
		{"cancelled", "claude", "private-fixture-account", "interrupted", context.Canceled, "unknown"},
		{"start failure", "claude", "private-fixture-account", "Could not run", errors.New("private-fixture-account"), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := harnessAuthentication(tc.provider, tc.output, tc.err)
			value := "unknown"
			if got != nil {
				value = "false"
				if *got {
					value = "true"
				}
			}
			if value != tc.want || !strings.Contains(detail, tc.detail) || strings.Contains(detail, "private-fixture-account") {
				t.Fatalf("incorrect safe authentication classification: %s, %s", value, detail)
			}
		})
	}
}

func TestHarnessAuthenticationUsesNativeExitStatus(t *testing.T) {
	t.Setenv("LOCALAGENT_PROVIDER_FIXTURE", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	root := filepath.Join(state, "tools", "harnesses", "claude")
	release := "1.0.0-" + strings.Repeat("c", 32)
	name := executableName("claude")
	directory := filepath.Join(root, "releases", release)
	if err := privateDir(directory); err != nil {
		t.Fatal(err)
	}
	func() {
		in, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		out, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatal("could not copy test-owned provider fixture")
		}
	}()
	data, _ := json.Marshal(harnessPointer{1, "claude", release, name})
	if err := atomicWrite(filepath.Join(root, "current.json"), data); err != nil {
		t.Fatal(err)
	}
	s := &Server{stateDir: state, home: t.TempDir(), executable: executable}
	for _, code := range []string{"0", "1", "2"} {
		t.Run("exit"+code, func(t *testing.T) {
			t.Setenv("LOCALAGENT_AUTH_FIXTURE", code)
			h := s.inspectHarness(context.Background(), "claude")
			if !h.Installed || h.Version == "" {
				t.Fatal("test provider could not run")
			}
			if code == "2" {
				if h.Authenticated != nil || !strings.Contains(h.AuthDetail, "code 2") {
					t.Fatal("unexpected CLI failure was interpreted as a login state")
				}
			} else if h.Authenticated == nil || *h.Authenticated != (code == "0") {
				t.Fatal("actual provider exit status lost")
			}
			encoded, _ := json.Marshal(h)
			if strings.Contains(string(encoded), "private-fixture-account") {
				t.Fatal("provider account output escaped through the harness API")
			}
		})
	}
}
