package controller

import (
	"crypto/sha256"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestFreshLoginLinksPreserveExistingClients(t *testing.T) {
	s, ts, first := testController(t, "")
	login(t, s, first)
	csrf, session := s.csrf, s.sessionToken
	link, err := s.IssueLoginURL()
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	second := &http.Client{Jar: jar, Timeout: time.Second}
	response, err := second.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || s.csrf != csrf || s.sessionToken != session {
		t.Fatal("issuing another login replaced the existing client credentials")
	}
	for _, client := range []*http.Client{first, second} {
		response := request(t, client, http.MethodGet, ts.URL+"/api/bootstrap", "", "", ts.URL)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("one of the authenticated clients stopped working: %d", response.StatusCode)
		}
		response = request(t, client, http.MethodPost, ts.URL+"/api/hosts", `{"target":"unsafe;target"}`, csrf, ts.URL)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("existing CSRF token stopped working: %d", response.StatusCode)
		}
	}
	if got := request(t, second, http.MethodGet, link, "", "", ""); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fresh link could be used twice: %d", got.StatusCode)
	}
	if got := request(t, first, http.MethodPost, ts.URL+"/api/launch", "", csrf, ts.URL); got.StatusCode != http.StatusNotFound {
		t.Fatal("web API exposes a login-link issuer")
	}
}

func TestLoginLinkRedemptionIsAtomic(t *testing.T) {
	s, _, _ := testController(t, "")
	link, err := s.IssueLoginURL()
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, err := client.Get(link)
			if err != nil {
				statuses <- 0
				return
			}
			response.Body.Close()
			statuses <- response.StatusCode
		})
	}
	workers.Wait()
	close(statuses)
	accepted := 0
	for status := range statuses {
		if status == http.StatusSeeOther {
			accepted++
		} else if status != http.StatusUnauthorized {
			t.Fatalf("unexpected redemption status %d", status)
		}
	}
	if accepted != 1 {
		t.Fatalf("one-time link authenticated %d concurrent requests", accepted)
	}
}

func TestLoginLinksExpireAndRemainBounded(t *testing.T) {
	s, _, client := testController(t, "")
	link, err := s.IssueLoginURL()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	key := sha256.Sum256([]byte(u.Query().Get("token")))
	s.authMu.Lock()
	s.launchTokens[key] = time.Now().Add(-time.Second)
	s.authMu.Unlock()
	if got := request(t, client, http.MethodGet, link, "", "", ""); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired link accepted: %d", got.StatusCode)
	}
	for range maxLaunchTokens - 1 { // The unused startup link occupies one slot.
		if _, err := s.IssueLoginURL(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.IssueLoginURL(); err == nil {
		t.Fatal("outstanding launch capabilities are unbounded")
	}
	// Reaching the cap must not revoke links already handed to another client.
	login(t, s, client)
	if _, err := s.IssueLoginURL(); err != nil {
		t.Fatal("redeeming a link did not free capacity", err)
	}
	s.authMu.Lock()
	for key := range s.launchTokens {
		s.launchTokens[key] = time.Now().Add(-time.Second)
	}
	s.authMu.Unlock()
	if _, err := s.IssueLoginURL(); err != nil {
		t.Fatal("expired capabilities did not free capacity", err)
	}
}
