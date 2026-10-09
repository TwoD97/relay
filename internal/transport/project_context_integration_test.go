//go:build !windows && !relay_ssh_native

package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TwoD97/relay/internal/projectcontext"
)

// Called only inside TestRealSSH's disposable, explicitly enabled container.
func exerciseSSHProjectContext(t *testing.T, ctx context.Context, connection *Connection) {
	t.Helper()
	path := "/home/relay/shared project ' $(touch CONTEXT_INJECTION)"
	setup := "mkdir -p -- " + quoteShell(path) + "\n" +
		"printf '%s' 'existing instructions without newline' > " + quoteShell(path+"/AGENTS.md") + "\n" +
		"printf '%s' 'existing durable memory' > " + quoteShell(path+"/MEMORY.md")
	if _, err := connection.Run(ctx, setup, nil); err != nil {
		t.Fatal("create disposable shared context fixture", err)
	}
	payload, _ := json.Marshal(projectcontext.Request{Path: path})
	for attempt := range 2 {
		output, err := connection.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" project-context --json-stdin`, bytes.NewReader(payload))
		if err != nil {
			t.Fatal("shared context helper beside existing runtime", err)
		}
		var result projectcontext.Result
		if err := json.Unmarshal(output, &result); err != nil || result.Path != path || len(result.Files) != 4 {
			t.Fatalf("shared context result: %s (%v)", output, err)
		}
		for _, file := range result.Files {
			if attempt == 1 && file.Status != "preserved" {
				t.Fatalf("second prepare changed %s", file.Path)
			}
		}
	}
	agents, err := connection.Run(ctx, "cat -- "+quoteShell(path+"/AGENTS.md"), nil)
	if err != nil || !bytes.HasPrefix(agents, []byte("existing instructions without newline")) || strings.Count(string(agents), "<!-- relay:shared-context:v1 -->") != 1 {
		t.Fatalf("existing instructions not preserved: %q %v", agents, err)
	}
	memory, err := connection.Run(ctx, "cat -- "+quoteShell(path+"/MEMORY.md"), nil)
	if err != nil || string(memory) != "existing durable memory" {
		t.Fatal("existing memory changed", err)
	}
	if _, err := connection.Run(ctx, `test ! -e "$HOME/CONTEXT_INJECTION"`, nil); err != nil {
		t.Fatal("project path executed as shell code", err)
	}
	health, err := connection.remoteHealth(ctx)
	if err != nil || health.Version != "ssh-integration" {
		t.Fatal("shared context preparation replaced the old live daemon", health, err)
	}
	t.Log("shared context CLI prepared literal-path notes twice, preserved original bytes, and kept the older live daemon")
}
