package controller

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNativeLoginEstablishesDocument(t *testing.T) {
	for _, site := range []string{"", "none"} {
		t.Run("fetch-site="+site, func(t *testing.T) {
			s, ts, client := testController(t, "")
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			link := s.LoginURL()
			req, err := http.NewRequest(http.MethodGet, link, nil)
			if err != nil {
				t.Fatal(err)
			}
			if site != "" {
				req.Header.Set("Sec-Fetch-Site", site)
				req.Header.Set("Sec-Fetch-Mode", "navigate")
				req.Header.Set("Sec-Fetch-Dest", "document")
			}
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Location") != "" || !strings.Contains(string(body), `http-equiv="refresh" content="0;url=/"`) {
				t.Fatalf("native navigation must establish a document before following its Strict cookie: %d", response.StatusCode)
			}
			if strings.Contains(string(body), s.launchToken) {
				t.Fatal("handoff leaked its launch token")
			}
			cookies := response.Cookies()
			if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
				t.Fatal("native handoff weakened its session cookie")
			}
			if r := request(t, client, http.MethodGet, ts.URL+"/api/bootstrap", "", "", ts.URL); r.StatusCode != http.StatusOK {
				t.Fatal("native cookie did not authenticate", r.StatusCode)
			}
			if r := request(t, client, http.MethodGet, link, "", "", ""); r.StatusCode != http.StatusUnauthorized {
				t.Fatal("native handoff replayed launch token", r.StatusCode)
			}
		})
	}
}

func TestMissingFetchMetadataDoesNotBypassExplicitForeignOrigin(t *testing.T) {
	s, _, client := testController(t, "")
	response := request(t, client, http.MethodGet, s.LoginURL(), "", "", "https://foreign.example")
	if response.StatusCode != http.StatusForbidden || len(response.Cookies()) != 0 {
		t.Fatal("foreign fetch without navigation evidence redeemed token", response.StatusCode)
	}
	// A rejected attempt must not consume the private link.
	response = request(t, client, http.MethodGet, s.LoginURL(), "", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatal("rejected foreign request consumed token", response.StatusCode)
	}
}

func TestKnownSameOriginLoginCanRedirect(t *testing.T) {
	s, _, client := testController(t, "")
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest(http.MethodGet, s.LoginURL(), nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		t.Fatal("known same-origin login changed", response.StatusCode)
	}
}
