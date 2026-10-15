package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The production local worker is the current executable. Intercept that exact
// subcommand in the test executable rather than adding a product-only test hook
// or accidentally starting another copy of the Go test suite.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "mastermind-worker" {
		os.Exit(mastermindTestWorker())
	}
	os.Exit(m.Run())
}

func mastermindTestWorker() int {
	root := os.Getenv("RELAY_MASTERMIND_WORKER_FIXTURE")
	if root == "" || len(os.Args) < 3 {
		return 2
	}
	if os.Args[2] == "probe" {
		f, err := os.OpenFile(filepath.Join(root, "probe-calls"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return 2
		}
		_, err = f.WriteString("probe\n")
		f.Close()
		if err != nil {
			return 2
		}
	}
	if _, err := os.Stat(filepath.Join(root, "fail-"+os.Args[2])); err == nil {
		return 2
	}
	switch os.Args[2] {
	case "probe":
		if _, err := os.Stat(filepath.Join(root, "pause-probe")); err == nil {
			_ = os.WriteFile(filepath.Join(root, "probe-started"), []byte("started"), 0600)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(filepath.Join(root, "pause-probe")); os.IsNotExist(err) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		executable, err := os.Executable()
		if err != nil {
			return 2
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return 2
		}
		data, _ := json.Marshal(os.Args[3:])
		if os.WriteFile(filepath.Join(root, "probe-args.json"), data, 0600) != nil {
			return 2
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": 1, "mastermindWorkerV1": true, "executable": executable})
		return 0
	case "status":
		launch := ""
		for i, arg := range os.Args {
			if arg == "--launch-id" && i+1 < len(os.Args) {
				launch = os.Args[i+1]
			}
		}
		if !validMastermindID(launch) {
			return 2
		}
		data, err := os.ReadFile(filepath.Join(root, launch+".json"))
		if os.IsNotExist(err) {
			data = []byte(`{"protocol":1,"found":false}`)
		} else if err != nil {
			return 2
		}
		_, err = os.Stdout.Write(data)
		if err != nil {
			return 2
		}
		return 0
	}
	return 2
}

func writeMastermindFixtureReceipt(root string, rec mastermindRecord) error {
	data, _ := json.Marshal(map[string]any{"protocol": 1, "found": true, "launch": mastermindWorkerReceipt{LaunchID: rec.Token, SessionID: rec.Session.ID, SessionCreatedAt: rec.Session.CreatedAt, Cwd: rec.CanonicalCwd, Harness: rec.Config.Harness, AgentMode: rec.Config.AgentMode}})
	return os.WriteFile(filepath.Join(root, rec.Token+".json"), data, 0600)
}

func TestMastermindWorkerLegacyLaunchAndLostRepliesNeverRepeat(t *testing.T) {
	requireLinuxMastermindCreation(t)
	for _, outcome := range []string{"confirmed", "lost-input-reply", "missing-receipt", "input-error", "lost-create-reply", "persistence-after-create", "persistence-after-input"} {
		t.Run(outcome, func(t *testing.T) {
			worker := t.TempDir()
			t.Setenv("RELAY_MASTERMIND_WORKER_FIXTURE", worker)
			f, backend := newMastermindRuntime(t)
			f.supported, f.legacyStrict = false, true
			dir := t.TempDir()
			s, ts, c := mastermindController(t, dir, backend.URL)
			if outcome == "lost-create-reply" {
				f.lostResponse = true
			}
			var breakStore func()
			if outcome == "persistence-after-create" || outcome == "persistence-after-input" {
				bad := filepath.Join(t.TempDir(), "cannot-replace-directory")
				if err := os.Mkdir(bad, 0700); err != nil {
					t.Fatal(err)
				}
				breakStore = func() { s.mastermind.mu.Lock(); s.mastermind.path = bad; s.mastermind.mu.Unlock() }
				if outcome == "persistence-after-create" {
					f.beforeCreate = breakStore
				}
			}
			f.workerInput = func(w http.ResponseWriter, r *http.Request, command string) {
				rec := s.mastermind.current()
				if rec.Phase != "worker-dispatched" || rec.Session == nil || rec.Session.Harness != "shell" || rec.Execution != "worker" || rec.Session.AgentMode != "" || rec.Session.MastermindID != "" {
					t.Errorf("input without durable truthful identity: %+v", rec)
					writeError(w, 500, "bad identity")
					return
				}
				want := "exec " + mastermindShellArguments(append([]string{rec.WorkerPath}, mastermindWorkerArgs(rec, "run")...)) + "\r"
				if command != want || r.URL.Path != "/api/sessions/"+rec.Session.ID+"/input" {
					t.Errorf("unexpected worker input %q", command)
				}
				if outcome == "confirmed" || outcome == "lost-input-reply" || outcome == "persistence-after-input" {
					if err := writeMastermindFixtureReceipt(worker, rec); err != nil {
						t.Error(err)
					}
				}
				if outcome == "persistence-after-input" {
					breakStore()
				}
				if outcome == "lost-input-reply" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				if outcome == "input-error" {
					writeError(w, 500, "unknown input outcome")
					return
				}
				w.WriteHeader(204)
			}
			mastermindSetup(t, s, ts, c, 0, "claude")
			wanted := "uncertain"
			if outcome == "confirmed" || outcome == "lost-input-reply" {
				wanted = "running"
			}
			first := awaitMastermind(t, s, wanted)
			if outcome == "persistence-after-input" {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					s.mastermind.mu.Lock()
					failed := s.mastermind.err != ""
					s.mastermind.mu.Unlock()
					if failed {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				s.mastermind.mu.Lock()
				failed := s.mastermind.err != ""
				s.mastermind.mu.Unlock()
				if !failed {
					t.Fatal("attachment never attempted its failing durable write")
				}
			}
			if first.Execution != "worker" {
				t.Fatal("legacy runtime was not given worker execution", first)
			}
			s.Close()
			ts.Close()
			if outcome == "persistence-after-input" {
				wanted = "running"
			}
			s2, _, _ := mastermindController(t, dir, backend.URL)
			restored := awaitMastermind(t, s2, wanted)
			if wanted == "running" && (restored.Session == nil || restored.Session.ID != first.Session.ID || !restored.Session.CreatedAt.Equal(first.Session.CreatedAt)) {
				t.Fatal("worker identity changed on restart")
			}
			// A new observer pass must not create or send input, even if receipt
			// recovery can confirm a previously lost response.
			s2.wakeMastermind()
			time.Sleep(70 * time.Millisecond)
			f.mu.Lock()
			creates, inputs := f.creates, len(f.inputs)
			f.mu.Unlock()
			wantInputs := 1
			if outcome == "lost-create-reply" || outcome == "persistence-after-create" {
				wantInputs = 0
			}
			if creates != 1 || inputs != wantInputs {
				t.Fatalf("replayed mutation creates=%d inputs=%d", creates, inputs)
			}
			if wanted == "uncertain" && restored.Phase == "attached" {
				t.Fatal("unconfirmed shell was attached as Mastermind")
			}
			data, err := os.ReadFile(filepath.Join(worker, "probe-args.json"))
			if err != nil {
				t.Fatal(err)
			}
			var args []string
			if json.Unmarshal(data, &args) != nil {
				t.Fatal("invalid probe arguments")
			}
			if !strings.Contains(strings.Join(args, "\n"), "--state-dir\n"+filepath.Join(dir, "runtime")) {
				t.Fatal("worker lost custom local runtime directory", args)
			}
		})
	}
}

func TestMastermindWorkerPendingPhasesRecoverReadOnly(t *testing.T) {
	requireLinuxMastermindCreation(t)
	for _, phase := range []string{"worker-prepared", "worker-dispatched"} {
		for _, receipt := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "-unconfirmed", true: "-confirmed"}[receipt], func(t *testing.T) {
				worker := t.TempDir()
				t.Setenv("RELAY_MASTERMIND_WORKER_FIXTURE", worker)
				f, backend := newMastermindRuntime(t)
				dir := t.TempDir()
				s, ts, _ := mastermindController(t, dir, backend.URL)
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				executable, err = filepath.EvalSymlinks(executable)
				if err != nil {
					t.Fatal(err)
				}
				session := MastermindSession{ID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC(), Status: "running", Harness: "shell", Cwd: f.home}
				rec := mastermindRecord{Format: 1, MastermindState: MastermindState{Revision: 1, Enabled: true, Config: &MastermindConfig{HostID: "local", Harness: "codex", AgentMode: "auto", Cwd: "~"}, Status: "starting", Execution: "worker", Session: &session}, Phase: phase, Token: strings.Repeat("a", 32), CanonicalCwd: f.home, HostTarget: s.config.LocalSocket, WorkerPath: executable, WorkerStateDir: filepath.Join(dir, "runtime")}
				f.mu.Lock()
				f.sessions = []MastermindSession{session}
				f.mu.Unlock()
				if receipt {
					if err := writeMastermindFixtureReceipt(worker, rec); err != nil {
						t.Fatal(err)
					}
				}
				s.mastermind.mu.Lock()
				err = s.mastermind.saveLocked(rec)
				s.mastermind.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
				ts.Close()
				s2, ts2, c2 := mastermindController(t, dir, backend.URL)
				s2.mastermind.notice(rec.Revision, "starting", "Awaiting fixture validation", "")
				s2.wakeMastermind()
				wanted := "uncertain"
				if receipt {
					wanted = "running"
				}
				current := awaitMastermind(t, s2, wanted)
				if !receipt {
					mastermindPost(t, s2, ts2, c2, "start", map[string]any{"revision": current.Revision}, 409)
					paused := mastermindPost(t, s2, ts2, c2, "disable", map[string]any{"revision": current.Revision}, 202)
					mastermindPost(t, s2, ts2, c2, "start", map[string]any{"revision": paused.Revision}, 202)
					awaitMastermind(t, s2, "uncertain")
				}
				f.mu.Lock()
				mutations := append([]string(nil), f.mutations...)
				f.mu.Unlock()
				if len(mutations) != 0 {
					t.Fatal("recovery replayed a mutation", mutations)
				}
				if receipt && (current.Session == nil || current.Session.ID != session.ID || current.Config.AgentMode != "auto") {
					t.Fatal("worker ownership changed", current)
				}
			})
		}
	}
}

func TestMastermindWorkerReceiptRequiresEveryExactIdentityField(t *testing.T) {
	rec := mastermindRecord{Token: strings.Repeat("a", 32), CanonicalCwd: "/project", MastermindState: MastermindState{Config: &MastermindConfig{Harness: "claude", AgentMode: "auto"}, Session: &MastermindSession{ID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC()}}}
	valid := mastermindWorkerReceipt{LaunchID: rec.Token, SessionID: rec.Session.ID, SessionCreatedAt: rec.Session.CreatedAt, Cwd: rec.CanonicalCwd, Harness: "claude", AgentMode: "auto"}
	for _, field := range []string{"match", "launch", "session", "created", "cwd", "harness", "mode", "missing", "invalid", "oversized"} {
		t.Run(field, func(t *testing.T) {
			value := valid
			switch field {
			case "launch":
				value.LaunchID = strings.Repeat("c", 32)
			case "session":
				value.SessionID = strings.Repeat("d", 32)
			case "created":
				value.SessionCreatedAt = value.SessionCreatedAt.Add(time.Second)
			case "cwd":
				value.Cwd = "/other"
			case "harness":
				value.Harness = "codex"
			case "mode":
				value.AgentMode = "manual"
			}
			data, _ := json.Marshal(map[string]any{"protocol": 1, "found": field != "missing", "launch": value})
			if field == "invalid" {
				data = []byte("bad json")
			}
			if field == "oversized" {
				data = bytes.Repeat([]byte(" "), mastermindWorkerOutputLimit+1)
			}
			err := verifyMastermindWorkerReceipt(rec, data)
			if (err == nil) != (field == "match") {
				t.Fatalf("receipt %s: %v", field, err)
			}
		})
	}
}

func TestMastermindWorkerQueuedCheckAndLatePreparationRespectRevision(t *testing.T) {
	requireLinuxMastermindCreation(t)
	worker := t.TempDir()
	t.Setenv("RELAY_MASTERMIND_WORKER_FIXTURE", worker)
	f, backend := newMastermindRuntime(t)
	f.supported, f.legacyStrict = false, true
	s, ts, c := mastermindController(t, t.TempDir(), backend.URL)
	if err := os.WriteFile(filepath.Join(worker, "fail-probe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	configured := mastermindSetup(t, s, ts, c, 0, "claude")
	blocked := awaitMastermind(t, s, "blocked")
	if blocked.Blocker != "worker" || f.createCount() != 0 {
		t.Fatal("probe failure allowed creation", blocked)
	}
	for range 3 {
		s.reconcileMastermind(s.mastermind.current())
	}
	calls, err := os.ReadFile(filepath.Join(worker, "probe-calls"))
	if err != nil || string(calls) != "probe\n" {
		t.Fatal("failed worker preparation was automatically repeated", string(calls), err)
	}
	if err := os.Remove(filepath.Join(worker, "fail-probe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, "pause-probe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	checked := mastermindPost(t, s, ts, c, "start", map[string]any{"revision": configured.Revision}, 200)
	if checked.Revision != configured.Revision {
		t.Fatal("setup check changed launch intent")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(worker, "probe-started")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(worker, "probe-started")); err != nil {
		t.Fatal("setup check did not wake worker")
	}
	// The newer configuration uses native capability to isolate the old probe.
	f.mu.Lock()
	f.supported = true
	f.legacyStrict = false
	f.mu.Unlock()
	mastermindSetup(t, s, ts, c, configured.Revision, "codex")
	if err := os.Remove(filepath.Join(worker, "pause-probe")); err != nil {
		t.Fatal(err)
	}
	current := awaitMastermind(t, s, "running")
	if current.Config.Harness != "codex" || current.Execution != "native" || f.createCount() != 1 {
		t.Fatal("late worker probe replaced newer intent", current)
	}
}

func TestMastermindWorkerCommandQuotesLiteralPathsAndRejectsControlBytes(t *testing.T) {
	for _, value := range []string{"relative/path", "/tmp/a\nexec bad", "/tmp/a\tbad", "/tmp/a\x1b[A", strings.Repeat("/", 4097)} {
		if validMastermindWorkerPath(value) {
			t.Fatal("accepted unsafe path")
		}
	}
	if !validMastermindWorkerPath("/tmp/Relay's files/$(not-a-command)") {
		t.Fatal("safe printable absolute path rejected")
	}
	got := mastermindShellArguments([]string{"/tmp/Relay's files/$(not-a-command)", "--mode", "auto"})
	if got != "'/tmp/Relay'\"'\"'s files/$(not-a-command)' '--mode' 'auto'" {
		t.Fatal("argument boundaries lost", got)
	}
}
