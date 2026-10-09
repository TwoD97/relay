//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionSSHHelper(t *testing.T) {
	if os.Getenv("RELAY_TEST_SSH_HELPER") != "1" {
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(os.Stdin))
	if err != nil {
		os.Exit(3)
	}
	body, _ := io.ReadAll(req.Body)
	if mode := os.Getenv("RELAY_TEST_SSH_RESPONSE"); mode != "" {
		file, err := os.OpenFile(os.Getenv("RELAY_TEST_SSH_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(4)
		}
		_, _ = file.WriteString(req.Method + " " + req.URL.Path + "\n")
		_ = file.Close()
		if mode == "redirect" {
			fmt.Fprint(os.Stdout, "HTTP/1.1 307 Temporary Redirect\r\nLocation: http://other-host/api/sessions/other/input\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}
		os.Exit(0)
	}
	response, _ := json.Marshal(map[string]any{"method": req.Method, "path": req.URL.Path, "body": string(body), "args": os.Args})
	fmt.Fprintf(os.Stdout, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", len(response))
	_, _ = os.Stdout.Write(response)
	os.Exit(0)
}

func TestSessionSSHDoesNotReplayRedirectedOrUncertainMutation(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec "+quoted+" -test.run=TestSessionSSHHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELAY_TEST_SSH_HELPER", "1")
	for _, mode := range []string{"redirect", "drop"} {
		t.Run(mode, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			t.Setenv("RELAY_TEST_SSH_RESPONSE", mode)
			t.Setenv("RELAY_TEST_SSH_CALLS", calls)
			if _, err := runSessionCLI(t, "send", "--ssh", "host", "--id", strings.Repeat("a", 32), "--text", "do this once"); err == nil {
				t.Fatal("uncertain mutation reported success")
			}
			data, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(data), "\n") != 1 {
				t.Fatal("mutation resent", string(data), err)
			}
		})
	}
}

func TestSessionSSHKeepsPromptsOutOfRemoteCommand(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec "+quoted+" -test.run=TestSessionSSHHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELAY_TEST_SSH_HELPER", "1")
	literal := "Review 'quotes', $(touch SHOULD_NOT_EXIST), `false`, and\nnewlines."
	output, err := runSessionCLI(t, "send", "--ssh", "review-host", "--ssh-port", "2222", "--id", strings.Repeat("a", 32), "--text", literal, "--enter")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Method, Path, Body string
		Args               []string
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	var input struct{ Data string }
	if err := json.Unmarshal([]byte(got.Body), &input); err != nil {
		t.Fatal(err)
	}
	if input.Data != literal+"\r" || got.Method != "POST" || got.Path != "/api/sessions/"+strings.Repeat("a", 32)+"/input" {
		t.Fatalf("request changed: %#v", got)
	}
	args := strings.Join(got.Args, " ")
	for _, required := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "ClearAllForwardings=yes", "ControlPath=none", "-p 2222 -- review-host"} {
		if !strings.Contains(args, required) {
			t.Errorf("SSH lost %s", required)
		}
	}
	if strings.Contains(args, literal) || !strings.HasSuffix(args, `exec "$HOME/.local/share/relay/bin/relay" bridge`) {
		t.Fatal("remote command contains data or lost fixed bridge")
	}
}

func TestSessionSSHRejectsAmbiguousOrUnsafeTargets(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--ssh", "-oProxyCommand=bad"},
		{"list", "--ssh", "host;bad"},
		{"list", "--ssh", "host", "--socket", "/tmp/unrelated.sock"},
		{"list", "--ssh-port", "22"},
		{"list", "--ssh", "host", "--ssh-port", "65536"},
	} {
		if _, err := runSessionCLI(t, args...); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}
