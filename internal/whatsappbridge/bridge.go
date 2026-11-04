// Package whatsappbridge receives signed Meta webhooks on a separate Linux
// host. It holds no Cloud API access token and cannot execute Relay commands.
package whatsappbridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	AppSecret     string `json:"appSecret"`
	VerifyToken   string `json:"verifyToken"`
	PollToken     string `json:"pollToken"`
	PhoneNumberID string `json:"phoneNumberId"`
	Owner         string `json:"owner"`
}
type Event struct {
	ID            string    `json:"id"`
	Sender        string    `json:"sender"`
	PhoneNumberID string    `json:"phoneNumberId"`
	Text          string    `json:"text"`
	SentAt        time.Time `json:"sentAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}
type receipt struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type record struct {
	Format        int       `json:"format"`
	PhoneNumberID string    `json:"phoneNumberId"`
	Owner         string    `json:"owner"`
	Events        []Event   `json:"events"`
	Receipts      []receipt `json:"receipts"`
}
type Bridge struct {
	mu      sync.Mutex
	config  Config
	path    string
	record  record
	failed  bool
	release func()
	now     func() time.Time
	write   func(string, []byte) error
}

var Digits = regexp.MustCompile(`^[1-9][0-9]{5,24}$`)
var secretPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validConfig(c Config) bool {
	return len(c.AppSecret) >= 16 && len(c.AppSecret) <= 256 && len(c.VerifyToken) >= 32 && len(c.VerifyToken) <= 256 && secretPattern.MatchString(c.PollToken) && Digits.MatchString(c.PhoneNumberID) && Digits.MatchString(c.Owner)
}
func strict(data []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := privateRead(path, 8192)
	if err == nil {
		err = strict(data, &c)
	}
	if err != nil || !validConfig(c) {
		return Config{}, errors.New("Bridge configuration must be a private regular JSON file with valid secrets, phoneNumberId and owner")
	}
	return c, nil
}
func New(dir string, c Config) (*Bridge, error) {
	if !validConfig(c) {
		return nil, errors.New("invalid bridge configuration")
	}
	release, err := lockDirectory(dir)
	if err != nil {
		return nil, err
	}
	b := &Bridge{config: c, path: filepath.Join(dir, "queue.json"), release: release, now: time.Now, write: privateWrite, record: record{Format: 1, PhoneNumberID: c.PhoneNumberID, Owner: c.Owner, Events: []Event{}, Receipts: []receipt{}}}
	data, err := privateRead(b.path, 3<<20)
	if isNotExist(err) {
		return b, nil
	}
	if err != nil || strict(data, &b.record) != nil || !b.validRecord() {
		release()
		return nil, errors.New("Bridge queue is invalid or belongs to another owner; existing bytes were preserved")
	}
	return b, nil
}
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failed = true
	if b.release != nil {
		b.release()
		b.release = nil
	}
}
func validEvent(e Event) bool {
	return len(e.ID) > 0 && len(e.ID) <= 256 && !strings.ContainsAny(e.ID, "\x00\r\n") && Digits.MatchString(e.Sender) && Digits.MatchString(e.PhoneNumberID) && utf8.ValidString(e.Text) && len(e.Text) <= 16000 && utf8.RuneCountInString(e.Text) <= 8000 && !strings.ContainsRune(e.Text, 0) && !e.SentAt.IsZero() && e.ExpiresAt.Equal(e.SentAt.Add(10*time.Minute))
}
func (b *Bridge) validRecord() bool {
	r := b.record
	if r.Format != 1 || r.PhoneNumberID != b.config.PhoneNumberID || r.Owner != b.config.Owner || len(r.Events) > 256 || len(r.Receipts) > 2048 {
		return false
	}
	seen := map[string]bool{}
	receipts := map[string]time.Time{}
	for _, receipt := range r.Receipts {
		if receipt.ID == "" || len(receipt.ID) > 256 || strings.ContainsAny(receipt.ID, "\x00\r\n") || receipt.ExpiresAt.IsZero() {
			return false
		}
		if _, exists := receipts[receipt.ID]; exists {
			return false
		}
		receipts[receipt.ID] = receipt.ExpiresAt
	}
	for _, e := range r.Events {
		if !validEvent(e) || e.Sender != r.Owner || e.PhoneNumberID != r.PhoneNumberID || seen[e.ID] || !receipts[e.ID].Equal(e.ExpiresAt) {
			return false
		}
		seen[e.ID] = true
	}
	return true
}
func (b *Bridge) save(next record) error {
	data, err := json.Marshal(next)
	if err != nil || len(data) > 3<<20 {
		return errors.New("bridge queue capacity exceeded")
	}
	if err = b.write(b.path, data); err != nil {
		b.failed = true
		return errors.New("Bridge queue save was not confirmed; restart the bridge after checking storage")
	}
	b.record = next
	return nil
}
func clean(r record, now time.Time) record {
	r.Events = append([]Event{}, r.Events...)
	r.Receipts = append([]receipt{}, r.Receipts...)
	events := r.Events[:0]
	for _, e := range r.Events {
		if now.Before(e.ExpiresAt) {
			events = append(events, e)
		}
	}
	r.Events = events
	receipts := r.Receipts[:0]
	for _, e := range r.Receipts {
		if now.Before(e.ExpiresAt) {
			receipts = append(receipts, e)
		}
	}
	r.Receipts = receipts
	return r
}
func response(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webhook", b.verify)
	mux.HandleFunc("POST /webhook", b.webhook)
	mux.HandleFunc("GET /api/info", b.authorize(b.info))
	mux.HandleFunc("GET /api/events", b.authorize(b.events))
	mux.HandleFunc("POST /api/events/ack", b.authorize(b.ack))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}
func (b *Bridge) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			if strings.EqualFold(name, "Origin") || strings.EqualFold(name, "Cookie") || strings.HasPrefix(strings.ToLower(name), "sec-fetch-") {
				http.Error(w, "Browser access is not allowed", 403)
				return
			}
		}
		values := r.Header.Values("Authorization")
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+b.config.PollToken)) != 1 {
			http.Error(w, "Unauthorized", 401)
			return
		}
		next(w, r)
	}
}
func (b *Bridge) verify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" || len(q["hub.verify_token"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(b.config.VerifyToken)) != 1 || len(q["hub.challenge"]) != 1 || len(q.Get("hub.challenge")) > 256 {
		http.Error(w, "Verification rejected", 403)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, q.Get("hub.challenge"))
}
func (b *Bridge) info(w http.ResponseWriter, r *http.Request) {
	response(w, 200, map[string]any{"protocol": 1, "phoneNumberId": b.config.PhoneNumberID, "owner": b.config.Owner})
}
func (b *Bridge) events(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		http.Error(w, "Bridge storage needs review", 503)
		return
	}
	events := []Event{}
	now := b.now()
	for _, e := range b.record.Events {
		if now.Before(e.ExpiresAt) {
			events = append(events, e)
			if len(events) == 32 {
				break
			}
		}
	}
	response(w, 200, map[string]any{"protocol": 1, "events": events})
}
func (b *Bridge) ack(w http.ResponseWriter, r *http.Request) {
	var value struct {
		IDs []string `json:"ids"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || strict(data, &value) != nil || len(value.IDs) > 32 {
		http.Error(w, "Invalid acknowledgment", 400)
		return
	}
	ids := map[string]bool{}
	for _, id := range value.IDs {
		if id == "" || len(id) > 256 {
			http.Error(w, "Invalid identity", 400)
			return
		}
		ids[id] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		http.Error(w, "Bridge storage needs review", 503)
		return
	}
	next := clean(b.record, b.now())
	events := next.Events[:0]
	for _, e := range next.Events {
		if !ids[e.ID] {
			events = append(events, e)
		}
	}
	next.Events = events
	if err = b.save(next); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	response(w, 200, map[string]bool{"ok": true})
}
func (b *Bridge) webhook(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Webhook body too large", 413)
		return
	}
	signatures := r.Header.Values("X-Hub-Signature-256")
	if len(signatures) != 1 || !strings.HasPrefix(signatures[0], "sha256=") {
		http.Error(w, "Signature required", 401)
		return
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(signatures[0], "sha256="))
	mac := hmac.New(sha256.New, []byte(b.config.AppSecret))
	_, _ = mac.Write(data)
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		http.Error(w, "Signature rejected", 401)
		return
	}
	var payload struct {
		Object string `json:"object"`
		Entry  []struct {
			Changes []struct {
				Field string `json:"field"`
				Value struct {
					Metadata struct {
						PhoneNumberID string `json:"phone_number_id"`
					} `json:"metadata"`
					Messages []struct {
						ID        string `json:"id"`
						From      string `json:"from"`
						Timestamp string `json:"timestamp"`
						Type      string `json:"type"`
						Text      struct {
							Body string `json:"body"`
						} `json:"text"`
					} `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if json.Unmarshal(data, &payload) != nil {
		http.Error(w, "Invalid webhook", 400)
		return
	}
	if payload.Object != "whatsapp_business_account" {
		response(w, 200, map[string]bool{"ok": true})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		http.Error(w, "Bridge storage needs review", 503)
		return
	}
	now := b.now()
	next := clean(b.record, now)
	seen := map[string]bool{}
	for _, r := range next.Receipts {
		seen[r.ID] = true
	}
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" || change.Value.Metadata.PhoneNumberID != b.config.PhoneNumberID {
				continue
			}
			for _, message := range change.Value.Messages {
				sent, parseErr := parseTimestamp(message.Timestamp)
				e := Event{ID: message.ID, Sender: message.From, PhoneNumberID: b.config.PhoneNumberID, Text: message.Text.Body, SentAt: sent, ExpiresAt: sent.Add(10 * time.Minute)}
				if parseErr != nil || message.Type != "text" || message.From != b.config.Owner || !validEvent(e) || !now.Before(e.ExpiresAt) || sent.After(now.Add(time.Minute)) || seen[e.ID] {
					continue
				}
				if len(next.Events) >= 256 || len(next.Receipts) >= 2048 {
					http.Error(w, "Bridge queue full; retry later", 503)
					return
				}
				seen[e.ID] = true
				next.Events = append(next.Events, e)
				next.Receipts = append(next.Receipts, receipt{ID: e.ID, ExpiresAt: e.ExpiresAt})
			}
		}
	}
	if err = b.save(next); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	response(w, 200, map[string]bool{"ok": true})
}
