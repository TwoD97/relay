//go:build linux

package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestSessionStopPreservesExactRecordAndHistory(t *testing.T) {
	s := testServer(t)
	p := startTest(t, s, "printf 'STOP PROOF\\n'; cat")
	ready(t, p)
	eventually(t, func() bool { return strings.Contains(historyOf(p), "STOP PROOF") })
	meta := p.snapshot()
	bad := restartCall(s, "POST", "/api/sessions/"+meta.ID+"/stop", map[string]any{"createdAt": meta.CreatedAt.Add(time.Second)})
	if bad.Code != 409 || p.snapshot().Status != "running" {
		t.Fatal("wrong identity stopped")
	}
	good := restartCall(s, "POST", "/api/sessions/"+meta.ID+"/stop", map[string]any{"createdAt": meta.CreatedAt})
	if good.Code != 200 {
		t.Fatal(good.Code, good.Body.String())
	}
	saved, err := s.lookup(meta.ID)
	if err != nil || saved.snapshot().Status == "running" || !strings.Contains(historyOf(saved), "STOP PROOF") {
		t.Fatal("stop removed terminal")
	}
}
