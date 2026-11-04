package localagent

import (
	"testing"
	"time"
)

func TestSessionStopPreservesExactRecordAndHistory(t *testing.T) {
	s, server := testService(t)
	session := createFixture(t, s, server, "manual")
	code, _ := requestJSON(t, server.URL+"/api/sessions/"+session.ID+"/stop", "POST", map[string]any{"createdAt": session.CreatedAt.Add(time.Second)})
	p, _ := s.lookup(session.ID)
	if code != 409 || p.snapshot().Status != "running" {
		t.Fatal("wrong identity stopped")
	}
	code, body := requestJSON(t, server.URL+"/api/sessions/"+session.ID+"/stop", "POST", map[string]any{"createdAt": session.CreatedAt})
	if code != 200 {
		t.Fatal(code, string(body))
	}
	if _, err := s.lookup(session.ID); err != nil || p.snapshot().Status == "running" {
		t.Fatal("record removed or process still running")
	}
	awaitHistory(t, s, session.ID, "READY")
}
