package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/personalwhatsapp"
)

type fakePersonalClient struct {
	mu                                      sync.Mutex
	state                                   personalwhatsapp.State
	pairs, connects, sends, logouts, closes int
	pairError, sendError                    bool
	outputs                                 []string
}

func (f *fakePersonalClient) State() personalwhatsapp.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}
func (f *fakePersonalClient) Connect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	f.state.Connected = true
	return nil
}
func (f *fakePersonalClient) Pair(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairs++
	if f.pairError {
		return "", errors.New("PRIVATE PHONE OR CREDENTIAL DETAIL")
	}
	return "ABCD-EFGH", nil
}
func (f *fakePersonalClient) Send(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends++
	f.outputs = append(f.outputs, text)
	if f.sendError {
		return errors.New("delivery unknown")
	}
	return nil
}
func (f *fakePersonalClient) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	f.state = personalwhatsapp.State{}
	return nil
}
func (f *fakePersonalClient) Close() error { f.mu.Lock(); defer f.mu.Unlock(); f.closes++; return nil }
func personalFixture(t *testing.T) (*Server, *fakePersonalClient) {
	t.Helper()
	s, _ := controlFixture(t)
	s.personalWhatsApp = openPersonalWhatsAppStore(s.config.StateDir)
	f := &fakePersonalClient{state: personalwhatsapp.State{Linked: true, Connected: true}}
	s.personalWhatsApp.replaceClient(f)
	err := s.personalWhatsApp.update("", func(r *personalWhatsAppRecord) error {
		r.Enabled = true
		r.Epoch = strings.Repeat("a", 32)
		r.StartedAt = time.Now().Add(-time.Minute)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}
func TestPersonalWhatsAppPairingRequiresLocalAuthAndNeverExposesSecrets(t *testing.T) {
	s, desktop, browser := testController(t, "")
	f := &fakePersonalClient{}
	s.personalWhatsApp.lifecycle.Lock()
	s.personalWhatsApp.factory = func(context.Context, string, func(personalwhatsapp.Message)) (personalwhatsapp.Client, error) {
		return f, nil
	}
	s.personalWhatsApp.lifecycle.Unlock()
	path := desktop.URL + "/api/mastermind/control/whatsapp-personal"
	if response := request(t, browser, "GET", path, "", "", ""); response.StatusCode != 401 {
		t.Fatal(response.StatusCode)
	}
	login(t, s, browser)
	if response := request(t, browser, "POST", path, `{"phone":"+19995550123"}`, "", desktop.URL); response.StatusCode != 403 {
		t.Fatal(response.StatusCode)
	}
	if response := request(t, browser, "POST", path, `{"phone":"+19995550123"}`, s.csrf, "https://evil.invalid"); response.StatusCode != 403 {
		t.Fatal(response.StatusCode)
	}
	response := request(t, browser, "POST", path, `{"phone":"+19995550123"}`, s.csrf, desktop.URL)
	var pairing struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&pairing) != nil || pairing.Code != "ABCD-EFGH" || !pairing.ExpiresAt.After(time.Now()) {
		t.Fatal("pair response", response.StatusCode)
	}
	for _, route := range []string{path, desktop.URL + "/api/mastermind/control"} {
		response = request(t, browser, "GET", route, "", "", desktop.URL)
		data, _ := io.ReadAll(response.Body)
		if strings.Contains(string(data), pairing.Code) || strings.Contains(string(data), "19995550123") {
			t.Fatal("pairing secret exposed in polling state")
		}
	}
	data, err := os.ReadFile(s.personalWhatsApp.path)
	if err != nil || strings.Contains(string(data), pairing.Code) || strings.Contains(string(data), "19995550123") {
		t.Fatal("pairing secret persisted", err)
	}
	if response = request(t, browser, "POST", path, `{"phone":"+19995550999"}`, s.csrf, desktop.URL); response.StatusCode != 409 {
		t.Fatal("pending link replaced", response.StatusCode)
	}
	response = request(t, browser, "DELETE", path, "", s.csrf, desktop.URL)
	if response.StatusCode != 200 || s.personalWhatsApp.snapshot().Enabled {
		t.Fatal("explicit disconnect failed", response.StatusCode)
	}
	f.mu.Lock()
	pairs, closes, logouts := f.pairs, f.closes, f.logouts
	f.mu.Unlock()
	if pairs != 1 || closes != 1 || logouts != 1 {
		t.Fatal("unexpected link operations", pairs, closes, logouts)
	}
}
func TestPersonalWhatsAppPairingFailureIsSanitizedAndNotReplayed(t *testing.T) {
	s, _ := controlFixture(t)
	s.personalWhatsApp = openPersonalWhatsAppStore(s.config.StateDir)
	f := &fakePersonalClient{pairError: true}
	s.personalWhatsApp.factory = func(context.Context, string, func(personalwhatsapp.Message)) (personalwhatsapp.Client, error) {
		return f, nil
	}
	mux := http.NewServeMux()
	s.registerPersonalWhatsApp(mux)
	w := workspaceCall(t, mux, "POST", "/api/mastermind/control/whatsapp-personal", map[string]string{"phone": "19995550123"})
	if w.Code != 502 || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = workspaceCall(t, mux, "POST", "/api/mastermind/control/whatsapp-personal", map[string]string{"phone": "19995550123"}); w.Code != 409 {
		t.Fatal("unknown pairing repeated", w.Code)
	}
	if f.pairs != 1 {
		t.Fatal("pairing retried", f.pairs)
	}
}
func TestPersonalWhatsAppIncomingAndOutgoingReplayProtectionSurvivesReopen(t *testing.T) {
	s, f := personalFixture(t)
	epoch := s.personalWhatsApp.snapshot().Epoch
	message := personalwhatsapp.Message{ID: "MESSAGE_ONE", Text: "inspect fixture only", At: time.Now().UTC()}
	for range 2 {
		if err := s.handlePersonalWhatsApp(epoch, message); err != nil {
			t.Fatal(err)
		}
	}
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 1 || state.Messages[0].Source != "whatsapp-personal" || f.sends != 1 {
		t.Fatal("duplicate input or delivery", len(state.Messages), f.sends)
	}
	s.personalWhatsApp = openPersonalWhatsAppStore(s.config.StateDir)
	s.personalWhatsApp.replaceClient(f)
	if err := s.handlePersonalWhatsApp(epoch, message); err != nil {
		t.Fatal(err)
	}
	if f.sends != 1 {
		t.Fatal("reopen replayed delivery")
	}
	item := state.Messages[0]
	if _, err := s.control.claim(item.ID, "fixture-launch"); err != nil {
		t.Fatal(err)
	}
	if err := s.control.complete(item.ID, "fixture-launch", "Verified fixture result"); err != nil {
		t.Fatal(err)
	}
	f.sendError = true
	s.deliverPersonalWhatsApp(s.personalWhatsApp.snapshot())
	s.deliverPersonalWhatsApp(s.personalWhatsApp.snapshot())
	state, _, _ = s.control.snapshot()
	if state.Messages[0].Delivery != "unknown" || f.sends != 2 {
		t.Fatal("unknown delivery replayed", state.Messages[0].Delivery, f.sends)
	}
	otherEpoch := strings.Repeat("b", 32)
	if err := s.personalWhatsApp.update("", func(r *personalWhatsAppRecord) error { r.Epoch = otherEpoch; r.Seen = nil; r.Sent = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	s.deliverPersonalWhatsApp(s.personalWhatsApp.snapshot())
	if f.sends != 2 {
		t.Fatal("old account result delivered to a new link")
	}
}
func TestPersonalWhatsAppFreshnessAndRateBounds(t *testing.T) {
	s, _ := personalFixture(t)
	epoch := s.personalWhatsApp.snapshot().Epoch
	for _, at := range []time.Time{time.Time{}, time.Now().Add(-11 * time.Minute), time.Now().Add(2 * time.Minute)} {
		if err := s.handlePersonalWhatsApp(epoch, personalwhatsapp.Message{ID: "STALE", Text: "do not queue", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.handlePersonalWhatsApp("wrong-epoch", personalwhatsapp.Message{ID: "WRONG", Text: "do not queue", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 0 {
		t.Fatal("old or different-link message queued")
	}
	for i := 0; i < 8; i++ {
		if err := s.handlePersonalWhatsApp(epoch, personalwhatsapp.Message{ID: strings.Repeat("A", i+1), Text: "status", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.handlePersonalWhatsApp(epoch, personalwhatsapp.Message{ID: "TOO_MANY", Text: "status", At: time.Now()}); err == nil {
		t.Fatal("command rate unbounded")
	}
}
func TestPersonalWhatsAppRevocationFencesInstructionAdmission(t *testing.T) {
	s, _ := personalFixture(t)
	epoch := s.personalWhatsApp.snapshot().Epoch
	if err := s.personalWhatsApp.update(epoch, func(record *personalWhatsAppRecord) error {
		record.Enabled = false
		record.Epoch = strings.Repeat("b", 32)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueuePersonalInstruction(epoch, strings.Repeat("c", 32), "do not queue after disconnect", "whatsapp-personal:"+epoch+":OLD"); err == nil {
		t.Fatal("revoked account queued an instruction")
	}
	s.personalWhatsApp.setStatus("Disconnected")
	s.personalWhatsApp.setEpochStatus(epoch, "Late failure from an old account")
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 0 || s.personalWhatsApp.view().Status != "Disconnected" {
		t.Fatal("revoked event changed current control state")
	}
}
func TestPersonalWhatsAppApprovalConsumesBeforeUncertainSend(t *testing.T) {
	s, _ := personalFixture(t)
	now := time.Now().UTC()
	approval := controlApproval{ID: strings.Repeat("1", 32), SessionID: strings.Repeat("2", 32), SessionCreatedAt: now, Status: "pending", ExpiresAt: now.Add(time.Minute)}
	var posts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"requests": []controlApproval{approval}})
			return
		}
		posts.Add(1)
		connection, _, _ := w.(http.Hijacker).Hijack()
		connection.Close()
	}))
	defer backend.Close()
	s.store.hosts = []Host{{ID: "fixture", Status: "online"}}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(backend.URL, "http://"))
	}}
	defer tr.CloseIdleConnections()
	s.links["fixture"] = &link{httpTransport: tr}
	ticket := controlApprovalTicket{ID: strings.Repeat("3", 32), Host: "fixture", Request: approval.ID, Session: approval.SessionID, CreatedAt: now, ExpiresAt: approval.ExpiresAt, CanAllow: false}
	err := s.personalWhatsApp.update("", func(r *personalWhatsAppRecord) error { r.Tickets = []controlApprovalTicket{ticket}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	record := s.personalWhatsApp.snapshot()
	if result := s.personalDecision(record, ticket.ID, "allow"); !strings.Contains(result, "complete") || posts.Load() != 0 {
		t.Fatal("truncated request allowed", result, posts.Load())
	}
	if result := s.personalDecision(record, ticket.ID, "deny"); !strings.Contains(result, "unknown") || posts.Load() != 1 {
		t.Fatal(result, posts.Load())
	}
	s.personalDecision(record, ticket.ID, "deny")
	if posts.Load() != 1 || !s.personalWhatsApp.snapshot().Tickets[0].Used {
		t.Fatal("unknown decision replayed", posts.Load())
	}
}
