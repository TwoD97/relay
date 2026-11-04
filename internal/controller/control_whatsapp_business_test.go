package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/whatsappbridge"
)

func TestWhatsAppBusinessApprovalExactIdentityAndNoReplay(t *testing.T) {
	s, _ := businessFixture(t)
	now := time.Now().UTC()
	approval := controlApproval{ID: strings.Repeat("1", 32), SessionID: strings.Repeat("2", 32), SessionCreatedAt: now, Status: "pending", ExpiresAt: now.Add(time.Minute), ToolName: "Bash", Cwd: "/fixture", Input: json.RawMessage(`{"command":"true"}`)}
	var decisions atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"requests": []controlApproval{approval}})
		} else {
			decisions.Add(1)
			writeJSON(w, 200, map[string]bool{"ok": true})
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s.links["host"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
	})
	s.store.hosts = []Host{{ID: "host", Name: "Fixture", Status: "online"}}
	var texts []string
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		texts = append(texts, body.Text.Body)
		writeJSON(w, 200, map[string]any{"messages": []map[string]string{{"id": "sent-fixture"}}})
	}))
	defer graph.Close()
	s.whatsappBusiness.record = businessRecordFixture()
	s.whatsappBusiness.graphBase = graph.URL
	s.sendBusinessApprovals(s.whatsappBusiness.snapshot())
	s.sendBusinessApprovals(s.whatsappBusiness.snapshot())
	tickets := s.whatsappBusiness.snapshot().Tickets
	if len(tickets) != 1 || !validMastermindID(tickets[0].ID) || !tickets[0].CanAllow || len(texts) != 1 || !strings.Contains(texts[0], "!allow "+tickets[0].ID) {
		t.Fatal("approval cannot roundtrip")
	}
	message := whatsappbridge.Event{ID: "decision", Text: "!allow " + tickets[0].ID}
	for range 2 {
		if err := s.handleWhatsAppBusiness(s.whatsappBusiness.snapshot(), message); err != nil {
			t.Fatal(err)
		}
	}
	if decisions.Load() != 1 || !s.whatsappBusiness.snapshot().Tickets[0].Used {
		t.Fatal("approval replayed or not applied")
	}
}

func businessFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, mux := controlFixture(t)
	s.whatsappBusiness = openWhatsAppBusinessStore(s.config.StateDir)
	return s, mux
}
func businessRecordFixture() whatsappBusinessRecord {
	return whatsappBusinessRecord{Format: 1, Epoch: strings.Repeat("a", 32), BridgeToken: strings.Repeat("b", 64), AccessToken: strings.Repeat("t", 40), PhoneNumberID: "12345678", Owner: "49123456789", GraphVersion: "v25.0", ConfiguredAt: time.Now().Add(-time.Minute).UTC(), LastContactAt: time.Now().UTC()}
}

func TestWhatsAppBusinessConfigurationRedactionAndDurability(t *testing.T) {
	s, mux := businessFixture(t)
	var calls atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/info":
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 64) {
				t.Error("wrong bridge authorization")
			}
			writeJSON(w, 200, map[string]any{"protocol": 1, "phoneNumberId": "12345678", "owner": "49123456789"})
		case "/v25.0/12345678":
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 40) {
				t.Error("wrong Graph authorization")
			}
			writeJSON(w, 200, map[string]string{"id": "12345678"})
		default:
			t.Error("unexpected path")
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	s.whatsappBusiness.client = api.Client()
	s.whatsappBusiness.graphBase = api.URL
	body := map[string]string{"bridgeUrl": api.URL, "bridgeToken": strings.Repeat("b", 64), "accessToken": strings.Repeat("t", 40), "phoneNumberId": "12345678", "owner": "49123456789", "graphVersion": "v25.0"}
	w := workspaceCall(t, mux, "POST", "/api/mastermind/control/whatsapp-business", body)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	record := s.whatsappBusiness.snapshot()
	if !validMastermindID(record.Epoch) {
		t.Fatal("invalid epoch")
	}
	for _, secret := range []string{record.AccessToken, record.BridgeToken, record.Epoch, record.Owner, record.PhoneNumberID} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("public response leaked private configuration")
		}
	}
	reopened := openWhatsAppBusinessStore(s.config.StateDir)
	if reopened.err != nil || reopened.snapshot().Epoch != record.Epoch {
		t.Fatal("configuration did not survive restart", reopened.err)
	}
	body["bridgeUrl"] = "http://insecure.invalid"
	if w = workspaceCall(t, mux, "POST", "/api/mastermind/control/whatsapp-business", body); w.Code != 400 || calls.Load() != 2 {
		t.Fatal("insecure endpoint contacted")
	}
	if w = workspaceCall(t, mux, "DELETE", "/api/mastermind/control/whatsapp-business", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.whatsappBusiness.snapshot().AccessToken != "" {
		t.Fatal("disconnect retained credential")
	}
}

func TestWhatsAppBusinessReconfigureDuringPollNeverPromotesOldEvent(t *testing.T) {
	s, _ := businessFixture(t)
	record := businessRecordFixture()
	sent := time.Now().UTC().Truncate(time.Second)
	event := whatsappbridge.Event{ID: "old-owner-event", Sender: record.Owner, PhoneNumberID: record.PhoneNumberID, Text: "old owner instruction", SentAt: sent, ExpiresAt: sent.Add(10 * time.Minute)}
	var acknowledgments, leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(500) }))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events":
			writeJSON(w, 200, map[string]any{"protocol": 1, "events": []whatsappbridge.Event{event}})
		case "/api/events/ack":
			acknowledgments.Add(1)
			writeJSON(w, 200, map[string]bool{"ok": true})
		case "/v25.0/12345678/messages":
			if err := s.whatsappBusiness.update(record.Epoch, func(next *whatsappBusinessRecord) error {
				*next = businessRecordFixture()
				next.Epoch = strings.Repeat("d", 32)
				next.Owner = "49999999999"
				next.BridgeURL = other.URL
				return nil
			}); err != nil {
				t.Error(err)
			}
			writeJSON(w, 200, map[string]any{"messages": []map[string]string{{"id": "old-ack"}}})
		}
	}))
	defer api.Close()
	record.BridgeURL = api.URL
	s.whatsappBusiness.record = record
	s.whatsappBusiness.graphBase = api.URL
	s.pollWhatsAppBusiness()
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 1 || !strings.Contains(state.Messages[0].Key, record.Epoch) || acknowledgments.Load() != 0 || leaked.Load() != 0 {
		t.Fatal("old poll crossed reconfiguration boundary")
	}
	// This is the contact-persist / account-change boundary: an event already
	// verified under the old identity cannot be reinterpreted using new settings.
	if err := s.handleWhatsAppBusiness(record, event); err == nil {
		t.Fatal("old verified event promoted into current pairing")
	}
	state, _, _ = s.control.snapshot()
	if len(state.Messages) != 1 {
		t.Fatal("old event was replayed")
	}
}

func TestWhatsAppBusinessPollDedupAndUnknownDelivery(t *testing.T) {
	s, _ := businessFixture(t)
	record := businessRecordFixture()
	sent := record.ConfiguredAt.Add(10 * time.Second).Truncate(time.Second)
	event := whatsappbridge.Event{ID: "wamid.fixture", Sender: record.Owner, PhoneNumberID: record.PhoneNumberID, Text: "inspect fixture", SentAt: sent, ExpiresAt: sent.Add(10 * time.Minute)}
	var sends, acks atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events":
			writeJSON(w, 200, map[string]any{"protocol": 1, "events": []whatsappbridge.Event{event}})
		case "/api/events/ack":
			acks.Add(1)
			w.WriteHeader(502)
		case "/v25.0/12345678/messages":
			sends.Add(1)
			var body struct {
				To string `json:"to"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.To != record.Owner {
				t.Error("wrong reply owner")
			}
			w.WriteHeader(502)
		default:
			t.Error("unexpected path")
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	record.BridgeURL = api.URL
	s.whatsappBusiness.record = record
	s.whatsappBusiness.graphBase = api.URL
	s.pollWhatsAppBusiness()
	s.pollWhatsAppBusiness()
	state, _, err := s.control.snapshot()
	if err != nil || len(state.Messages) != 1 || state.Messages[0].Text != "inspect fixture" || state.Messages[0].Source != "whatsapp-business" || sends.Load() != 1 || acks.Load() != 2 {
		t.Fatalf("duplicate/unknown outcome: messages=%d sends=%d acks=%d err=%v", len(state.Messages), sends.Load(), acks.Load(), err)
	}
	if err = s.control.claimDelivery(state.Messages[0].ID, "whatsapp-business", "wrong:"); err == nil {
		t.Fatal("queued instruction marked delivered")
	}
	if _, err = s.control.claim(state.Messages[0].ID, "fixture-launch"); err != nil {
		t.Fatal(err)
	}
	if err = s.control.complete(state.Messages[0].ID, "fixture-launch", "verified result"); err != nil {
		t.Fatal(err)
	}
	s.deliverWhatsAppBusiness(s.whatsappBusiness.snapshot())
	s.deliverWhatsAppBusiness(s.whatsappBusiness.snapshot())
	state, _, _ = s.control.snapshot()
	if state.Messages[0].Delivery != "unknown" || sends.Load() != 2 {
		t.Fatal("unknown result replayed")
	}
	if err = s.whatsappBusiness.update("", func(next *whatsappBusinessRecord) error {
		next.Epoch = strings.Repeat("c", 32)
		next.Owner = "49999999999"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.handleWhatsAppBusiness(record, whatsappbridge.Event{ID: "late", Text: "must not enter inbox"}); err == nil {
		t.Fatal("old epoch accepted instruction")
	}
	s.whatsappBusiness.setStatus(record.Epoch, "stale error", false)
	if s.whatsappBusiness.view()["status"] == "stale error" {
		t.Fatal("late poll changed new connection status")
	}
	s.deliverWhatsAppBusiness(s.whatsappBusiness.snapshot())
	if sends.Load() != 2 {
		t.Fatal("old account result sent to new owner")
	}
}

func TestWhatsAppBusinessRejectsStaleAndForeignInbound(t *testing.T) {
	s, _ := businessFixture(t)
	record := businessRecordFixture()
	s.whatsappBusiness.record = record
	var acks atomic.Int32
	now := time.Now().UTC().Truncate(time.Second)
	valid := whatsappbridge.Event{ID: "fixture", Sender: record.Owner, PhoneNumberID: record.PhoneNumberID, Text: "fixture", SentAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	events := []whatsappbridge.Event{}
	for _, kind := range []string{"owner", "phone", "old", "before-setup", "future", "expiry", "blank"} {
		e := valid
		e.ID = kind
		switch kind {
		case "owner":
			e.Sender = "49999999999"
		case "phone":
			e.PhoneNumberID = "99999999"
		case "old":
			e.SentAt = now.Add(-11 * time.Minute)
			e.ExpiresAt = e.SentAt.Add(10 * time.Minute)
		case "before-setup":
			e.SentAt = record.ConfiguredAt.Add(-time.Second)
			e.ExpiresAt = e.SentAt.Add(10 * time.Minute)
		case "future":
			e.SentAt = now.Add(2 * time.Minute)
			e.ExpiresAt = e.SentAt.Add(10 * time.Minute)
		case "expiry":
			e.ExpiresAt = now.Add(time.Hour)
		case "blank":
			e.Text = ""
		}
		events = append(events, e)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"protocol": 1, "events": events})
		} else if r.URL.Path == "/api/events/ack" {
			acks.Add(1)
			writeJSON(w, 200, map[string]bool{"ok": true})
		} else {
			t.Error("invalid message generated outgoing action")
		}
	}))
	defer api.Close()
	s.whatsappBusiness.record.BridgeURL = api.URL
	s.whatsappBusiness.graphBase = api.URL
	s.pollWhatsAppBusiness()
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 0 || acks.Load() != int32(len(events)) {
		t.Fatal("untrusted inbox event accepted or not drained")
	}
}

func TestWhatsAppBusinessRateLimitRedirectAndClosedReplyWindow(t *testing.T) {
	s, _ := businessFixture(t)
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(429) }))
	defer api.Close()
	for range 2 {
		if s.whatsappBusiness.call(context.Background(), api.URL, "secret", "GET", nil, nil) == nil {
			t.Fatal("rate limit ignored")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("rate pause ignored")
	}
	s.whatsappBusiness.limitedUntil = time.Time{}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	_ = s.whatsappBusiness.call(context.Background(), redirect.URL, "secret", "GET", nil, nil)
	if calls.Load() != 1 {
		t.Fatal("token followed redirect")
	}
	record := businessRecordFixture()
	record.LastContactAt = time.Now().Add(-25 * time.Hour)
	s.whatsappBusiness.record = record
	s.whatsappBusiness.graphBase = destination.URL
	if s.businessSend(record, "expired reply") == nil || calls.Load() != 1 {
		t.Fatal("closed customer-service window dispatched message")
	}
}
