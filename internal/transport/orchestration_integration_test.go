//go:build linux && !relay_ssh_native

package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const orchestrationCLI = "/home/relay/.local/share/relay/bin/relay"

// This exercises CLI orchestration, not LLM autonomy. Every worker is a
// deterministic shell in a disposable container. No provider account, user SSH
// configuration, user keys, or existing Relay runtime is accessed.
//
//	RELAY_SSH_INTEGRATION=1 go test -race ./internal/transport \
//	  -run '^TestRealSSHMultiHostOrchestration$' -count=1 -v -timeout 5m
func TestRealSSHMultiHostOrchestration(t *testing.T) {
	if os.Getenv("RELAY_SSH_INTEGRATION") != "1" {
		t.Skip("set RELAY_SSH_INTEGRATION=1 for disposable multi-host Docker orchestration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "relay-orch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &orchestrationFixture{t: t, ctx: ctx, dir: dir}
	f.docker = f.lookPath("docker")
	ssh := f.lookPath("ssh")
	f.run(f.docker, "build", "-q", "-t", "relay-orchestration-test:local", "testdata/sshd")
	network := filepath.Base(dir)
	f.run(f.docker, "network", "create", network)
	t.Cleanup(func() { _ = exec.Command(f.docker, "network", "rm", network).Run() })
	for _, alias := range []string{"source", "worker"} {
		// Each fixture gets a distinct host key, instead of sharing the key baked
		// into the base SSH image. The only exposed ports bind to loopback.
		id := strings.TrimSpace(string(f.run(f.docker, "run", "--rm", "-d", "--network", network,
			"--network-alias", alias, "-p", "127.0.0.1::22", "relay-orchestration-test:local",
			"sh", "-c", "rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; exec /usr/sbin/sshd -D -e")))
		t.Cleanup(func() { _ = exec.Command(f.docker, "rm", "-f", id).Run() })
		binding := strings.TrimSpace(string(f.run(f.docker, "port", id, "22/tcp")))
		_, port, err := net.SplitHostPort(binding)
		if err != nil {
			t.Fatal(err)
		}
		f.hosts = append(f.hosts, orchestrationHost{id: id, alias: alias, port: port})
	}
	f.source, f.worker = f.hosts[0], f.hosts[1]
	controllerKey := filepath.Join(dir, "controller_key")
	f.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", controllerKey)
	public, err := os.ReadFile(controllerKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	var config, known strings.Builder
	for _, host := range f.hosts {
		f.writeRemote(host, ".ssh/authorized_keys", public, "600")
		var key []byte
		f.until("SSH fixture startup", 10*time.Second, func() bool {
			out, err := f.command(nil, f.docker, "exec", host.id, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
			key = out
			return err == nil && len(strings.Fields(string(key))) >= 2
		})
		parts := strings.Fields(string(key))
		fmt.Fprintf(&known, "[127.0.0.1]:%s %s %s\n", host.port, parts[0], parts[1])
		fmt.Fprintf(&config, "Host %s\n HostName 127.0.0.1\n Port %s\n User relay\n IdentityFile %s\n IdentitiesOnly yes\n IdentityAgent none\n PreferredAuthentications publickey\n UserKnownHostsFile %s\n GlobalKnownHostsFile /dev/null\n StrictHostKeyChecking yes\n", host.alias, host.port, controllerKey, filepath.Join(dir, "known_hosts"))
	}
	f.writeLocal("ssh_config", []byte(config.String()), 0600)
	f.writeLocal("known_hosts", []byte(known.String()), 0600)
	f.writeLocal("ssh", []byte("#!/bin/sh\nexec "+quoteShell(ssh)+" -F "+quoteShell(filepath.Join(dir, "ssh_config"))+" \"$@\"\n"), 0700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The source creates its own disposable keys. Only their public halves are
	// authorized on the worker; no desktop identity or SSH agent is forwarded.
	f.remote(f.source, "sh", "-c", `umask 077; for name in source_key wrong_key fault_key; do ssh-keygen -q -t ed25519 -N '' -f "$HOME/.ssh/$name"; done`)
	sourceKey := f.remote(f.source, "cat", "/home/relay/.ssh/source_key.pub")
	faultKey := f.remote(f.source, "cat", "/home/relay/.ssh/fault_key.pub")
	authorized := append(append([]byte{}, public...), sourceKey...)
	authorized = append(authorized, []byte(`restrict,command="/home/relay/drop-response" `)...)
	authorized = append(authorized, faultKey...)
	f.writeRemote(f.worker, ".ssh/authorized_keys", authorized, "600")
	workerKey := f.run(f.docker, "exec", f.worker.id, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	wrongHostKey := f.run(f.docker, "exec", f.source.id, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	f.writeRemote(f.source, ".ssh/known_hosts", []byte("worker "+string(workerKey)), "600")
	f.writeRemote(f.source, ".ssh/wrong_hosts", []byte("worker "+string(wrongHostKey)), "600")
	f.writeRemote(f.source, ".ssh/config", []byte(`Host worker bad-host-key bad-auth dropped-response
 HostName worker
 User relay
 IdentitiesOnly yes
 IdentityAgent none
 GlobalKnownHostsFile /dev/null
 PreferredAuthentications publickey
 ConnectTimeout 3
Host bad-host-key
 UserKnownHostsFile ~/.ssh/wrong_hosts
 IdentityFile ~/.ssh/source_key
Host bad-auth
 IdentityFile ~/.ssh/wrong_key
Host dropped-response
 IdentityFile ~/.ssh/fault_key
Host worker
 IdentityFile ~/.ssh/source_key
`), "600")
	f.writeRemote(f.worker, "drop-response", []byte("#!/bin/sh\nset -eu\nprintf 'call\\n' >> \"$HOME/dropped-response.calls\"\nexec \"$HOME/.local/share/relay/bin/relay\" bridge >/dev/null\n"), "700")
	f.buildBundle()
	f.writeLocal("state/hosts.json", []byte(`[{"id":"source","name":"Source","target":"source"},{"id":"worker","name":"Worker","target":"worker"}]`), 0600)
	app := f.launch()
	f.waitOnline(app)
	for _, host := range f.hosts {
		f.remote(host, "sh", "-c", `test -s "$HOME/.agents/skills/relay/SKILL.md" && test -s "$HOME/.claude/skills/relay/SKILL.md"`)
	}
	t.Log("controller bootstrapped two isolated hosts and installed the shared agent skill")

	const title = "same-name worker ' $()"
	parent := f.start(app, "source", title)
	// The parent PTY itself starts and coordinates its child via source-host CLI
	// commands. The Go test only observes the resulting task output and IDs.
	f.writeRemote(f.source, "coordinate.sh", []byte(orchestrationParentScript), "700")
	f.waitHistory(app, "source", parent.ID, "")
	f.send(app, "source", parent.ID, `export RELAY_PARENT_STATE=parent-alive; printf '%s\n' "$PPID" > "$HOME/runtime.pid"; /bin/sh "$HOME/coordinate.sh"`)
	f.waitHistory(app, "source", parent.ID, "PARENT_ACK=42")
	children := f.sessions(app, "worker")
	if len(children) != 1 || children[0].Title != title || children[0].ID == parent.ID {
		t.Fatalf("same-title sessions were not host-isolated: parent=%+v worker=%+v", parent, children)
	}
	child := children[0]
	parents := f.sessions(app, "source")
	if len(parents) != 1 || parents[0].ID != parent.ID {
		t.Fatalf("source session list crossed host boundaries: %+v", parents)
	}
	for _, host := range f.hosts {
		wrongID := parent.ID
		if host.alias == "source" {
			wrongID = child.ID
		}
		app.request("GET", "/api/hosts/"+host.alias+"/runtime/sessions/"+wrongID+"/history", nil, 404)
	}
	if output, err := f.cli("worker", "read", "--id", parent.ID); err == nil {
		t.Fatalf("cross-host CLI accepted another host's session ID: %v %s", err, output)
	}
	t.Log("source-host parent created/read/sent to its child, computed RESULT=42, and kept identical titles isolated by host and ID")

	ws := f.claim(app, "worker", child.ID)
	if output, err := f.cli("worker", "send", "--id", child.ID, "--text", "touch /home/relay/lease-bypassed", "--enter"); err == nil || !strings.Contains(string(output), "another viewer") {
		t.Fatalf("source CLI bypassed the browser writer lease: %v %s", err, output)
	}
	f.remote(f.worker, "test", "!", "-e", "/home/relay/lease-bypassed")
	f.release(ws)
	for _, bad := range []struct{ alias, message string }{{"bad-host-key", "HOST IDENTIFICATION HAS CHANGED"}, {"bad-auth", "Permission denied"}} {
		if output, err := f.cli(bad.alias, "send", "--id", child.ID, "--text", "touch /home/relay/unauthorized-input", "--enter"); err == nil || !strings.Contains(string(output), bad.message) {
			t.Fatalf("%s failed to reject SSH input: %v %s", bad.alias, err, output)
		}
	}
	f.remote(f.worker, "test", "!", "-e", "/home/relay/unauthorized-input")
	// The real runtime accepts the mutation, but the forced-command bridge drops
	// its response. A failed send must not lead to automatic replay on another
	// connection. Check both SSH invocation count and shell side effect count.
	if output, err := f.cli("dropped-response", "send", "--id", child.ID, "--text", `printf 'once\n' >> /home/relay/executed-once`, "--enter"); err == nil || !strings.Contains(string(output), "not retried") {
		t.Fatalf("dropped mutation response was reported as success: %v %s", err, output)
	}
	f.until("accepted mutation after dropped SSH response", 5*time.Second, func() bool {
		out, err := f.remoteResult(f.worker, "cat", "/home/relay/executed-once")
		return err == nil && string(out) == "once\n"
	})
	if count := string(f.remote(f.worker, "cat", "/home/relay/dropped-response.calls")); count != "call\n" {
		t.Fatalf("uncertain mutation replayed across SSH: %q", count)
	}
	t.Log("browser lease, changed host key, and missing authentication rejected input; accepted-but-unacknowledged mutation ran exactly once")

	identity := map[string]string{}
	for _, host := range f.hosts {
		identity[host.alias] = f.runtimeIdentity(host)
	}
	app.request("POST", "/api/hosts/worker/disconnect", nil, 204)
	// Cross-machine CLI authorization does not depend on the controller's link.
	f.cliOK("worker", "send", "--id", child.ID, "--text", `printf 'DETACHED-%s\n' "$RELAY_CHILD_STATE"`, "--enter")
	if output := f.cliOK("worker", "list"); !bytes.Contains(output, []byte(child.ID)) {
		t.Fatal("disconnecting the controller removed the remote child")
	}
	app.request("POST", "/api/reconnect", nil, 202)
	f.waitOnline(app)
	f.waitHistory(app, "worker", child.ID, "DETACHED-child-alive")
	app.stop()
	f.cliOK("worker", "send", "--id", child.ID, "--text", `printf 'NO_CONTROLLER-%s\n' "$RELAY_CHILD_STATE"`, "--enter")
	restarted := f.launch()
	if restarted.pid == app.pid {
		t.Fatal("controller restart did not create a new controller process")
	}
	f.waitOnline(restarted)
	f.waitHistory(restarted, "worker", child.ID, "NO_CONTROLLER-child-alive")
	f.send(restarted, "source", parent.ID, `printf 'RECONNECTED-%s\n' "$RELAY_PARENT_STATE"`)
	f.waitHistory(restarted, "source", parent.ID, "RECONNECTED-parent-alive")
	for _, host := range f.hosts {
		if got := f.runtimeIdentity(host); got != identity[host.alias] {
			t.Fatalf("controller reconnect replaced %s runtime: before=%q after=%q", host.alias, identity[host.alias], got)
		}
	}
	restarted.terminate(syscall.SIGKILL)
	f.cliOK("worker", "send", "--id", child.ID, "--text", `printf 'CRASHED_CONTROLLER-%s\n' "$RELAY_CHILD_STATE"`, "--enter")
	recovered := f.launch()
	f.waitOnline(recovered)
	f.waitHistory(recovered, "worker", child.ID, "CRASHED_CONTROLLER-child-alive")
	f.send(recovered, "source", parent.ID, `printf 'RECOVERED-%s\n' "$RELAY_PARENT_STATE"`)
	f.waitHistory(recovered, "source", parent.ID, "RECOVERED-parent-alive")
	for _, host := range f.hosts {
		if got := f.runtimeIdentity(host); got != identity[host.alias] {
			f.t.Fatalf("controller crash recovery replaced %s runtime: before=%q after=%q", host.alias, identity[host.alias], got)
		}
	}
	// Finish the delegation from the parent PTY, not from the controller. Stopping
	// the child must leave its same-named parent running on the other host.
	f.send(recovered, "source", parent.ID, orchestrationCLI+" session stop --ssh worker --id "+child.ID+`; printf 'PARENT_%s\n' CHILD_STOPPED`)
	f.waitHistory(recovered, "source", parent.ID, "PARENT_CHILD_STOPPED")
	if children := f.sessions(recovered, "worker"); len(children) != 0 {
		t.Fatalf("parent did not stop its child: %+v", children)
	}
	if parents := f.sessions(recovered, "source"); len(parents) != 1 || parents[0].ID != parent.ID || parents[0].Status != "running" {
		t.Fatalf("child stop affected same-named parent: %+v", parents)
	}
	f.remote(f.source, orchestrationCLI, "session", "stop", "--id", parent.ID)
	if parents := f.sessions(recovered, "source"); len(parents) != 0 {
		t.Fatal("source CLI failed to stop its own disposable parent")
	}
	t.Log("disconnect/reconnect, graceful restart, and SIGKILL recovery preserved both runtime PIDs/start times and live shell state; parent stopped only its remote child")
}

const orchestrationParentScript = `#!/bin/sh
set -eu
cli="$HOME/.local/share/relay/bin/relay"
child=$("$cli" session start --ssh worker --harness shell --cwd /home/relay --workspace Orchestration --title "same-name worker ' \$()")
id=$(printf '%s\n' "$child" | sed -n 's/.*"id":"\([0-9a-f]\{32\}\)".*/\1/p')
test -n "$id"
printf 'CHILD_ID=%s\n' "$id"
attempt=0
while :; do
  history=$("$cli" session read --ssh worker --id "$id")
  [ -n "$history" ] && break
  attempt=$((attempt + 1)); [ "$attempt" -lt 60 ]; sleep 0.1
done
"$cli" session send --ssh worker --id "$id" --text 'export RELAY_CHILD_STATE=child-alive; printf "%s\n" "$PPID" > "$HOME/runtime.pid"; printf "RESULT=%s\n" "$((6*7))"' --enter
attempt=0
while :; do
  history=$("$cli" session read --ssh worker --id "$id")
  if printf '%s' "$history" | grep -q 'RESULT=42'; then break; fi
  attempt=$((attempt + 1)); [ "$attempt" -lt 60 ]; sleep 0.1
done
printf 'PARENT_%s=%s\n' ACK 42
`

type orchestrationHost struct{ id, alias, port string }
type orchestrationSession struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}
type orchestrationFixture struct {
	t              *testing.T
	ctx            context.Context
	dir, docker    string
	binary, bundle string
	binaryHash     [32]byte
	hosts          []orchestrationHost
	source, worker orchestrationHost
}

func (f *orchestrationFixture) lookPath(name string) string {
	f.t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		f.t.Fatal(err)
	}
	return path
}
func (f *orchestrationFixture) command(input []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	return cmd.CombinedOutput()
}
func (f *orchestrationFixture) run(name string, args ...string) []byte {
	f.t.Helper()
	out, err := f.command(nil, name, args...)
	if err != nil {
		f.t.Fatalf("fixture %s failed: %v: %s", filepath.Base(name), err, out)
	}
	return out
}
func (f *orchestrationFixture) remoteResult(host orchestrationHost, args ...string) ([]byte, error) {
	return f.command(nil, f.docker, append([]string{"exec", "--user", "relay", host.id}, args...)...)
}
func (f *orchestrationFixture) remote(host orchestrationHost, args ...string) []byte {
	f.t.Helper()
	out, err := f.remoteResult(host, args...)
	if err != nil {
		f.t.Fatalf("fixture command on %s failed: %v: %s", host.alias, err, out)
	}
	return out
}
func (f *orchestrationFixture) cli(alias string, args ...string) ([]byte, error) {
	argv := append([]string{orchestrationCLI, "session"}, args...)
	return f.remoteResult(f.source, append(argv, "--ssh", alias)...)
}
func (f *orchestrationFixture) cliOK(alias string, args ...string) []byte {
	f.t.Helper()
	out, err := f.cli(alias, args...)
	if err != nil {
		f.t.Fatalf("source-host CLI failed: %v: %s", err, out)
	}
	return out
}
func (f *orchestrationFixture) writeLocal(name string, data []byte, mode os.FileMode) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		f.t.Fatal(err)
	}
}
func (f *orchestrationFixture) writeRemote(host orchestrationHost, name string, data []byte, mode string) {
	f.t.Helper()
	path := "/home/relay/" + name
	script := "umask 077; mkdir -p " + quoteShell(filepath.Dir(path)) + "; cat > " + quoteShell(path) + "; chmod " + mode + " " + quoteShell(path)
	if out, err := f.command(data, f.docker, "exec", "-i", "--user", "relay", host.id, "sh", "-c", script); err != nil {
		f.t.Fatalf("write fixture file on %s: %v %s", host.alias, err, out)
	}
}
func (f *orchestrationFixture) until(what string, timeout time.Duration, condition func() bool) {
	f.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-f.ctx.Done():
			f.t.Fatalf("%s: %v", what, f.ctx.Err())
		case <-deadline.C:
			f.t.Fatalf("timed out waiting for %s", what)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (f *orchestrationFixture) buildBundle() {
	f.t.Helper()
	f.bundle = filepath.Join(f.dir, "bundle")
	if err := os.Mkdir(f.bundle, 0700); err != nil {
		f.t.Fatal(err)
	}
	var manifest strings.Builder
	for _, arch := range []string{"amd64", "arm64"} {
		name := "relay-linux-" + arch
		path := filepath.Join(f.bundle, name)
		cmd := exec.CommandContext(f.ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-trimpath", "-ldflags", "-X main.version=orchestration-integration", "-o", path, "../../cmd/relay")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			f.t.Fatalf("build orchestration bundle: %v %s", err, out)
		}
		binary, err := os.ReadFile(path)
		if err != nil {
			f.t.Fatal(err)
		}
		digest := sha256.Sum256(binary)
		fmt.Fprintf(&manifest, "%x  %s\n", digest, name)
		if arch == runtime.GOARCH {
			f.binary, f.binaryHash = path, digest
		}
	}
	f.writeLocal("bundle/SHA256SUMS", []byte(manifest.String()), 0600)
}

type orchestrationController struct {
	f             *orchestrationFixture
	client        *http.Client
	address, csrf string
	executable    string
	startTime     string
	pid           int
}

func (f *orchestrationFixture) launch() *orchestrationController {
	f.t.Helper()
	cmd := exec.CommandContext(f.ctx, f.binary, "desktop", "--state-dir", filepath.Join(f.dir, "state"), "--binaries", f.bundle, "--local=false")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("launch isolated controller: %v %s", err, &stderr)
	}
	var launch struct {
		URL, Address string
		PID          int
	}
	if err := json.Unmarshal(data, &launch); err != nil || launch.PID < 2 {
		f.t.Fatal("invalid controller launch response")
	}
	jar, _ := cookiejar.New(nil)
	app := &orchestrationController{f: f, client: &http.Client{Jar: jar, Timeout: 15 * time.Second}, address: launch.Address, pid: launch.PID}
	app.executable, err = os.Readlink("/proc/" + strconv.Itoa(app.pid) + "/exe")
	if err != nil {
		f.t.Fatal("cannot verify fixture controller executable", err)
	}
	binary, err := os.ReadFile(app.executable)
	if err != nil || sha256.Sum256(binary) != f.binaryHash {
		f.t.Fatal("fixture controller executable does not match the test binary")
	}
	app.startTime = orchestrationProcessStart(app.pid)
	if app.startTime == "" {
		f.t.Fatal("cannot verify fixture controller process start time")
	}
	f.t.Cleanup(app.stop)
	response, err := app.client.Get(launch.URL)
	if err != nil {
		f.t.Fatal("fixture controller login failed", err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		f.t.Fatal("fixture controller login failed", response.StatusCode)
	}
	var bootstrap struct{ CSRF string }
	if json.Unmarshal(app.request("GET", "/api/bootstrap", nil, 200), &bootstrap) != nil || bootstrap.CSRF == "" {
		f.t.Fatal("fixture controller omitted CSRF")
	}
	app.csrf = bootstrap.CSRF
	return app
}
func (a *orchestrationController) stop() {
	a.terminate(syscall.SIGTERM)
}
func (a *orchestrationController) terminate(signal os.Signal) {
	// Only the exact executable/PID launched by this fixture can be signalled.
	path := "/proc/" + strconv.Itoa(a.pid) + "/exe"
	if executable, err := os.Readlink(path); err != nil || executable != a.executable || orchestrationProcessStart(a.pid) != a.startTime {
		return
	}
	process, err := os.FindProcess(a.pid)
	if err != nil {
		return
	}
	_ = process.Signal(signal)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if executable, err := os.Readlink(path); err != nil || executable != a.executable || orchestrationProcessStart(a.pid) != a.startTime {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = process.Kill()
	a.f.t.Error("fixture controller did not shut down within five seconds")
}

func orchestrationProcessStart(pid int) string {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// The executable name in parentheses may contain spaces.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}
func (a *orchestrationController) request(method, path string, body any, status int) []byte {
	a.f.t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			a.f.t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(a.f.ctx, method, a.address+path, bytes.NewReader(data))
	if err != nil {
		a.f.t.Fatal(err)
	}
	req.Header.Set("Origin", a.address)
	req.Header.Set("X-Relay-CSRF", a.csrf)
	req.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(req)
	if err != nil {
		a.f.t.Fatal("fixture controller request failed", method, path, err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil || response.StatusCode != status {
		a.f.t.Fatalf("%s %s: status=%d want=%d error=%v body=%s", method, path, response.StatusCode, status, err, data)
	}
	return data
}
func (f *orchestrationFixture) waitOnline(app *orchestrationController) {
	f.t.Helper()
	f.until("both saved hosts online", 30*time.Second, func() bool {
		var state struct {
			Hosts []struct{ ID, Status, Error, SetupWarning string }
		}
		data := app.request("GET", "/api/state", nil, 200)
		if err := json.Unmarshal(data, &state); err != nil || len(state.Hosts) != 2 {
			f.t.Fatalf("invalid two-host state: %s %v", data, err)
		}
		ready := true
		for _, host := range state.Hosts {
			if host.Status == "error" || host.SetupWarning != "" {
				f.t.Fatalf("fixture host setup failed: %+v", host)
			}
			ready = ready && host.Status == "online"
		}
		return ready
	})
}
func (f *orchestrationFixture) start(app *orchestrationController, host, title string) orchestrationSession {
	f.t.Helper()
	data := app.request("POST", "/api/hosts/"+host+"/runtime/sessions", map[string]string{"harness": "shell", "cwd": "/home/relay", "title": title, "workspace": "Orchestration"}, 201)
	var session orchestrationSession
	if json.Unmarshal(data, &session) != nil || len(session.ID) != 32 {
		f.t.Fatalf("invalid session: %s", data)
	}
	return session
}
func (f *orchestrationFixture) sessions(app *orchestrationController, host string) []orchestrationSession {
	f.t.Helper()
	var sessions []orchestrationSession
	if err := json.Unmarshal(app.request("GET", "/api/hosts/"+host+"/runtime/sessions", nil, 200), &sessions); err != nil {
		f.t.Fatal(err)
	}
	return sessions
}
func (f *orchestrationFixture) waitHistory(app *orchestrationController, host, id, marker string) {
	f.t.Helper()
	var history []byte
	f.until("session output "+marker, 15*time.Second, func() bool {
		history = app.request("GET", "/api/hosts/"+host+"/runtime/sessions/"+id+"/history", nil, 200)
		return len(history) > 0 && bytes.Contains(history, []byte(marker))
	})
}
func (f *orchestrationFixture) send(app *orchestrationController, host, id, text string) {
	f.t.Helper()
	app.request("POST", "/api/hosts/"+host+"/runtime/sessions/"+id+"/input", map[string]string{"data": text + "\r"}, 204)
}
func (f *orchestrationFixture) claim(app *orchestrationController, host, id string) *websocket.Conn {
	f.t.Helper()
	u, _ := url.Parse(app.address)
	headers := http.Header{"Origin": []string{app.address}}
	var cookies []string
	for _, cookie := range app.client.Jar.Cookies(u) {
		cookies = append(cookies, cookie.Name+"="+cookie.Value)
	}
	headers.Set("Cookie", strings.Join(cookies, "; "))
	ws, _, err := websocket.DefaultDialer.DialContext(f.ctx, "ws://"+u.Host+"/api/hosts/"+host+"/runtime/sessions/"+id+"/terminal", headers)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = ws.Close() })
	f.control(ws, true)
	return ws
}
func (f *orchestrationFixture) release(ws *websocket.Conn) {
	f.t.Helper()
	f.control(ws, false)
	_ = ws.Close()
}
func (f *orchestrationFixture) control(ws *websocket.Conn, owner bool) {
	f.t.Helper()
	action := "release"
	if owner {
		action = "claim"
	}
	if err := ws.WriteJSON(map[string]string{"type": action}); err != nil {
		f.t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			f.t.Fatal(err)
		}
		var frame struct {
			Type  string
			Owner bool
		}
		if kind == websocket.TextMessage && json.Unmarshal(data, &frame) == nil && frame.Type == "control" && frame.Owner == owner {
			return
		}
	}
}
func (f *orchestrationFixture) runtimeIdentity(host orchestrationHost) string {
	f.t.Helper()
	// The worker's interactive Bash recorded its daemon parent. PID plus Linux
	// process start time distinguishes reuse from replacement even if a PID wraps.
	return strings.TrimSpace(string(f.remote(host, "sh", "-c", `pid=$(cat "$HOME/runtime.pid"); test "$pid" -gt 1; awk '{ print $1, $22 }' "/proc/$pid/stat"`)))
}
