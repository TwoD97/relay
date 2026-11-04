//go:build linux

package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/conversations"
)

const conversationTestID = "1a2b3c4d-1234-4567-89ab-0123456789ab"

func claudeHistoryFixture(t *testing.T, cwd, id, title string) string {
	t.Helper()
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		root = t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", root)
	}
	dir := filepath.Join(root, "projects", claudeProjectPrefix(cwd))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, id+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]string{"content": "PRIVATE PROMPT NEVER RETURN"}})
	tail, _ := json.Marshal(map[string]string{"type": "custom-title", "sessionId": id, "customTitle": title})
	if err := os.WriteFile(file, append(append(header, '\n'), append(tail, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestConversationClaudeMetadataIsolationAndAliases(t *testing.T) {
	s := testServer(t)
	s.home = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	file := claudeHistoryFixture(t, s.home, conversationTestID, "Original project title")
	original, _ := os.ReadFile(file)
	// A colliding project path and an external symlink must never enter this list.
	wrongID := "2a2b3c4d-1234-4567-89ab-0123456789ab"
	wrong, _ := json.Marshal(map[string]string{"type": "user", "cwd": "/another/project", "sessionId": wrongID, "customTitle": "PRIVATE OTHER PROJECT"})
	os.WriteFile(filepath.Join(filepath.Dir(file), wrongID+".jsonl"), wrong, 0600)
	os.Symlink(file, filepath.Join(filepath.Dir(file), "3a2b3c4d-1234-4567-89ab-0123456789ab.jsonl"))
	q := conversations.Request{Operation: "list", Harness: "claude", Cwd: s.home}
	value, err := ConversationOperation(context.Background(), s.stateDir, q)
	if err != nil {
		t.Fatal(err)
	}
	list := value.(conversations.Listing)
	if len(list.Conversations) != 1 || list.Conversations[0].Title != "Original project title" || list.RenameMode != "relay" {
		t.Fatalf("unexpected metadata: %+v", list)
	}
	data, _ := json.Marshal(list)
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("transcript or cross-project data leaked")
	}
	q.Operation, q.ID, q.Title = "rename", conversationTestID, "Renamed in Relay"
	value, err = ConversationOperation(context.Background(), s.stateDir, q)
	if err != nil {
		t.Fatal(err)
	}
	entry := value.(conversations.Entry)
	if entry.TitleSource != "relay" || entry.Title != q.Title {
		t.Fatalf("alias: %+v", entry)
	}
	if after, _ := os.ReadFile(file); string(after) != string(original) {
		t.Fatal("provider transcript changed during rename")
	}
	q.Operation = "lookup"
	value, err = ConversationOperation(context.Background(), s.stateDir, q)
	if err != nil || value.(conversations.Entry).Title != q.Title {
		t.Fatalf("durable alias not loaded: %v %+v", err, value)
	}
	q.Cwd = t.TempDir()
	if _, err = ConversationOperation(context.Background(), s.stateDir, q); err == nil {
		t.Fatal("cross-project resume identity accepted")
	}
}

func TestConversationClaudeBoundedHistoryAndLiteralResume(t *testing.T) {
	s := testServer(t)
	_, capture := modeStub(t, s, "claude", "--resume --settings", "exit 0")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	file := claudeHistoryFixture(t, s.home, conversationTestID, "Safe saved title")
	data, _ := os.ReadFile(file)
	cut := strings.IndexByte(string(data), '\n') + 1
	// Metadata can survive a large middle transcript without reading it all.
	large := append(append(append([]byte{}, data[:cut]...), []byte(strings.Repeat("x", 2<<20)+"\n")...), data[cut:]...)
	os.WriteFile(file, large, 0600)
	response := modeRequest(t, s, map[string]any{"harness": "claude", "cwd": s.home, "resumeConversationId": conversationTestID})
	live := modeSession(t, s, response)
	meta := live.snapshot()
	if meta.ConversationID != conversationTestID || meta.ResumedFrom != conversationTestID {
		t.Fatalf("resume identity missing: %+v", meta)
	}
	args := capturedArguments(t, capture)
	if len(args) < 2 || !reflect.DeepEqual(args[:2], []string{"--resume", conversationTestID}) {
		t.Fatalf("wrong resume args: %q", args)
	}
	stored, _ := os.ReadFile(filepath.Join(s.stateDir, "sessions.json"))
	if !strings.Contains(string(stored), `"conversationId":"`+conversationTestID+`"`) {
		t.Fatal("provider identity not persisted")
	}
	before := len(s.sessions)
	for _, id := range []string{"--last", "../../conversation", conversationTestID + ";true", "4a2b3c4d-1234-4567-89ab-0123456789ab"} {
		r := modeRequest(t, s, map[string]any{"harness": "claude", "cwd": s.home, "resumeConversationId": id})
		if r.Code < 400 || len(s.sessions) != before {
			t.Fatalf("invalid/missing resume created session: %d", r.Code)
		}
	}
}

func TestConversationCodexFixture(t *testing.T) {
	if os.Getenv("RELAY_CONVERSATION_TEST_PROVIDER") != "1" {
		return
	}
	if os.Getenv("RELAY_CONVERSATION_TEST_HANG") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) && os.Args[i+1] == "resume" {
			if i+2 < len(os.Args) && os.Args[i+2] == "--help" {
				fmt.Println("resume [SESSION_ID] [PROMPT] --dangerously-bypass-approvals-and-sandbox")
				os.Exit(0)
			}
			data, _ := json.Marshal(os.Args[i+1:])
			os.WriteFile(os.Getenv("RELAY_CONVERSATION_TEST_LAUNCH"), data, 0600)
			os.Exit(0)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		f, _ := os.OpenFile(os.Getenv("RELAY_CONVERSATION_TEST_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		fmt.Fprintln(f, string(scanner.Bytes()))
		f.Close()
		if request.ID == 0 {
			continue
		}
		var result any = map[string]any{}
		thread := map[string]any{"id": conversationTestID, "name": "Provider name", "cwd": os.Getenv("RELAY_CONVERSATION_TEST_CWD"), "updatedAt": time.Now().Unix(), "preview": "PRIVATE PROMPT NEVER RETURN", "turns": []string{"PRIVATE TRANSCRIPT"}}
		switch request.Method {
		case "initialize":
		case "thread/list":
			result = map[string]any{"data": []any{thread}, "nextCursor": nil}
		case "thread/read":
			result = map[string]any{"thread": thread}
		case "thread/name/set":
		default:
			os.Exit(3)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": request.ID, "result": result})
	}
	os.Exit(0)
}

func codexMetadataFixture(t *testing.T, s *Server) string {
	t.Helper()
	s.home = t.TempDir()
	t.Setenv("RELAY_CONVERSATION_TEST_PROVIDER", "1")
	t.Setenv("RELAY_CONVERSATION_TEST_CWD", s.home)
	t.Setenv("RELAY_CONVERSATION_TEST_CALLS", filepath.Join(s.stateDir, "rpc-calls"))
	t.Setenv("RELAY_CONVERSATION_TEST_LAUNCH", filepath.Join(s.stateDir, "provider-launch"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.stateDir, "bin")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nif [ \"$1\" = --help ]; then printf 'help\\n'; exit 0; fi\nexec " + shellQuote(exe) + " -test.run='^TestConversationCodexFixture$' -- \"$@\"\n"
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConversationCodexMetadataUsesProviderAPIWithoutStartingTurns(t *testing.T) {
	s := testServer(t)
	codexMetadataFixture(t, s)
	q := conversations.Request{Operation: "list", Harness: "codex", Cwd: s.home}
	value, err := ConversationOperation(context.Background(), s.stateDir, q)
	if err != nil {
		t.Fatal(err)
	}
	list := value.(conversations.Listing)
	data, _ := json.Marshal(list)
	if len(list.Conversations) != 1 || strings.Contains(string(data), "PRIVATE") || list.RenameMode != "provider" {
		t.Fatalf("bad metadata: %s", data)
	}
	q.Operation, q.ID, q.Title = "rename", conversationTestID, "New provider name"
	if _, err = ConversationOperation(context.Background(), s.stateDir, q); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(os.Getenv("RELAY_CONVERSATION_TEST_CALLS"))
	text := string(calls)
	if !strings.Contains(text, `"useStateDbOnly":true`) || !strings.Contains(text, `"includeTurns":false`) || !strings.Contains(text, `"thread/name/set"`) || strings.Contains(text, "turn/start") || strings.Contains(text, "thread/start") || strings.Contains(text, "thread/resume") {
		t.Fatalf("unsafe provider operations: %s", text)
	}
	live := modeSession(t, s, modeRequest(t, s, map[string]any{"harness": "codex", "cwd": s.home, "resumeConversationId": conversationTestID}))
	if live.snapshot().ConversationID != conversationTestID {
		t.Fatal("Codex resume identity missing")
	}
	launch, _ := os.ReadFile(os.Getenv("RELAY_CONVERSATION_TEST_LAUNCH"))
	var args []string
	json.Unmarshal(launch, &args)
	if len(args) < 2 || !reflect.DeepEqual(args[:2], []string{"resume", conversationTestID}) {
		t.Fatalf("not literal UUID resume: %s", launch)
	}
}

func TestConversationCodexCancellationDoesNotLeaveProviderRunning(t *testing.T) {
	s := testServer(t)
	path := codexMetadataFixture(t, s)
	t.Setenv("RELAY_CONVERSATION_TEST_HANG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := s.codexRPC(ctx, path, s.home, "thread/list", map[string]any{}, nil); err == nil {
		t.Fatal("cancelled request succeeded")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("cancelled metadata process did not terminate promptly")
	}
}

func TestConversationIdentityRequiresLiveSessionCapability(t *testing.T) {
	s, p := approvalFixture(t)
	for _, token := range []string{"wrong", p.eventToken} {
		req := httptest.NewRequest("POST", "/api/sessions/"+p.meta.ID+"/approval-hook", strings.NewReader(`{"provider":"claude","event":"SessionStart","conversationId":"`+conversationTestID+`"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if token == "wrong" {
			if res.Code != 401 || p.snapshot().ConversationID != "" {
				t.Fatal("untrusted conversation identity admitted")
			}
		} else if res.Code != 204 || p.snapshot().ConversationID != conversationTestID {
			t.Fatalf("trusted identity not captured: %d", res.Code)
		}
	}
	p.mu.Lock()
	p.finished = true
	p.meta.Status = "exited"
	p.mu.Unlock()
	req := httptest.NewRequest("POST", "/api/sessions/"+p.meta.ID+"/approval-hook", strings.NewReader(`{"provider":"claude","event":"SessionStart","conversationId":"2a2b3c4d-1234-4567-89ab-0123456789ab"}`))
	req.Header.Set("Authorization", "Bearer "+p.eventToken)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != 409 || p.snapshot().ConversationID != conversationTestID {
		t.Fatal("late hook rewrote finished conversation identity")
	}
}

func TestConversationResumeAlreadyRunningIsRejectedAtomically(t *testing.T) {
	s := testServer(t)
	_, capture := modeStub(t, s, "claude", "--resume", "sleep 30")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	claudeHistoryFixture(t, s.home, conversationTestID, "Title")
	body := map[string]any{"harness": "claude", "cwd": s.home, "resumeConversationId": conversationTestID}
	first := modeRequest(t, s, body)
	if first.Code != 201 {
		t.Fatal(first.Body.String())
	}
	second := modeRequest(t, s, body)
	if second.Code != 409 || len(s.sessions) != 1 {
		t.Fatalf("duplicate conversation started: %d", second.Code)
	}
	eventually(t, func() bool { data, _ := os.ReadFile(capture + "-starts"); return string(data) == "started\n" })
}
