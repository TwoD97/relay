// Package viewrequests holds short-lived requests to open an exact terminal
// viewer. It never starts a process, writes terminal input, or supplies a URL.
package viewrequests

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Ref struct {
	SessionID        string    `json:"sessionId"`
	SessionCreatedAt time.Time `json:"sessionCreatedAt"`
}
type Request struct {
	ID string `json:"id"`
	Ref
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	client    string
}
type Queue struct {
	mu       sync.Mutex
	requests map[string]Request
	validate func(Ref) bool
	now      func() time.Time
}

func New(validate func(Ref) bool) *Queue {
	return &Queue{requests: map[string]Request{}, validate: validate, now: time.Now}
}
func ValidID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func ValidRef(ref Ref) bool { return ValidID(ref.SessionID) && !ref.SessionCreatedAt.IsZero() }
func SameRef(a, b Ref) bool {
	return a.SessionID == b.SessionID && a.SessionCreatedAt.Equal(b.SessionCreatedAt)
}

func (q *Queue) expireLocked(now time.Time) {
	for id, r := range q.requests {
		if !now.Before(r.ExpiresAt) {
			if r.Status == "pending" {
				r.Status, r.Detail = "expired", "No desktop opened this request before it expired."
			}
			if r.Status == "claimed" {
				r.Status, r.Detail = "uncertain", "A desktop claimed this request but did not confirm the window. It will not be opened again automatically."
			}
			q.requests[id] = r
		}
	}
}
func (q *Queue) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/views", q.list)
	mux.HandleFunc("POST /api/views", q.create)
	mux.HandleFunc("POST /api/views/{view}/claim", q.claim)
	mux.HandleFunc("POST /api/views/{view}/complete", q.complete)
}
func output(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	output(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, value any) error {
	if r.Context().Err() != nil {
		return r.Context().Err()
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func (q *Queue) list(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked(q.now())
	list := make([]Request, 0, len(q.requests))
	for _, item := range q.requests {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	output(w, 200, map[string]any{"requests": list})
}
func (q *Queue) create(w http.ResponseWriter, r *http.Request) {
	var ref Ref
	if decode(w, r, &ref) != nil || !ValidRef(ref) {
		fail(w, 400, "An exact session ID and creation time are required")
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now().UTC()
	q.expireLocked(now)
	if !q.validate(ref) {
		fail(w, 409, "The terminal session no longer matches this request, or is a setup session")
		return
	}
	active, recent := 0, 0
	for _, item := range q.requests {
		if item.CreatedAt.After(now.Add(-time.Minute)) {
			recent++
		}
		if item.Status == "pending" || item.Status == "claimed" {
			active++
			if SameRef(item.Ref, ref) {
				fail(w, 409, "A window request already exists for this session; inspect it instead of repeating the request")
				return
			}
		}
	}
	if active >= 16 || recent >= 8 {
		fail(w, 429, "Too many terminal window requests. Wait before requesting another window.")
		return
	}
	if len(q.requests) >= 64 {
		var oldest string
		var at time.Time
		for id, item := range q.requests {
			if item.Status != "pending" && item.Status != "claimed" && (oldest == "" || item.CreatedAt.Before(at)) {
				oldest, at = id, item.CreatedAt
			}
		}
		if oldest == "" {
			fail(w, 429, "Terminal window request capacity reached")
			return
		}
		delete(q.requests, oldest)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		fail(w, 500, "Could not create a window request")
		return
	}
	item := Request{ID: hex.EncodeToString(nonce[:]), Ref: ref, CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute), Status: "pending"}
	q.requests[item.ID] = item
	output(w, 202, item)
}
func (q *Queue) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string `json:"clientId"`
	}
	if decode(w, r, &body) != nil || !ValidID(body.ClientID) {
		fail(w, 400, "An exact desktop client identity is required")
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked(q.now())
	item, ok := q.requests[r.PathValue("view")]
	if !ok {
		fail(w, 404, "Window request not found")
		return
	}
	if item.Status != "pending" {
		fail(w, 409, "This request has already been claimed or has expired; it will not be replayed")
		return
	}
	if !q.validate(item.Ref) {
		item.Status, item.Detail = "failed", "The session changed before the window could open."
		q.requests[item.ID] = item
		fail(w, 409, item.Detail)
		return
	}
	item.Status, item.client = "claimed", body.ClientID
	q.requests[item.ID] = item
	output(w, 200, item)
}
func (q *Queue) complete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string `json:"clientId"`
		Status   string `json:"status"`
	}
	if decode(w, r, &body) != nil || !ValidID(body.ClientID) || (body.Status != "opened" && body.Status != "failed") {
		fail(w, 400, "Invalid terminal window outcome")
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked(q.now())
	item, ok := q.requests[r.PathValue("view")]
	if !ok {
		fail(w, 404, "Window request not found")
		return
	}
	if item.Status != "claimed" || item.client != body.ClientID {
		fail(w, 409, "This desktop does not own the pending window outcome")
		return
	}
	item.Status = body.Status
	if body.Status == "failed" {
		item.Detail = "The desktop could not confirm that a window opened. Inspect the desktop before making another request."
	}
	q.requests[item.ID] = item
	output(w, 200, item)
}
