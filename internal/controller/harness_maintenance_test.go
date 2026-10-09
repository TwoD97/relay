package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

type maintenanceRoundTrip func(*http.Request) (*http.Response, error)

func (f maintenanceRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaintenanceRuntimeMutationsDoNotRedirectReplayOrForwardBrowserSecrets(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			jar, _ := cookiejar.New(nil)
			origin, _ := url.Parse("http://relay-runtime")
			jar.SetCookies(origin, []*http.Cookie{{Name: "browser-session", Value: "must-not-leak"}})
			client := &http.Client{Jar: jar, Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.URL.Host != "relay-runtime" || r.URL.Path != "/api/sessions/test/input" || r.GetBody != nil {
					t.Fatal("mutation destination or replay capability changed", r.Method, r.URL, r.GetBody != nil)
				}
				for _, key := range []string{"Cookie", "Authorization", "X-Relay-CSRF", "Origin"} {
					if r.Header.Get(key) != "" {
						t.Fatalf("forwarded browser header %s", key)
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"http://other-host/api/sessions/other/input"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: r}, nil
			})}
			if _, err := runtimeJSON(context.Background(), client, "POST", "/api/sessions/test/input", map[string]string{"data": "literal prompt"}); err == nil || calls != 1 {
				t.Fatal("redirected or replayed mutation", calls, err)
			}
		})
	}
}

func TestMaintenanceInputRetriesOnlyDefinitivePTYStartupRejection(t *testing.T) {
	for _, scenario := range []string{"not-ready", "lease", "uncertain", "write-error", "exited", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			posts, reads := 0, 0
			client := &http.Client{Transport: maintenanceRoundTrip(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusConflict, `{"error":"session is not accepting input"}`
				if r.Method == "GET" {
					reads++
					status, body = 200, `[{"id":"job","status":"running"}]`
					if scenario == "exited" {
						body = `[{"id":"job","status":"exited"}]`
					}
				} else {
					posts++
					switch scenario {
					case "lease":
						body = `{"error":"another viewer has terminal control"}`
					case "write-error":
						body = `{"error":"write /dev/ptmx: input/output error"}`
					case "uncertain":
						return nil, io.ErrUnexpectedEOF
					case "not-ready":
						if posts == 2 {
							status, body = 204, ""
						}
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			if scenario != "cancelled" {
				ctx = context.Background()
			}
			err := startMaintenanceInput(ctx, client, "job", "exec trusted-command\r")
			if scenario == "not-ready" {
				if err != nil || posts != 2 || reads != 1 {
					t.Fatal("startup rejection was not handled", posts, reads, err)
				}
			} else if err == nil || posts != 1 {
				t.Fatal("replayed uncertain, leased, exited or cancelled input", posts, reads, err)
			}
			if scenario == "cancelled" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("startup wait ignored cancellation", err)
			}
		})
	}
}

func TestHarnessMaintenanceEndpointRejectsUnauthenticatedWrongHostAndUnclosedActions(t *testing.T) {
	s, ts, client := testController(t, "")
	endpoint := ts.URL + "/api/hosts/missing/harnesses/codex/update"
	if r := request(t, client, "POST", endpoint, "", "", ts.URL); r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	login(t, s, client)
	if r := request(t, client, "POST", endpoint, "", "", ts.URL); r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if r := request(t, client, "POST", endpoint, "", s.csrf, "https://other.example"); r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if r := request(t, client, "POST", endpoint, "", s.csrf, ts.URL); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
	if err := s.store.add(Host{ID: "host", Target: "unused", Status: "disconnected"}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"codex/update;false", "unknown/update", "codex/install"} {
		if r := request(t, client, "POST", ts.URL+"/api/hosts/host/harnesses/"+suffix, "", s.csrf, ts.URL); r.StatusCode != 404 {
			t.Fatal(suffix, r.StatusCode)
		}
	}
	if r := request(t, client, "POST", ts.URL+"/api/hosts/host/harnesses/codex/update", "", s.csrf, ts.URL); r.StatusCode != 503 {
		t.Fatal(r.StatusCode)
	}
}
