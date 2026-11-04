//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/viewrequests"
)

func TestSessionOpenUsesExactIdentityAndDoesNotCreateOrSend(t *testing.T) {
	ref := viewrequests.Ref{SessionID: strings.Repeat("a", 32), SessionCreatedAt: time.Now().UTC()}
	var posts atomic.Int32
	socket := sessionCLISocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/sessions" {
			json.NewEncoder(w).Encode([]any{map[string]any{"id": ref.SessionID, "createdAt": ref.SessionCreatedAt}})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/views" {
			t.Error("window request created a session or sent input")
			w.WriteHeader(500)
			return
		}
		posts.Add(1)
		var got viewrequests.Ref
		json.NewDecoder(r.Body).Decode(&got)
		if !viewrequests.SameRef(got, ref) {
			t.Error("creation identity changed")
		}
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	}))
	data, err := runSessionCLI(t, "open", "--socket", socket, "--id", ref.SessionID)
	if err != nil || !strings.Contains(string(data), "pending") || posts.Load() != 1 {
		t.Fatal(err, string(data), posts.Load())
	}
}
func TestSessionOpenUnknownOutcomeIsNotRetried(t *testing.T) {
	var posts atomic.Int32
	id := strings.Repeat("a", 32)
	socket := sessionCLISocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]any{map[string]any{"id": id, "createdAt": time.Now().UTC()}})
			return
		}
		posts.Add(1)
		connection, _, _ := w.(http.Hijacker).Hijack()
		connection.Close()
	}))
	if _, err := runSessionCLI(t, "open", "--socket", socket, "--id", id); err == nil {
		t.Fatal("uncertain request reported success")
	}
	if posts.Load() != 1 {
		t.Fatal("window mutation replayed")
	}
}
