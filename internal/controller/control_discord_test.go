package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func discordFixtureMessage(id, text string) discordMessage {
	var message discordMessage
	message.ID, message.Content, message.ChannelID, message.Author.ID = id, text, "200000", "300000"
	return message
}
func TestDiscordPrivatePairQueueAndRedaction(t *testing.T) {
	s, mux := controlFixture(t)
	sent := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot "+strings.Repeat("t", 40) {
			t.Error("wrong bot authorization")
		}
		switch r.URL.Path {
		case "/users/@me":
			writeJSON(w, 200, map[string]any{"bot": true, "username": "fixture"})
		case "/users/@me/channels":
			writeJSON(w, 200, map[string]any{"id": "200000", "type": 1, "last_message_id": "100000"})
		case "/channels/200000/messages":
			sent++
			var body struct {
				Content  string `json:"content"`
				Mentions struct {
					Parse []string `json:"parse"`
				} `json:"allowed_mentions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Mentions.Parse == nil || len(body.Mentions.Parse) != 0 {
				t.Error("outbound content can trigger mentions")
			}
			writeJSON(w, 200, map[string]any{"id": "400000"})
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	s.discord.base = api.URL
	response := workspaceCall(t, mux, "POST", "/api/mastermind/control/discord", map[string]string{"token": strings.Repeat("t", 40), "userId": "300000"})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var pair struct {
		PairCommand string `json:"pairCommand"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &pair)
	record := s.discord.snapshot()
	for _, wrong := range []string{"guild", "bot", "owner", "channel", "webhook", "code"} {
		message := discordFixtureMessage("100001", pair.PairCommand)
		switch wrong {
		case "guild":
			message.GuildID = "999999"
		case "bot":
			message.Author.Bot = true
		case "owner":
			message.Author.ID = "999999"
		case "channel":
			message.ChannelID = "999999"
		case "webhook":
			message.WebhookID = "999999"
		case "code":
			message.Content = "!pair wrong"
		}
		if err := s.handleDiscord(record, message); err != nil {
			t.Fatal(err)
		}
		if s.discord.snapshot().Paired {
			t.Fatalf("paired invalid %s", wrong)
		}
	}
	if err := s.handleDiscord(record, discordFixtureMessage("100002", pair.PairCommand)); err != nil {
		t.Fatal(err)
	}
	record = s.discord.snapshot()
	if !record.Paired || record.PairHash != "" {
		t.Fatal("pair identity not consumed")
	}
	message := discordFixtureMessage("100003", "inspect blocked fixture")
	for range 2 {
		if err := s.handleDiscord(record, message); err != nil {
			t.Fatal(err)
		}
	}
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 1 || state.Messages[0].Source != "discord" || state.Messages[0].Status != "queued" || sent != 2 {
		t.Fatalf("duplicate instructions/sends: %d/%d", len(state.Messages), sent)
	}
	view := workspaceCall(t, mux, "GET", "/api/mastermind/control", nil).Body.String()
	for _, secret := range []string{record.Token, record.Epoch, state.Messages[0].Key} {
		if strings.Contains(view, secret) {
			t.Fatal("public view leaked secret")
		}
	}
	if err := s.discord.update("", func(next *discordRecord) error { next.Epoch = "new"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.handleDiscord(record, discordFixtureMessage("100004", "stale instruction")); err == nil {
		t.Fatal("old pairing queued work")
	}
	if result := workspaceCall(t, mux, "DELETE", "/api/mastermind/control/discord", nil); result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	if s.discord.snapshot().Token != "" {
		t.Fatal("disconnect retained token")
	}
}

func TestDiscordPollingOrdersMessagesAndNeverReplays(t *testing.T) {
	s, _ := controlFixture(t)
	s.discord.record = discordRecord{Format: 1, Epoch: "epoch", Token: "token", Owner: "300000", Channel: "200000", Paired: true, Cursor: "100000"}
	polls, sends := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			polls++
			if polls == 1 {
				writeJSON(w, 200, []discordMessage{discordFixtureMessage("100002", "second"), discordFixtureMessage("100001", "first")})
			} else {
				if r.URL.Query().Get("after") != "100002" {
					t.Error("cursor did not advance durably")
				}
				writeJSON(w, 200, []discordMessage{})
			}
			return
		}
		sends++
		writeJSON(w, 200, map[string]string{"id": "400000"})
	}))
	defer api.Close()
	s.discord.base = api.URL
	s.pollDiscord()
	s.pollDiscord()
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 2 || state.Messages[0].Text != "first" || state.Messages[1].Text != "second" || sends != 2 {
		t.Fatal("poll ordering/replay failed")
	}
	reloaded := openDiscordStore(s.config.StateDir)
	if reloaded.snapshot().Cursor != "100002" {
		t.Fatal("cursor not durable")
	}
}

func TestDiscordRateLimitRedirectAndReplyUnknown(t *testing.T) {
	s, _ := controlFixture(t)
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, 429, map[string]any{"retry_after": 60})
	}))
	defer api.Close()
	s.discord.base = api.URL
	if s.discord.call(context.Background(), "secret", "GET", "/test", nil, nil) == nil {
		t.Fatal("rate limit ignored")
	}
	_ = s.discord.call(context.Background(), "secret", "GET", "/test", nil, nil)
	if calls != 1 {
		t.Fatal("rate limit did not pause HTTP")
	}
	s.discord.limitedUntil = time.Time{}
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++ }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	s.discord.base = redirect.URL
	_ = s.discord.call(context.Background(), "secret", "GET", "/test", nil, nil)
	if destinationCalls != 0 {
		t.Fatal("bot token followed redirect")
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(502) }))
	defer failing.Close()
	s.discord.base = failing.URL
	calls = 0
	s.discord.record = discordRecord{Format: 1, Epoch: "current", Token: "secret", Owner: "300000", Channel: "200000", Paired: true}
	for index, epoch := range []string{"previous", "current"} {
		id := strings.Repeat(string(rune('a'+index)), 32)
		_, _ = s.control.enqueue(id, "discord", "task", "discord:"+epoch+":100001")
		_, _ = s.control.claim(id, "launch")
		_ = s.control.complete(id, "launch", "verified fixture")
	}
	s.deliverDiscord(s.discord.snapshot())
	s.discord.record.Sent = nil
	s.deliverDiscord(s.discord.snapshot())
	state, _, _ := s.control.snapshot()
	if calls != 1 || state.Messages[0].Delivery != "" || state.Messages[1].Delivery != "unknown" {
		t.Fatal("unknown result replayed or crossed account epoch")
	}
}

func TestRemoteApprovalIncompleteExpiredOrReusedNeverDispatches(t *testing.T) {
	s, _ := controlFixture(t)
	consumed := false
	ticket := controlApprovalTicket{ExpiresAt: time.Now().Add(time.Minute)}
	if message := s.applyControlDecision(ticket, "allow", func() error { consumed = true; return nil }); !strings.Contains(message, "complete permission") {
		t.Fatal(message)
	}
	ticket.CanAllow = true
	ticket.ExpiresAt = time.Now().Add(-time.Second)
	if message := s.applyControlDecision(ticket, "allow", func() error { consumed = true; return nil }); !strings.Contains(message, "expired") {
		t.Fatal(message)
	}
	ticket.ExpiresAt = time.Now().Add(time.Minute)
	ticket.Used = true
	_ = s.applyControlDecision(ticket, "deny", func() error { consumed = true; return nil })
	if consumed {
		t.Fatal("invalid approval consumed dispatcher")
	}
	if validDiscordID("99999999999999999999") {
		t.Fatal("overflowed Discord identity accepted")
	}
}

func TestDiscordGeneratedApprovalCanBeAnsweredExactlyOnce(t *testing.T) {
	s, _ := controlFixture(t)
	now := time.Now().UTC()
	approval := controlApproval{ID: strings.Repeat("1", 32), SessionID: strings.Repeat("2", 32), SessionCreatedAt: now, Status: "pending", ExpiresAt: now.Add(time.Minute), ToolName: "Bash", Cwd: "/fixture", Input: json.RawMessage(`{"command":"true"}`)}
	decisions := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"requests": []controlApproval{approval}})
			return
		}
		decisions++
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s.links["host"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	})
	s.store.hosts = []Host{{ID: "host", Name: "Fixture", Status: "online"}}
	var texts []string
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		texts = append(texts, body.Content)
		writeJSON(w, 200, map[string]string{"id": "400000"})
	}))
	defer discord.Close()
	s.discord.base = discord.URL
	s.discord.record = discordRecord{Format: 1, Epoch: "epoch", Token: "secret", Owner: "300000", Channel: "200000", Paired: true}
	s.sendDiscordApprovals(s.discord.snapshot())
	s.sendDiscordApprovals(s.discord.snapshot())
	tickets := s.discord.snapshot().Tickets
	if len(tickets) != 1 || !validMastermindID(tickets[0].ID) || !tickets[0].CanAllow || len(texts) != 1 || !strings.Contains(texts[0], "!allow "+tickets[0].ID) {
		t.Fatal("generated approval cannot roundtrip")
	}
	message := discordFixtureMessage("100001", "!allow "+tickets[0].ID)
	if err := s.handleDiscord(s.discord.snapshot(), message); err != nil {
		t.Fatal(err)
	}
	if err := s.handleDiscord(s.discord.snapshot(), message); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || !s.discord.snapshot().Tickets[0].Used {
		t.Fatal("approval replayed or never applied")
	}
}
