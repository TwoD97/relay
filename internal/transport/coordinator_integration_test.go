//go:build !windows && !relay_ssh_native

package transport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only the disposable TestRealSSH container is used. Provider activation uses a
// fixture shell script, never a provider account, real harness or model call.
func exerciseSSHCoordinator(t *testing.T, ctx context.Context, connection *Connection, bundle string, reconnect func(bool) *Connection) *Connection {
	t.Helper()
	const version = "ssh-integration-updated"
	id := strings.Repeat("e", 32)
	if err := os.WriteFile(filepath.Join(bundle, "VERSION"), []byte(version+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	remote, err := connection.StageCoordinatorBundle(ctx, bundle, version)
	if err != nil {
		t.Fatal("stage dedicated coordinator", err)
	}
	prepare := func(c *Connection) {
		t.Helper()
		output, err := c.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" coordinator prepare --id `+id+` --binaries `+quoteShell(remote), nil)
		if err != nil {
			t.Fatal("prepare coordinator", err)
		}
		var result map[string]any
		if json.Unmarshal(output, &result) != nil || len(result) != 3 || result["protocol"] != float64(1) || result["id"] != id || result["version"] != version || strings.Contains(string(output), "token") {
			t.Fatal("invalid private coordinator prepare response")
		}
	}
	prepare(connection)
	request := func(c *Connection, method, path string, body any, want int) []byte {
		t.Helper()
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.DialCoordinatorContext(ctx, id) }, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		r, _ := http.NewRequestWithContext(ctx, method, "http://relay-coordinator"+path, strings.NewReader(string(data)))
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("private coordinator request", err)
		}
		defer response.Body.Close()
		data, err = io.ReadAll(io.LimitReader(response.Body, 256<<10))
		if err != nil || response.StatusCode != want {
			t.Fatalf("coordinator %s %s returned%d: %s %v", method, path, response.StatusCode, data, err)
		}
		return data
	}
	var first struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	if json.Unmarshal(request(connection, "GET", "/health", nil, 200), &first) != nil || first.PID <= 1 || first.Version != version {
		t.Fatal("invalid dedicated controller identity")
	}
	var standby struct {
		Protocol int      `json:"protocol"`
		Blockers []string `json:"blockers"`
	}
	if json.Unmarshal(request(connection, "POST", "/coordinator/standby", map[string]any{"sourceId": strings.Repeat("a", 48), "harness": "claude", "agentMode": "manual", "hosts": []any{}}, 200), &standby) != nil || standby.Protocol != 1 || len(standby.Blockers) == 0 {
		t.Fatal("fixture provider readiness not blocked honestly")
	}
	var workspace struct {
		Revision uint64 `json:"revision"`
	}
	if json.Unmarshal(request(connection, "GET", "/coordinator/api/mastermind/workspace", nil, 200), &workspace) != nil {
		t.Fatal("invalid workspace")
	}
	page := strings.Repeat("f", 32)
	request(connection, "POST", "/coordinator/api/mastermind/workspace/pages", map[string]any{"revision": workspace.Revision, "id": page, "title": "Fixture survival", "html": "<h1>Owned coordinator page</h1>"}, 201)
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	connection = reconnect(false)
	prepare(connection)
	var second struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(request(connection, "GET", "/health", nil, 200), &second) != nil || second.PID != first.PID {
		t.Fatal("SSH disconnect replaced dedicated controller")
	}
	content := request(connection, "GET", "/coordinator/api/mastermind/workspace/pages/"+page, nil, 200)
	if !strings.Contains(string(content), "Owned coordinator page") {
		t.Fatal("coordinator workspace did not survive client disconnect")
	}
	request(connection, "GET", "/coordinator/status", nil, 200)
	health, err := connection.remoteHealth(ctx)
	if err != nil || health.Version != "ssh-integration" {
		t.Fatal("coordinator preparation replaced older live runtime", err)
	}
	t.Log("dedicated coordinator prepare/private bridge/standby/page persisted across SSH reconnect with same controller PID and older runtime")
	// Publish only into an absent fixture-managed shim path. Existing installations
	// are never overwritten, and cleanup requires the exact bytes we placed there.
	fixture := `#!/bin/sh
case "${1-}" in
 --version) printf '2.1.209\n'; exit 0;;
 --help) printf '%s\n' '--append-system-prompt --dangerously-skip-permissions'; exit 0;;
 auth) printf '{"loggedIn":true}\n'; exit 0;;
esac
printf 'COORDINATOR_FIXTURE_READY\n'
state=$("$RELAY_BIN" workspace show)
revision=$(printf '%s' "$state" | sed -n 's/^{"revision":\([0-9][0-9]*\),.*/\1/p')
test -n "$revision" || exit 1
"$RELAY_BIN" workspace tasks create --revision "$revision" --id cccccccccccccccccccccccccccccccc --title 'Fixture agent task' --status done && printf 'COORDINATOR_WORKSPACE_WRITTEN\n'
while IFS= read -r line; do printf 'COORDINATOR_INPUT:%s\n' "$line"; done
`
	fixtureHash := fmt.Sprintf("%x", sha256.Sum256([]byte(fixture)))
	install := `set -eu
umask 077
directory="$HOME/.local/share/relay/bin"
mkdir -p "$directory"
temporary=$(mktemp "$directory/.coordinator-fixture.XXXXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
printf '%s  %s\n' '` + fixtureHash + `' "$temporary" | sha256sum -c - >/dev/null
chmod 700 "$temporary"
ln -T "$temporary" "$directory/claude"
`
	if _, err = connection.Run(ctx, install, strings.NewReader(fixture)); err != nil {
		t.Fatal("publish isolated synthetic provider", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = connection.Run(cleanup, `set -eu; path="$HOME/.local/share/relay/bin/claude"; test -f "$path"; test ! -L "$path"; printf '%s  %s\n' '`+fixtureHash+`' "$path" | sha256sum -c - >/dev/null; rm "$path"`, nil)
	}()
	id = strings.Repeat("d", 32)
	prepare(connection)
	deadline := time.Now().Add(30 * time.Second)
	for {
		data := request(connection, "POST", "/coordinator/standby", map[string]any{"sourceId": strings.Repeat("a", 48), "harness": "claude", "agentMode": "manual", "hosts": []any{}}, 200)
		if json.Unmarshal(data, &standby) == nil && len(standby.Blockers) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("synthetic provider standby: %s", data)
		}
		time.Sleep(time.Second)
	}
	operation := strings.Repeat("b", 32)
	seed := map[string]any{"id": operation, "sourceId": strings.Repeat("a", 48), "harness": "claude", "agentMode": "manual", "hosts": []any{}, "workspace": map[string]any{"format": 1, "revision": 0, "updatedAt": time.Now().UTC(), "statusTemplateSeeded": true, "pages": []any{}, "tasks": []any{}}, "control": map[string]any{"format": 1, "revision": 0, "recovery": false, "messages": []any{}, "lessons": []any{}}}
	var receipt struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
		Phase  string `json:"phase"`
	}
	if json.Unmarshal(request(connection, "POST", "/coordinator/seed", seed, 201), &receipt) != nil || receipt.ID != operation || receipt.Phase != "ready" {
		t.Fatal("synthetic handoff seed rejected")
	}
	request(connection, "POST", "/coordinator/activate", map[string]string{"id": receipt.ID, "digest": receipt.Digest}, 202)
	type sessionIdentity struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
		Status    string    `json:"status"`
	}
	type coordinatorState struct {
		Revision uint64           `json:"revision"`
		Status   string           `json:"status"`
		Session  *sessionIdentity `json:"session"`
	}
	var active coordinatorState
	deadline = time.Now().Add(30 * time.Second)
	for {
		data := request(connection, "GET", "/coordinator/api/mastermind", nil, 200)
		if json.Unmarshal(data, &active) == nil && active.Status == "running" && active.Session != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("synthetic coordinator activation: %s", data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	owned := *active.Session
	deadline = time.Now().Add(10 * time.Second)
	for {
		data := request(connection, "GET", "/coordinator/api/mastermind/workspace", nil, 200)
		if strings.Contains(string(data), "Fixture agent task") {
			break
		}
		if time.Now().After(deadline) {
			history, _ := connection.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" session read --id `+owned.ID, nil)
			t.Fatalf("coordinator agent did not write private workspace: %s", history)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var activeController struct {
		PID int `json:"pid"`
	}
	_ = json.Unmarshal(request(connection, "GET", "/health", nil, 200), &activeController)
	_ = connection.Close()
	connection = reconnect(false)
	prepare(connection)
	var retained coordinatorState
	_ = json.Unmarshal(request(connection, "GET", "/coordinator/api/mastermind", nil, 200), &retained)
	_ = json.Unmarshal(request(connection, "GET", "/health", nil, 200), &second)
	if retained.Status != "running" || retained.Session == nil || retained.Session.ID != owned.ID || !retained.Session.CreatedAt.Equal(owned.CreatedAt) || second.PID != activeController.PID {
		t.Fatal("activated coordinator duplicated or lost after SSH disconnect")
	}
	if data := request(connection, "GET", "/coordinator/api/mastermind/workspace", nil, 200); !strings.Contains(string(data), "Fixture agent task") {
		t.Fatal("agent-authored workspace lost")
	}
	request(connection, "POST", "/coordinator/api/mastermind/disable", map[string]any{"revision": retained.Revision}, 202)
	if _, err = connection.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" session stop --id `+owned.ID, nil); err != nil {
		t.Fatal("stop only owned synthetic coordinator", err)
	}
	t.Log("synthetic coordinator activation and capability-scoped workspace CLI passed; same exact session/controller survived SSH disconnect without any provider call")
	return connection
}
