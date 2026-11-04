package viewrequests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func call(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestWindowRequestsClaimOnceValidateIdentityAndNeverReplay(t *testing.T) {
	ref := Ref{SessionID: strings.Repeat("a", 32), SessionCreatedAt: time.Now().UTC()}
	valid := true
	q := New(func(candidate Ref) bool { return valid && SameRef(candidate, ref) })
	mux := http.NewServeMux()
	q.Register(mux)
	w := call(t, mux, "POST", "/api/views", ref)
	var request Request
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &request) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if call(t, mux, "POST", "/api/views", ref).Code != 409 {
		t.Fatal("duplicate pending request admitted")
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			results <- call(t, mux, "POST", "/api/views/"+request.ID+"/claim", map[string]string{"clientId": id}).Code
		}(id)
	}
	wg.Wait()
	close(results)
	sum := 0
	for code := range results {
		sum += code
	}
	if sum != 609 {
		t.Fatal("two desktops could claim the same window")
	}
	q.mu.Lock()
	owner := q.requests[request.ID].client
	q.mu.Unlock()
	if call(t, mux, "POST", "/api/views/"+request.ID+"/claim", map[string]string{"clientId": owner}).Code != 409 {
		t.Fatal("unknown claim reply can be replayed")
	}
	if call(t, mux, "POST", "/api/views/"+request.ID+"/complete", map[string]string{"clientId": strings.Repeat("d", 32), "status": "opened"}).Code != 409 {
		t.Fatal("different desktop completed claim")
	}
	if call(t, mux, "POST", "/api/views/"+request.ID+"/complete", map[string]string{"clientId": owner, "status": "opened"}).Code != 200 {
		t.Fatal("owner completion rejected")
	}
	valid = false
	if call(t, mux, "POST", "/api/views", ref).Code != 409 {
		t.Fatal("stale session accepted")
	}
	if call(t, mux, "POST", "/api/views", map[string]any{"sessionId": ref.SessionID, "sessionCreatedAt": ref.SessionCreatedAt, "url": "https://untrusted.example"}).Code != 400 {
		t.Fatal("arbitrary URL accepted")
	}
}
func TestWindowRequestsBoundExpiryUncertainClaimsAndNewRuntime(t *testing.T) {
	now := time.Now().UTC()
	q := New(func(Ref) bool { return true })
	q.now = func() time.Time { return now }
	mux := http.NewServeMux()
	q.Register(mux)
	var first Request
	for i := 0; i < 8; i++ {
		id := strings.Repeat(string(rune('a'+i%6)), 31) + string("01234567"[i])
		w := call(t, mux, "POST", "/api/views", Ref{id, now})
		if w.Code != 202 {
			t.Fatalf("enqueue %d: %d", i, w.Code)
		}
		if i == 0 {
			json.Unmarshal(w.Body.Bytes(), &first)
		}
	}
	if call(t, mux, "POST", "/api/views", Ref{strings.Repeat("f", 32), now}).Code != 429 {
		t.Fatal("unbounded window creation")
	}
	if call(t, mux, "POST", "/api/views/"+first.ID+"/claim", map[string]string{"clientId": strings.Repeat("b", 32)}).Code != 200 {
		t.Fatal("claim failed")
	}
	now = now.Add(3 * time.Minute)
	w := call(t, mux, "GET", "/api/views", nil)
	var list struct {
		Requests []Request `json:"requests"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	expired, uncertain := 0, 0
	for _, r := range list.Requests {
		if r.Status == "expired" {
			expired++
		}
		if r.Status == "uncertain" {
			uncertain++
		}
	}
	if expired != 7 || uncertain != 1 {
		t.Fatalf("bad expiry %d/%d", expired, uncertain)
	}
	if call(t, mux, "POST", "/api/views/"+first.ID+"/claim", map[string]string{"clientId": strings.Repeat("c", 32)}).Code != 409 {
		t.Fatal("uncertain window was replayed")
	}
	fresh := New(func(Ref) bool { return true })
	next := http.NewServeMux()
	fresh.Register(next)
	if strings.Contains(call(t, next, "GET", "/api/views", nil).Body.String(), first.ID) {
		t.Fatal("new runtime replayed transient queue")
	}
}
