package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestControlCLIUsesPrivateCapabilityAndEvidence(t *testing.T) {
	agentCLIController(t)
	data, err := captureAgentCLI(t, controlCommand, "show")
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Messages []any `json:"messages"`
		Lessons  []any `json:"lessons"`
	}
	if json.Unmarshal(data, &state) != nil || len(state.Messages) != 0 || len(state.Lessons) != 0 {
		t.Fatal("unexpected control state")
	}
	id := strings.Repeat("f", 32)
	if _, err = captureAgentCLI(t, controlCommand, "learn", "--id", id, "--problem", "fixture outage", "--action", "verified health", "--outcome", "success", "--evidence", "test target health returned 200"); err != nil {
		t.Fatal(err)
	}
	if _, err = captureAgentCLI(t, controlCommand, "learn", "--id", id, "--problem", "fixture outage", "--action", "verified health", "--outcome", "success", "--evidence", "test target health returned 200"); err == nil {
		t.Fatal("duplicate evidence write replayed")
	}
	if _, err = captureAgentCLI(t, controlCommand, "claim", "--id", id); err == nil {
		t.Fatal("claimed nonexistent instruction")
	}
	if _, err = captureAgentCLI(t, controlCommand, "wait", "--seconds", "0"); err == nil {
		t.Fatal("unbounded invalid wait accepted")
	}
	t.Setenv("RELAY_AGENT_CAPABILITY_FILE", "")
	if _, err = captureAgentCLI(t, controlCommand, "show"); err == nil {
		t.Fatal("control bypassed private capability")
	}
}
