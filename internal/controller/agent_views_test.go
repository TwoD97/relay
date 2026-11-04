package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/viewrequests"
)

func TestAgentViewsRequireCapabilityExactHostAndNeverForwardCredentials(t *testing.T) {
	s, _ := workspaceFixture(t)
	s.ctx = context.Background()
	s.links = map[string]*link{}
	s.store = &hostStore{hosts: []Host{{ID: "fixture", Status: "online"}}}
	launch := strings.Repeat("a", 32)
	s.mastermind = &mastermindStore{record: mastermindRecord{Token: launch, MastermindState: MastermindState{Enabled: true}}}
	if _, err := s.PrepareWorkspaceAgentCapability(launch); err != nil {
		t.Fatal(err)
	}
	credential, err := readWorkspaceCapability(s.config.StateDir, launch)
	if err != nil {
		t.Fatal(err)
	}
	ref := viewrequests.Ref{SessionID: strings.Repeat("b", 32), SessionCreatedAt: time.Now().UTC()}
	var creates atomic.Int32
	var drop atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"Authorization", "Cookie", "X-Relay-CSRF"} {
			if r.Header.Get(key) != "" {
				t.Error("controller credential copied to runtime")
			}
		}
		if r.Method == "GET" && r.URL.Path == "/api/sessions" {
			writeJSON(w, 200, []map[string]any{{"id": ref.SessionID, "createdAt": ref.SessionCreatedAt, "purpose": ""}})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/views" {
			t.Error("view request mutated a session")
			w.WriteHeader(500)
			return
		}
		creates.Add(1)
		if drop.Load() {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		writeJSON(w, 202, viewrequests.Request{ID: strings.Repeat("c", 32), Ref: ref, Status: "pending"})
	}))
	defer backend.Close()
	l := s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Listener.Addr().String())
	})
	defer l.httpTransport.CloseIdleConnections()
	s.links["fixture"] = l
	handler := s.AgentHandler()
	call := func(token, host string, reference viewrequests.Ref, origin bool) int {
		data, _ := json.Marshal(struct {
			HostID string `json:"hostId"`
			viewrequests.Ref
		}{host, reference})
		req := httptest.NewRequest("POST", "/agent/views", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin {
			req.Header.Set("Origin", "http://127.0.0.1")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	if call("", "fixture", ref, false) != 401 || call(credential.Token, "fixture", ref, true) != 403 {
		t.Fatal("view request bypassed private capability/browser separation")
	}
	wrong := ref
	wrong.SessionCreatedAt = wrong.SessionCreatedAt.Add(time.Nanosecond)
	if call(credential.Token, "fixture", wrong, false) != 409 || creates.Load() != 0 {
		t.Fatal("stale target caused a window request")
	}
	if call(credential.Token, "other", ref, false) != 503 || creates.Load() != 0 {
		t.Fatal("request crossed host boundary")
	}
	if call(credential.Token, "fixture", ref, false) != 202 || creates.Load() != 1 {
		t.Fatal("valid viewer request failed")
	}
	drop.Store(true)
	if call(credential.Token, "fixture", ref, false) != 502 || creates.Load() != 2 {
		t.Fatal("unknown viewer request was replayed")
	}
}
