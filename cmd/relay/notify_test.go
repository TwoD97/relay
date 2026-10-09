//go:build linux

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNotificationUsesSessionCapabilityAndDropsProviderContent(t *testing.T) {
	dir, err := os.MkdirTemp("", "relay-notify-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "runtime.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_SOCKET", socket)
	t.Setenv("RELAY_SESSION_ID", "0123456789abcdef0123456789abcdef")
	t.Setenv("RELAY_EVENT_TOKEN", "private-test-capability")
	type received struct {
		Path, Authorization string
		Body                map[string]string
	}
	got := make(chan received, 1)
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- received{r.URL.Path, r.Header.Get("Authorization"), body}
		w.WriteHeader(204)
	})}
	go s.Serve(ln)
	defer s.Close()
	if err = notifyRuntime([]string{"--source", "codex-notify", "--kind", "completed", `{"secret":"must never be forwarded"}`}); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.Path != "/api/sessions/0123456789abcdef0123456789abcdef/events" || r.Authorization != "Bearer private-test-capability" || r.Body["source"] != "codex-notify" || r.Body["kind"] != "completed" || len(r.Body) != 2 {
		t.Fatalf("unexpected notification: %+v", r)
	}
}

func TestNotificationRequiresValidSessionAndKnownEvent(t *testing.T) {
	t.Setenv("RELAY_SESSION_ID", "../../secret")
	t.Setenv("RELAY_SOCKET", "/tmp/not-used")
	t.Setenv("RELAY_EVENT_TOKEN", "token")
	for _, args := range [][]string{{"--source", "unknown", "--kind", "completed"}, {"--source", "claude-hook", "--kind", "unknown"}, {"--source", "claude-hook", "--kind", "completed"}} {
		if err := notifyRuntime(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
