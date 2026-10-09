//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func hookTestSocket(t *testing.T, handler http.Handler) {
	t.Helper()
	dir, err := os.MkdirTemp("", "relay-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runtime.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	t.Setenv("RELAY_SOCKET", socket)
	t.Setenv("RELAY_SESSION_ID", "0123456789abcdef0123456789abcdef")
	t.Setenv("RELAY_EVENT_TOKEN", "private-test-capability")
}

const providerHookPayload = `{"hook_event_name":"PermissionRequest","session_id":"provider-private-id","transcript_path":"/secret/transcript","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"test command"},"cwd":"/test","permission_suggestions":[{"rule":"never forward"}]}`

func TestPermissionHookDecisionsAndPayloadMinimization(t *testing.T) {
	for _, decision := range []string{"allow", "deny", "terminal", "invalid"} {
		t.Run(decision, func(t *testing.T) {
			var calls atomic.Int32
			hookTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/api/sessions/0123456789abcdef0123456789abcdef/approval-hook" || r.Header.Get("Authorization") != "Bearer private-test-capability" {
					t.Error("missing private identity")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("bad JSON")
				}
				if len(body) != 6 || body["provider"] != "claude" || body["event"] != "PermissionRequest" || body["permissionMode"] != "default" {
					t.Errorf("unexpected normalized fields: %#v", body)
				}
				json.NewEncoder(w).Encode(map[string]string{"decision": decision})
			}))
			var output bytes.Buffer
			if err := forwardPermissionHook(context.Background(), "claude", "permission", strings.NewReader(providerHookPayload), &output); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("request replayed or missing")
			}
			if decision == "allow" || decision == "deny" {
				var result struct {
					Output struct {
						Event    string            `json:"hookEventName"`
						Decision map[string]string `json:"decision"`
					} `json:"hookSpecificOutput"`
				}
				if json.Unmarshal(output.Bytes(), &result) != nil || result.Output.Event != "PermissionRequest" || result.Output.Decision["behavior"] != decision {
					t.Fatalf("invalid provider decision: %s", output.String())
				}
				for _, forbidden := range []string{"updatedPermissions", "updatedInput", "interrupt"} {
					if strings.Contains(output.String(), forbidden) {
						t.Fatal("unsafe provider directive")
					}
				}
			} else if output.Len() != 0 {
				t.Fatal("fallback emitted a decision")
			}
		})
	}
}
func TestPermissionHookFailureNeverApprovesOrReplays(t *testing.T) {
	for _, scenario := range []string{"redirect", "server error", "malformed response", "oversized response", "timeout", "oversized input", "invalid input"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			hookTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "redirect":
					w.Header().Set("Location", "/repeat")
					w.WriteHeader(307)
				case "server error":
					w.WriteHeader(500)
				case "malformed response":
					w.Write([]byte(`{"decision":"allow"} extra`))
				case "oversized response":
					w.Write([]byte(`{"decision":"allow","extra":"` + strings.Repeat("x", 4096) + `"}`))
				case "timeout":
					<-r.Context().Done()
				default:
					json.NewEncoder(w).Encode(map[string]string{"decision": "allow"})
				}
			}))
			input := providerHookPayload
			if scenario == "oversized input" {
				input = strings.Repeat("x", maxHookPayload+1)
			}
			if scenario == "invalid input" {
				input = `{invalid`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			var output bytes.Buffer
			if err := forwardPermissionHook(ctx, "codex", "permission", strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			if output.Len() != 0 {
				t.Fatalf("fallback emitted approval: %s", output.String())
			}
			if calls.Load() > 1 {
				t.Fatal("mutation replayed")
			}
		})
	}
}
func TestObservationDropsPromptsAndToolArguments(t *testing.T) {
	hookTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 3 || body["event"] != "UserPromptSubmit" {
			t.Errorf("observation leaked payload: %#v", body)
		}
		w.WriteHeader(204)
	}))
	var output bytes.Buffer
	forwardPermissionHook(context.Background(), "codex", "observe", strings.NewReader(`{"hook_event_name":"UserPromptSubmit","permission_mode":"plan","prompt":"secret","tool_input":{"command":"secret"},"transcript_path":"secret"}`), &output)
	if output.Len() != 0 {
		t.Fatal("observation emitted a decision")
	}
}
