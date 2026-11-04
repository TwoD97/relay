package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func controlFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{config: Config{StateDir: dir}, ctx: context.Background(), control: openControlStore(dir), telegram: openTelegramStore(dir), discord: openDiscordStore(dir), mastermind: &mastermindStore{record: mastermindRecord{MastermindState: MastermindState{Enabled: true}, Token: strings.Repeat("a", 32)}}, store: &hostStore{}, links: map[string]*link{}}
	if err := s.initializeWorkspace(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterControlRoutes(mux)
	s.RegisterAgentControlRoutes(mux)
	return s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/agent/") {
			r = r.WithContext(context.WithValue(r.Context(), authenticatedCoordinatorKey{}, strings.Repeat("a", 32)))
		}
		mux.ServeHTTP(w, r)
	})
}
func TestControlInboxDurableClaimNoReplay(t *testing.T) {
	s, _ := controlFixture(t)
	id := strings.Repeat("b", 32)
	if _, err := s.control.enqueue(id, "desktop", "inspect exact session", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.control.enqueue(id, "desktop", "inspect exact session", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.control.enqueue(id, "desktop", "different task", ""); err == nil {
		t.Fatal("same ID replaced content")
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.control.claim(id, "launch"); results <- err }()
	}
	wg.Wait()
	close(results)
	claims := 0
	for err := range results {
		if err == nil {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("claims=%d", claims)
	}
	reloaded := openControlStore(s.config.StateDir)
	state, _, err := reloaded.snapshot()
	if err != nil || len(state.Messages) != 1 || state.Messages[0].Status != "processing" {
		t.Fatalf("lost processing intent: %+v %v", state, err)
	}
	if _, err = reloaded.claim(id, "other-launch"); err == nil {
		t.Fatal("restart replayed work")
	}
	if err = reloaded.complete(id, "other-launch", "done"); err == nil {
		t.Fatal("other coordinator completed work")
	}
	if err = reloaded.complete(id, "launch", "Verified result"); err != nil {
		t.Fatal(err)
	}
	state, _, _ = openControlStore(s.config.StateDir).snapshot()
	if state.Messages[0].Reply != "Verified result" {
		t.Fatal("reply not durable")
	}
}

func TestControlBrowserToPrivateCoordinatorRoundTrip(t *testing.T) {
	s, desktop, client := testController(t, "")
	login(t, s, client)
	launch := strings.Repeat("a", 32)
	s.mastermind.mu.Lock()
	s.mastermind.record.Enabled = true
	s.mastermind.record.Token = launch
	s.mastermind.mu.Unlock()
	if _, err := s.PrepareWorkspaceAgentCapability(launch); err != nil {
		t.Fatal(err)
	}
	capability, err := readWorkspaceCapability(s.config.StateDir, launch)
	if err != nil {
		t.Fatal(err)
	}
	private := httptest.NewServer(s.AgentHandler())
	defer private.Close()
	id := strings.Repeat("b", 32)
	path := desktop.URL + "/api/mastermind/control/messages"
	body := `{"id":"` + id + `","text":"Inspect the fixture job"}`
	denied := request(t, client, "POST", path, body, "", desktop.URL)
	denied.Body.Close()
	if denied.StatusCode != 403 {
		t.Fatal("instruction bypassed browser CSRF")
	}
	accepted := request(t, client, "POST", path, body, s.csrf, desktop.URL)
	accepted.Body.Close()
	if accepted.StatusCode != 201 {
		t.Fatalf("queue: %d", accepted.StatusCode)
	}
	agentCall := func(method, path, body, token string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, private.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, data
	}
	if code, _ := agentCall("GET", "/agent/control/wait", "", strings.Repeat("0", 64)); code != 401 {
		t.Fatal("foreign coordinator read the inbox")
	}
	if code, data := agentCall("GET", "/agent/control/wait", "", capability.Token); code != 200 || !strings.Contains(string(data), id) {
		t.Fatalf("poll: %d %s", code, data)
	}
	if code, _ := agentCall("POST", "/agent/control/messages/"+id+"/claim", "", capability.Token); code != 200 {
		t.Fatal("could not claim instruction")
	}
	if code, _ := agentCall("POST", "/agent/control/messages/"+id+"/claim", "", capability.Token); code != 409 {
		t.Fatal("second claim repeated instruction")
	}
	if code, _ := agentCall("POST", "/agent/control/messages/"+id+"/reply", `{"text":"Verified fixture result"}`, capability.Token); code != 200 {
		t.Fatal("result not delivered")
	}
	response := request(t, client, "GET", desktop.URL+"/api/mastermind/control", "", "", desktop.URL)
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(data), "Verified fixture result") || strings.Contains(string(data), capability.Token) {
		t.Fatal("roundtrip result or capability boundary failed")
	}
}
func TestControlCorruptStoragePreservedAndLessonsEvidence(t *testing.T) {
	s, mux := controlFixture(t)
	if err := os.WriteFile(s.control.path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := openControlStore(s.config.StateDir)
	if _, err := broken.enqueue(strings.Repeat("a", 32), "desktop", "task", ""); err == nil {
		t.Fatal("corrupt state overwritten")
	}
	data, _ := os.ReadFile(s.control.path)
	if string(data) != "broken" {
		t.Fatal("corrupt evidence lost")
	}
	lesson := map[string]any{"id": strings.Repeat("d", 32), "problem": "timeout", "action": "read health", "outcome": "success", "evidence": ""}
	if got := workspaceCall(t, mux, "POST", "/agent/control/lessons", lesson); got.Code != 400 {
		t.Fatalf("unverified lesson: %s", got.Body.String())
	}
	lesson["evidence"] = "health returned 200 for exact target"
	if got := workspaceCall(t, mux, "POST", "/agent/control/lessons", lesson); got.Code != 201 {
		t.Fatalf("verified lesson: %s", got.Body.String())
	}
}
func TestControlRecoveryOnlyExplicitLinkedBlockersOnce(t *testing.T) {
	s, _ := controlFixture(t)
	s.workspace.record.Tasks = []WorkspaceTask{{ID: strings.Repeat("e", 32), Title: "Fix service", Detail: "health check failed", Status: "blocked", HostID: "test-host", SessionID: strings.Repeat("f", 32)}, {ID: strings.Repeat("c", 32), Title: "Quiet terminal", Status: "doing", HostID: "test-host", SessionID: strings.Repeat("f", 32)}, {ID: strings.Repeat("d", 32), Title: "Missing decision", Status: "blocked"}}
	s.queueRecovery()
	s.queueRecovery()
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 1 || state.Messages[0].Source != "recovery" || !strings.Contains(state.Messages[0].Text, "Ask the user before destructive") {
		t.Fatalf("bad recovery requests: %+v", state.Messages)
	}
	if err := s.control.update(func(r *controlRecord) error { r.Recovery = false; return nil }); err != nil {
		t.Fatal(err)
	}
	s.workspace.record.Tasks[0].ID = strings.Repeat("1", 32)
	s.queueRecovery()
	state, _, _ = s.control.snapshot()
	if len(state.Messages) != 1 {
		t.Fatal("disabled recovery queued work")
	}
}
func TestTelegramPairOwnerQueueAndRedaction(t *testing.T) {
	s, mux := controlFixture(t)
	var callsMu sync.Mutex
	methods := []string{}
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		methods = append(methods, r.URL.Path)
		callsMu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			writeJSON(w, 200, map[string]any{"ok": true, "result": map[string]any{"is_bot": true, "username": "fixture_bot"}})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "result": true})
	}))
	defer bot.Close()
	s.telegram.base = bot.URL
	token := "123456:" + strings.Repeat("a", 30)
	response := workspaceCall(t, mux, "POST", "/api/mastermind/control/telegram", map[string]string{"token": token})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var pair struct {
		PairCommand string `json:"pairCommand"`
	}
	if json.Unmarshal(response.Body.Bytes(), &pair) != nil {
		t.Fatal("invalid pairing")
	}
	record := s.telegram.snapshot()
	wrong := telegramUpdate{ID: 1, Message: &telegramMessage{From: telegramUser{ID: 42}, Chat: telegramChat{ID: 42, Type: "group"}, Text: pair.PairCommand}}
	if err := s.handleTelegram(record, wrong); err != nil {
		t.Fatal(err)
	}
	if s.telegram.snapshot().Owner != 0 {
		t.Fatal("group paired")
	}
	wrong.Message.Chat.Type = "private"
	wrong.Message.Text = "/start wrong"
	_ = s.handleTelegram(record, wrong)
	if s.telegram.snapshot().Owner != 0 {
		t.Fatal("wrong code paired")
	}
	wrong.Message.Text = pair.PairCommand
	_ = s.handleTelegram(record, wrong)
	record = s.telegram.snapshot()
	if record.Owner != 42 || record.PairHash != "" {
		t.Fatal("pair identity not consumed")
	}
	message := telegramUpdate{ID: 2, Message: &telegramMessage{From: telegramUser{ID: 99}, Chat: telegramChat{ID: 99, Type: "private"}, Text: "do not execute this"}}
	_ = s.handleTelegram(record, message)
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 0 {
		t.Fatal("unpaired sender queued instruction")
	}
	message.Message.From.ID = 42
	message.Message.Chat.ID = 42
	message.Message.Text = "inspect the blocked task"
	if err := s.handleTelegram(record, message); err != nil {
		t.Fatal(err)
	}
	if err := s.handleTelegram(record, message); err != nil {
		t.Fatal(err)
	}
	state, _, _ = s.control.snapshot()
	if len(state.Messages) != 1 || state.Messages[0].Status != "queued" {
		t.Fatal("duplicate instruction or implicit execution")
	}
	view := workspaceCall(t, mux, "GET", "/api/mastermind/control", nil).Body.String()
	for _, secret := range []string{token, record.Epoch, state.Messages[0].Key} {
		if strings.Contains(view, secret) {
			t.Fatalf("public control view exposed private bookkeeping")
		}
	}
	if got := workspaceCall(t, mux, "DELETE", "/api/mastermind/control/telegram", nil); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if s.telegram.snapshot().Token != "" {
		t.Fatal("disconnect retained token")
	}
}
func TestTelegramExpiryRedirectAndUnknownDeliveryNoReplay(t *testing.T) {
	s, _ := controlFixture(t)
	code := "pair-code"
	sum := sha256.Sum256([]byte(code))
	s.telegram.record = telegramRecord{Format: 1, Epoch: "epoch", Token: "secret", PairHash: hex.EncodeToString(sum[:]), PairExpires: time.Now().Add(-time.Second)}
	_ = s.handleTelegram(s.telegram.snapshot(), telegramUpdate{ID: 1, Message: &telegramMessage{From: telegramUser{ID: 42}, Chat: telegramChat{ID: 42, Type: "private"}, Text: "/start " + code}})
	if s.telegram.snapshot().Owner != 0 {
		t.Fatal("expired pairing accepted")
	}
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; w.WriteHeader(200) }))
	defer target.Close()
	calls := 0
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Redirect(w, r, target.URL, 307) }))
	defer bot.Close()
	s.telegram.base = bot.URL
	s.telegram.record.Owner = 42
	s.telegram.record.Chat = 42
	for range 2 {
		if err := s.sendTelegramOnce(s.telegram.snapshot(), "one-delivery", "private result", nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || redirected != 0 {
		t.Fatalf("replay or redirect: %d %d", calls, redirected)
	}
}

func TestTelegramCompletedRepliesStayWithOriginalPairAndNeverReplay(t *testing.T) {
	s, _ := controlFixture(t)
	calls := 0
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(502) }))
	defer bot.Close()
	s.telegram.base = bot.URL
	s.telegram.record = telegramRecord{Format: 1, Epoch: "current", Token: "token", Owner: 42, Chat: 42}
	for index, epoch := range []string{"old-pair", "current"} {
		id := strings.Repeat(string(rune('a'+index)), 32)
		if _, err := s.control.enqueue(id, "telegram", "inspect task", "telegram:"+epoch+":1"); err != nil {
			t.Fatal(err)
		}
		_, _ = s.control.claim(id, "launch")
		if err := s.control.complete(id, "launch", "checked result"); err != nil {
			t.Fatal(err)
		}
	}
	s.deliverTelegram(s.telegram.snapshot())
	s.telegram.record.Sent = nil
	s.deliverTelegram(s.telegram.snapshot())
	if calls != 1 {
		t.Fatalf("reply crossed pair boundary or replayed: %d", calls)
	}
	state, _, _ := s.control.snapshot()
	if state.Messages[0].Delivery != "" || state.Messages[1].Delivery != "unknown" {
		t.Fatal("unknown delivery was not durably recorded")
	}
}
func TestTelegramApprovalExactIdentityAndUnknownNoRetry(t *testing.T) {
	s, _ := controlFixture(t)
	now := time.Now().UTC()
	approval := controlApproval{ID: strings.Repeat("1", 32), SessionID: strings.Repeat("2", 32), SessionCreatedAt: now, Status: "pending", ExpiresAt: now.Add(time.Minute)}
	posts := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"requests": []controlApproval{approval}})
			return
		}
		posts++
		var decision struct {
			Session   string    `json:"sessionId"`
			CreatedAt time.Time `json:"sessionCreatedAt"`
			Decision  string    `json:"decision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&decision)
		if decision.Session != approval.SessionID || !decision.CreatedAt.Equal(approval.SessionCreatedAt) || decision.Decision != "deny" {
			t.Error("decision lost exact identity")
		}
		w.WriteHeader(502)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s.links["host"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	})
	s.store.hosts = []Host{{ID: "host", Status: "online"}}
	ticket := controlApprovalTicket{ID: strings.Repeat("3", 32), Host: "host", Request: approval.ID, Session: approval.SessionID, CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	s.telegram.record = telegramRecord{Format: 1, Epoch: "epoch", Token: "token", Owner: 42, Chat: 42, Tickets: []controlApprovalTicket{ticket}}
	if got := s.telegramDecision(s.telegram.snapshot(), ticket.ID, "deny"); !strings.Contains(got, "no longer pending") || posts != 0 {
		t.Fatalf("reused session accepted: %s", got)
	}
	s.telegram.record.Tickets[0].CreatedAt = now
	if got := s.telegramDecision(s.telegram.snapshot(), ticket.ID, "deny"); !strings.Contains(got, "unknown") {
		t.Fatalf("lost outcome hidden: %s", got)
	}
	_ = s.telegramDecision(s.telegram.snapshot(), ticket.ID, "deny")
	if posts != 1 {
		t.Fatalf("unknown decision replayed: %d", posts)
	}
}

func TestControlRevokedCoordinatorCannotMutateAfterAuthentication(t *testing.T) {
	for _, route := range []string{"claim", "lessons"} {
		t.Run(route, func(t *testing.T) {
			s, _ := controlFixture(t)
			launch := strings.Repeat("a", 32)
			_, err := s.PrepareWorkspaceAgentCapability(launch)
			if err != nil {
				t.Fatal(err)
			}
			capability, err := readWorkspaceCapability(s.config.StateDir, launch)
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("b", 32)
			_, _ = s.control.enqueue(id, "desktop", "private work", "")
			mux := http.NewServeMux()
			s.RegisterAgentControlRoutes(mux)
			authenticated, proceed := make(chan struct{}), make(chan struct{})
			handler := s.WorkspaceAgentAuthorization(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(authenticated)
				<-proceed
				mux.ServeHTTP(w, r)
			}))
			path, body := "/agent/control/messages/"+id+"/claim", ""
			if route == "lessons" {
				path = "/agent/control/lessons"
				body = `{"id":"` + id + `","problem":"timeout","action":"read health","outcome":"success","evidence":"exact fixture returned 200"}`
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+capability.Token)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { handler.ServeHTTP(response, req); close(done) }()
			select {
			case <-authenticated:
			case <-time.After(2 * time.Second):
				t.Fatal("authentication did not complete")
			}
			s.mastermind.mu.Lock()
			s.mastermind.record.Token = strings.Repeat("c", 32)
			s.mastermind.mu.Unlock()
			close(proceed)
			<-done
			if response.Code != 401 {
				t.Fatalf("stale launch mutation: %d %s", response.Code, response.Body.String())
			}
			state, _, _ := s.control.snapshot()
			if state.Messages[0].Status != "queued" || len(state.Lessons) != 0 {
				t.Fatal("revoked launch changed durable state")
			}
		})
	}
}

func TestControlWaitRevalidatesCoordinatorBeforeReturningNewWork(t *testing.T) {
	s, handler := controlFixture(t)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/agent/control/wait", nil))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, listener, _ := s.control.snapshot()
		if !listener.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll did not start")
		}
		time.Sleep(time.Millisecond)
	}
	s.mastermind.mu.Lock()
	s.mastermind.record.Token = strings.Repeat("c", 32)
	s.mastermind.mu.Unlock()
	_, _ = s.control.enqueue(strings.Repeat("b", 32), "desktop", "new coordinator work", "")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not wake")
	}
	if response.Code != 401 || strings.Contains(response.Body.String(), "new coordinator work") {
		t.Fatalf("revoked poll leaked work: %d %s", response.Code, response.Body.String())
	}
}
