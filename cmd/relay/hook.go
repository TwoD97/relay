package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const maxHookPayload = 64 << 10

func hookRuntime(args []string) error {
	f := flag.NewFlagSet("hook", flag.ContinueOnError)
	provider := f.String("provider", "", "claude or codex")
	event := f.String("event", "", "observe or permission")
	if err := f.Parse(args); err != nil {
		return err
	}
	// A hook failure must return control to the provider. No output means Relay
	// supplied no decision; it does not mean the provider granted permission.
	return forwardPermissionHook(context.Background(), *provider, *event, os.Stdin, os.Stdout)
}

func forwardPermissionHook(ctx context.Context, provider, event string, input io.Reader, output io.Writer) error {
	if (provider != "claude" && provider != "codex") || (event != "permission" && event != "observe") {
		return nil
	}
	socket, session, token := os.Getenv("RELAY_SOCKET"), os.Getenv("RELAY_SESSION_ID"), os.Getenv("RELAY_EVENT_TOKEN")
	id, err := hex.DecodeString(session)
	if err != nil || len(id) != 16 || socket == "" || token == "" {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(input, maxHookPayload+1))
	if err != nil || len(raw) > maxHookPayload {
		return nil
	}
	var body struct {
		Event          string          `json:"hook_event_name"`
		ToolName       string          `json:"tool_name"`
		Input          json.RawMessage `json:"tool_input"`
		Cwd            string          `json:"cwd"`
		PermissionMode string          `json:"permission_mode"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	if event == "permission" && body.Event != "PermissionRequest" {
		return nil
	}
	if event == "observe" && body.Event != "SessionStart" && body.Event != "UserPromptSubmit" {
		return nil
	}
	// Do not forward transcript paths, provider session IDs, prompts, suggested
	// rules or arbitrary other fields. Only pending requests carry tool arguments.
	normalized := map[string]any{"provider": provider, "event": body.Event, "permissionMode": body.PermissionMode}
	if event == "permission" {
		if body.ToolName == "" || len(body.Input) == 0 {
			return nil
		}
		normalized["toolName"], normalized["input"], normalized["cwd"] = body.ToolName, body.Input, body.Cwd
	}
	payload, err := json.Marshal(normalized)
	if err != nil || len(payload) > maxHookPayload {
		return nil
	}
	timeout := 2 * time.Second
	if event == "permission" {
		timeout = 610 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://runtime/api/sessions/"+session+"/approval-hook", bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.GetBody = nil // Never replay this request, including after a redirect.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || event != "permission" {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 {
		return nil
	}
	var result struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal(data, &result) != nil || (result.Decision != "allow" && result.Decision != "deny") {
		return nil
	}
	decision := map[string]string{"behavior": result.Decision}
	if result.Decision == "deny" {
		decision["message"] = "Denied in Relay."
	}
	return json.NewEncoder(output).Encode(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}})
}
