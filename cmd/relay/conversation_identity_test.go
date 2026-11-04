//go:build linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestConversationIdentityHooksForwardOnlyVerifiedUUIDMetadata(t *testing.T) {
	const id = "1a2b3c4d-1234-4567-89ab-0123456789ab"
	var received map[string]any
	hookTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(string(data), "PRIVATE") {
			t.Error("provider content escaped hook filter")
		}
		received = nil
		if json.Unmarshal(data, &received) != nil {
			t.Error("invalid hook payload")
		}
		w.WriteHeader(204)
	}))
	input := `{"hook_event_name":"SessionStart","session_id":"` + id + `","transcript_path":"PRIVATE","prompt":"PRIVATE","permission_mode":"default"}`
	if err := forwardPermissionHook(context.Background(), "claude", "observe", strings.NewReader(input), io.Discard); err != nil {
		t.Fatal(err)
	}
	if received["conversationId"] != id || len(received) != 4 {
		t.Fatalf("wrong metadata: %+v", received)
	}
	if err := notifyRuntime([]string{"--source", "codex-notify", "--kind", "completed", `{"thread-id":"` + id + `","last-assistant-message":"PRIVATE","input-messages":["PRIVATE"]}`}); err != nil {
		t.Fatal(err)
	}
	if received["conversationId"] != id || len(received) != 3 {
		t.Fatalf("wrong notification metadata: %+v", received)
	}
}
