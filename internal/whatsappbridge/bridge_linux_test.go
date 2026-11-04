package whatsappbridge

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureConfig() Config {
	return Config{AppSecret: strings.Repeat("s", 32), VerifyToken: strings.Repeat("v", 32), PollToken: strings.Repeat("a", 64), PhoneNumberID: "12345678", Owner: "49123456789"}
}
func fixtureBridge(t *testing.T) (*Bridge, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bridge")
	b, e := New(dir, fixtureConfig())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(b.Close)
	return b, dir
}
func fixtureWebhook(b *Bridge, id, sender, phone, kind string, sent time.Time) []byte {
	return []byte(fmt.Sprintf(`{"object":"whatsapp_business_account","entry":[{"changes":[{"field":"messages","value":{"metadata":{"phone_number_id":%q},"messages":[{"id":%q,"from":%q,"timestamp":%q,"type":%q,"text":{"body":"inspect the fixture"}}]}}]}]}`, phone, id, sender, fmt.Sprint(sent.Unix()), kind))
}
func signed(b *Bridge, data []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/webhook", bytes.NewReader(data))
	m := hmac.New(sha256.New, []byte(b.config.AppSecret))
	m.Write(data)
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	return w
}
func privateCall(b *Bridge, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+b.config.PollToken)
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	return w
}

func TestWhatsAppBusinessBridgeSignedDurableQueue(t *testing.T) {
	b, dir := fixtureBridge(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := fixtureWebhook(b, "wamid.fixture", b.config.Owner, b.config.PhoneNumberID, "text", now)
	for range 2 {
		if w := signed(b, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(b.record.Events) != 1 {
		t.Fatal("duplicate enqueued")
	}
	b.Close()
	var err error
	b, err = New(dir, fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(b.record.Events) != 1 || b.record.Events[0].Text != "inspect the fixture" {
		t.Fatal("durable message lost")
	}
	if w := privateCall(b, "POST", "/api/events/ack", `{"ids":["wamid.fixture"]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	b.Close()
	b, err = New(dir, fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if w := signed(b, body); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(b.record.Events) != 0 || len(b.record.Receipts) != 1 {
		t.Fatal("acknowledged webhook replayed after restart")
	}
	info, err := os.Stat(filepath.Join(dir, "queue.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("queue permissions", err)
	}
}
func TestWhatsAppBusinessBridgeAuthenticationAndFiltering(t *testing.T) {
	b, _ := fixtureBridge(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, change := range []struct {
		id, sender, phone, kind string
		sent                    time.Time
	}{{"owner", "49999999999", b.config.PhoneNumberID, "text", now}, {"phone", b.config.Owner, "87654321", "text", now}, {"kind", b.config.Owner, b.config.PhoneNumberID, "image", now}, {"old", b.config.Owner, b.config.PhoneNumberID, "text", now.Add(-11 * time.Minute)}, {"future", b.config.Owner, b.config.PhoneNumberID, "text", now.Add(2 * time.Minute)}} {
		if w := signed(b, fixtureWebhook(b, change.id, change.sender, change.phone, change.kind, change.sent)); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if len(b.record.Events) != 0 {
		t.Fatal("untrusted/expired event accepted")
	}
	r := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{}`))
	r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, header := range []string{"Origin", "Cookie", "Sec-Fetch-Site"} {
		r = httptest.NewRequest("GET", "/api/events", nil)
		r.Header.Set("Authorization", "Bearer "+b.config.PollToken)
		r.Header.Set(header, "fixture")
		w = httptest.NewRecorder()
		b.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(header, w.Code)
		}
	}
	r = httptest.NewRequest("GET", "/api/events", nil)
	w = httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	q := url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {b.config.VerifyToken}, "hub.challenge": {"challenge-123"}}
	r = httptest.NewRequest("GET", "/webhook?"+q.Encode(), nil)
	w = httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "challenge-123" {
		t.Fatal("challenge")
	}
	q.Add("hub.verify_token", "other")
	r = httptest.NewRequest("GET", "/webhook?"+q.Encode(), nil)
	w = httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("ambiguous verify token accepted")
	}
}
func TestWhatsAppBusinessBridgeStorageBoundaries(t *testing.T) {
	b, dir := fixtureBridge(t)
	if _, err := New(dir, fixtureConfig()); err == nil {
		t.Fatal("concurrent writer accepted")
	}
	b.write = func(string, []byte) error { return errors.New("fixture disk failure") }
	body := fixtureWebhook(b, "failed", b.config.Owner, b.config.PhoneNumberID, "text", time.Now())
	if w := signed(b, body); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if len(b.record.Events) != 0 {
		t.Fatal("unconfirmed event published")
	}
	if w := privateCall(b, "GET", "/api/events", ""); w.Code != 503 {
		t.Fatal("poisoned store exposed")
	}
	b.Close()
	path := filepath.Join(dir, "queue.json")
	original := []byte(`{"format":99}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, fixtureConfig()); err == nil {
		t.Fatal("corruption accepted")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("corruption overwritten")
	}
	os.Remove(path)
	target := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(target, []byte("untouched"), 0600)
	os.Symlink(target, path)
	if _, err := New(dir, fixtureConfig()); err == nil {
		t.Fatal("symlink followed")
	}
}
func TestWhatsAppBusinessBridgeBoundsAndConfig(t *testing.T) {
	b, _ := fixtureBridge(t)
	if w := signed(b, bytes.Repeat([]byte("x"), (1<<20)+1)); w.Code != 413 {
		t.Fatal(w.Code)
	}
	for i := range 256 {
		event := Event{ID: fmt.Sprint(i), Sender: b.config.Owner, PhoneNumberID: b.config.PhoneNumberID, Text: "fixture", SentAt: time.Now(), ExpiresAt: time.Now()}
		event.ExpiresAt = event.SentAt.Add(10 * time.Minute)
		b.record.Events = append(b.record.Events, event)
		b.record.Receipts = append(b.record.Receipts, receipt{ID: event.ID, ExpiresAt: event.ExpiresAt})
	}
	if w := signed(b, fixtureWebhook(b, "overflow", b.config.Owner, b.config.PhoneNumberID, "text", time.Now())); w.Code != 503 {
		t.Fatal("capacity", w.Code)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(fixtureConfig())
	os.WriteFile(path, data, 0600)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0644)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("public credential file accepted")
	}
}
