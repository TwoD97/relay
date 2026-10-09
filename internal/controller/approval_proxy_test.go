package controller

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestApprovalProxyHostIsolationAndBrowserSecurity(t *testing.T) {
	s, ts, client := testController(t, "")
	var first, second atomic.Int32
	for index, id := range []string{"host-one", "host-two"} {
		counter := &first
		if index == 1 {
			counter = &second
		}
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			for _, header := range []string{"Authorization", "Cookie", "X-Relay-CSRF"} {
				if r.Header.Get(header) != "" {
					t.Errorf("forwarded browser secret %s", header)
				}
			}
			if strings.HasSuffix(r.URL.Path, "/approval-hook") {
				w.WriteHeader(401)
				return
			}
			if r.URL.Path != "/api/approvals/request/decision" && r.URL.Path != "/api/approvals" {
				t.Errorf("wrong runtime path %s", r.URL.Path)
			}
			w.WriteHeader(200)
		}))
		t.Cleanup(backend.Close)
		target, _ := url.Parse(backend.URL)
		s.links[id] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Host)
		})
		s.store.hosts = append(s.store.hosts, Host{ID: id, Status: "online"})
	}
	path := ts.URL + "/api/hosts/host-one/runtime/approvals/request/decision"
	if got := request(t, client, "POST", path, `{}`, "", ts.URL).StatusCode; got != 401 {
		t.Fatalf("unauthenticated decision: %d", got)
	}
	login(t, s, client)
	if got := request(t, client, "POST", path, `{}`, "", ts.URL).StatusCode; got != 403 {
		t.Fatalf("missing CSRF: %d", got)
	}
	if got := request(t, client, "POST", path, `{}`, s.csrf, "https://other.invalid").StatusCode; got != 403 {
		t.Fatalf("foreign origin: %d", got)
	}
	if first.Load() != 0 || second.Load() != 0 {
		t.Fatal("rejected mutation reached a runtime")
	}
	req, _ := http.NewRequest("POST", path, strings.NewReader(`{"sessionId":"session","sessionCreatedAt":"2026-10-09T00:00:00Z","decision":"allow"}`))
	req.Header.Set("Authorization", "Bearer forged-provider-capability")
	req.Header.Set("X-Relay-CSRF", s.csrf)
	req.Header.Set("Origin", ts.URL)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || first.Load() != 1 || second.Load() != 0 {
		t.Fatal("decision crossed host boundary")
	}
	hook := ts.URL + "/api/hosts/host-one/runtime/sessions/session/approval-hook"
	req, _ = http.NewRequest("POST", hook, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer forged-provider-capability")
	req.Header.Set("X-Relay-CSRF", s.csrf)
	req.Header.Set("Origin", ts.URL)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 || first.Load() != 2 || second.Load() != 0 {
		t.Fatal("browser could impersonate provider hook")
	}
	if got := request(t, client, "GET", ts.URL+"/api/hosts/host-two/runtime/approvals", "", "", ts.URL).StatusCode; got != 200 || second.Load() != 1 {
		t.Fatal("list not scoped to selected host")
	}
}
