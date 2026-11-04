package localagent

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/viewrequests"
)

func TestLocalViewsPreserveSessionAndRejectSetupOrReusedIdentity(t *testing.T) {
	meta := Session{ID: strings.Repeat("a", 32), CreatedAt: time.Now().UTC(), Status: "running", Harness: "claude", MastermindID: strings.Repeat("b", 32)}
	p := newSession(meta)
	s := &Server{sessions: map[string]*session{meta.ID: p}}
	call := func(ref viewrequests.Ref) int {
		data, _ := json.Marshal(ref)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/api/views", bytes.NewReader(data)))
		return w.Code
	}
	ref := viewrequests.Ref{SessionID: meta.ID, SessionCreatedAt: meta.CreatedAt}
	wrong := ref
	wrong.SessionCreatedAt = wrong.SessionCreatedAt.Add(time.Nanosecond)
	if call(wrong) != 409 {
		t.Fatal("reused identity accepted")
	}
	p.meta.Purpose = "login"
	if call(ref) != 409 {
		t.Fatal("authentication terminal can be opened automatically")
	}
	p.meta.Purpose = ""
	if call(ref) != 202 {
		t.Fatal("Mastermind terminal request failed")
	}
	after := p.snapshot()
	if len(s.sessions) != 1 || after.ID != meta.ID || after.Status != meta.Status || !after.CreatedAt.Equal(meta.CreatedAt) {
		t.Fatal("view request changed local process metadata")
	}
	if !s.Health().Capabilities["viewRequestsV1"] {
		t.Fatal("local native capability missing")
	}
}
