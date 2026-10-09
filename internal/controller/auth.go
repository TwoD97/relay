package controller

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const cookieName = "relay_session"
const launchTokenTTL = 5 * time.Minute
const maxLaunchTokens = 64

func equalSecret(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *Server) addLoginTokenLocked(token string, now time.Time) {
	s.launchTokens[sha256.Sum256([]byte(token))] = now.Add(launchTokenTTL)
}

// IssueLoginURL grants one additional browser its own short-lived launch link.
// Session cookies and CSRF stay stable, so existing clients remain connected.
// This is exposed only by the private local control socket, never the web API.
func (s *Server) IssueLoginURL() (string, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := time.Now()
	for token, expiry := range s.launchTokens {
		if !expiry.After(now) {
			delete(s.launchTokens, token)
		}
	}
	if len(s.launchTokens) >= maxLaunchTokens {
		return "", errors.New("too many unused login links; use an existing link or retry in five minutes")
	}
	token := randomID()
	s.addLoginTokenLocked(token, now)
	return "http://" + s.config.Address + "/auth?token=" + token, nil
}

func (s *Server) consumeLoginToken(token string) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	key := sha256.Sum256([]byte(token))
	expiry, found := s.launchTokens[key]
	delete(s.launchTokens, key)
	return found && expiry.After(time.Now())
}

func (s *Server) allowedHost(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if p != s.port {
		return false
	}
	configured, _, _ := net.SplitHostPort(s.config.Address)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == configured
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func loginNavigation(r *http.Request) bool {
	// Fetch Metadata is browser-controlled. A launch link may originate in a
	// different application, but only a top-level document navigation may redeem
	// it across origins; fetches and embedded documents keep the normal boundary.
	return r.URL.Path == "/auth" && r.Method == http.MethodGet &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-Dest") == "document"
}

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if !s.allowedHost(r.Host) {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		crossOrigin := !sameOrigin(r)
		if crossOrigin && !loginNavigation(r) {
			http.Error(w, "Cross-origin request denied", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/auth" {
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", 405)
				return
			}
			valid := s.consumeLoginToken(r.URL.Query().Get("token"))
			if !valid {
				http.Error(w, "This login link is invalid, expired, or has already been used. Run relay ui or reconnect the desktop window to get a fresh link.", 401)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.sessionToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			if crossOrigin {
				// A redirect keeps the external navigation's cross-site context:
				// its Strict cookie would be omitted and the root request denied.
				// Loading this document starts a fresh same-origin navigation while
				// keeping the exception confined to this one-time login endpoint.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/"><title>Opening Relay</title></head><body><p><a href="/">Open Relay</a></p></body></html>`))
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || !equalSecret(cookie.Value, s.sessionToken) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, 401, "Open the login link printed by relay ui")
			} else {
				http.Error(w, "Open the login link printed by relay ui in your terminal.", 401)
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !equalSecret(r.Header.Get("X-Relay-CSRF"), s.csrf) {
				writeError(w, 403, "Invalid CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
