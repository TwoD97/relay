//go:build !windows && !relay_ssh_native

package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TwoD97/relay/internal/conversations"
)

// Only the explicitly enabled TestRealSSH container is touched. No CLI account
// or real conversation is consulted, and no provider process is invoked.
func exerciseSSHConversations(t *testing.T, ctx context.Context, connection *Connection) {
	t.Helper()
	const id = "1a2b3c4d-1234-4567-89ab-0123456789ab"
	cwd := "/home/relay/conversation project ' $(touch CONVERSATION_INJECTION)"
	config := "/home/relay/conversation-fixture"
	var slug strings.Builder
	for _, c := range cwd {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			slug.WriteRune(c)
		} else {
			slug.WriteByte('-')
		}
	}
	dir := config + "/projects/" + slug.String()
	file := dir + "/" + id + ".jsonl"
	header, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]string{"content": "PRIVATE FIXTURE PROMPT"}})
	title, _ := json.Marshal(map[string]string{"type": "custom-title", "sessionId": id, "customTitle": "Original saved name"})
	original := append(append(header, '\n'), append(title, '\n')...)
	if _, err := connection.Run(ctx, "mkdir -p -- "+quoteShell(cwd)+" "+quoteShell(dir)+"\ncat > "+quoteShell(file), bytes.NewReader(original)); err != nil {
		t.Fatal(err)
	}
	call := func(q conversations.Request) []byte {
		payload, _ := json.Marshal(q)
		out, err := connection.Run(ctx, "CLAUDE_CONFIG_DIR="+quoteShell(config)+` exec "$HOME/.local/share/relay/bin/relay" conversations --json-stdin`, bytes.NewReader(payload))
		if err != nil {
			t.Fatal("conversation helper over existing SSH", err)
		}
		if bytes.Contains(out, []byte("PRIVATE")) {
			t.Fatal("transcript text reached API")
		}
		return out
	}
	q := conversations.Request{Operation: "list", Harness: "claude", Cwd: cwd}
	var list conversations.Listing
	if json.Unmarshal(call(q), &list) != nil || len(list.Conversations) != 1 || list.Cwd != cwd || list.Conversations[0].Title != "Original saved name" {
		t.Fatal("saved metadata not listed")
	}
	q.Operation, q.ID, q.Title = "rename", id, "Changed only in Relay"
	var entry conversations.Entry
	if json.Unmarshal(call(q), &entry) != nil || entry.TitleSource != "relay" || entry.Title != q.Title {
		t.Fatal("remote alias not saved")
	}
	q.Operation = "lookup"
	if json.Unmarshal(call(q), &entry) != nil || entry.Title != q.Title {
		t.Fatal("remote alias not durable")
	}
	unchanged, err := connection.Run(ctx, "cat -- "+quoteShell(file), nil)
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatal("conversation transcript changed")
	}
	if _, err = connection.Run(ctx, `test ! -e "$HOME/CONVERSATION_INJECTION"`, nil); err != nil {
		t.Fatal("project path executed as shell")
	}
	health, err := connection.remoteHealth(ctx)
	if err != nil || health.Version != "ssh-integration" {
		t.Fatal("conversation helper replaced the existing daemon", err)
	}
	t.Log("conversation metadata/host-local rename passed over SSH, with literal project paths and unchanged live daemon/transcript")
}
