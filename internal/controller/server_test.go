package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
)

func testController(t *testing.T, socket string) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{StateDir: t.TempDir(), Version: "test", Address: ln.Addr().String(), LocalSocket: socket, Assets: fstest.MapFS{"index.html": {Data: []byte("workspace")}}})
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(func() { s.Close(); ts.Close() })
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	return s, ts, client
}

func login(t *testing.T, s *Server, c *http.Client) {
	t.Helper()
	res, err := c.Get(s.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("login status %d", res.StatusCode)
	}
}
func request(t *testing.T, c *http.Client, method, uri, body, csrf, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, uri, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-Relay-CSRF", csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestBrowserSecurityBoundaries(t *testing.T) {
	s, ts, c := testController(t, "")
	if r := request(t, c, "GET", ts.URL+"/api/state", "", "", ""); r.StatusCode != 401 {
		t.Fatalf("unauthenticated %d", r.StatusCode)
	}
	login(t, s, c)
	if r := request(t, c, "GET", s.LoginURL(), "", "", ""); r.StatusCode != 401 {
		t.Fatalf("reused login token %d", r.StatusCode)
	}
	if r := request(t, c, "GET", ts.URL+"/api/state", "", "", "https://evil.example"); r.StatusCode != 403 {
		t.Fatalf("cross origin %d", r.StatusCode)
	}
	if r := request(t, c, "POST", ts.URL+"/api/hosts", `{"target":"-oProxyCommand=evil"}`, "", ts.URL); r.StatusCode != 403 {
		t.Fatalf("no CSRF %d", r.StatusCode)
	}
	if r := request(t, c, "POST", ts.URL+"/api/hosts", `{"target":"-oProxyCommand=evil"}`, s.csrf, ts.URL); r.StatusCode != 400 {
		t.Fatalf("SSH injection %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/state", nil)
	req.Host = "attacker.example:" + s.port
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatalf("DNS rebinding %d", r.StatusCode)
	}
	r = request(t, c, "GET", ts.URL+"/api/bootstrap", "", "", ts.URL)
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	var bootstrap struct {
		CSRF string `json:"csrf"`
	}
	if err = json.NewDecoder(r.Body).Decode(&bootstrap); err != nil || bootstrap.CSRF != s.csrf {
		t.Fatalf("bootstrap %v", err)
	}
	if r.Header.Get("Referrer-Policy") != "no-referrer" || r.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("security headers missing")
	}
}

func TestLaunchLinkTopLevelNavigation(t *testing.T) {
	for _, browserOrigin := range []string{"", "https://chat.example"} {
		t.Run("origin="+browserOrigin, func(t *testing.T) {
			s, ts, c := testController(t, "")
			c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			req, _ := http.NewRequest(http.MethodGet, s.LoginURL(), nil)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			req.Header.Set("Sec-Fetch-Mode", "navigate")
			req.Header.Set("Sec-Fetch-Dest", "document")
			if browserOrigin != "" {
				req.Header.Set("Origin", browserOrigin)
			}
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("launch navigation did not establish a same-origin document: status=%d", res.StatusCode)
			}
			if !strings.Contains(string(body), `http-equiv="refresh" content="0;url=/"`) || strings.Contains(string(body), s.launchToken) || strings.Contains(string(body), "chat.example") {
				t.Fatal("login handoff did not use a fixed local destination without secrets")
			}
			cookies := res.Cookies()
			if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
				t.Fatal("login did not issue the private same-site session cookie")
			}
			if res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("launch navigation lost browser security headers")
			}
			if got := request(t, c, http.MethodGet, ts.URL+"/api/bootstrap", "", "", ts.URL); got.StatusCode != http.StatusOK {
				t.Fatalf("issued session could not authenticate: %d", got.StatusCode)
			}
			res, err = c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized || len(res.Cookies()) != 0 {
				t.Fatalf("external navigation reused launch token: %d", res.StatusCode)
			}
		})
	}
}

func TestLaunchLinkExceptionDoesNotWeakenBrowserBoundaries(t *testing.T) {
	cases := []struct {
		name, method, path, mode, destination, origin string
		wrongHost                                     bool
	}{
		{name: "cross-origin fetch", method: "GET", mode: "cors", destination: "empty", origin: "https://chat.example"},
		{name: "opaque fetch", method: "GET", mode: "no-cors", destination: "empty"},
		{name: "image", method: "GET", mode: "no-cors", destination: "image"},
		{name: "iframe", method: "GET", mode: "navigate", destination: "iframe"},
		{name: "frame", method: "GET", mode: "navigate", destination: "frame"},
		{name: "missing mode", method: "GET", destination: "document"},
		{name: "missing destination", method: "GET", mode: "navigate"},
		{name: "post launch", method: "POST", mode: "navigate", destination: "document"},
		{name: "head launch", method: "HEAD", mode: "navigate", destination: "document"},
		{name: "API document navigation", method: "GET", path: "/api/bootstrap", mode: "navigate", destination: "document"},
		{name: "API mutation", method: "POST", path: "/api/hosts", mode: "navigate", destination: "document", origin: "https://chat.example"},
		{name: "root document navigation", method: "GET", path: "/", mode: "navigate", destination: "document"},
		{name: "wrong host", method: "GET", mode: "navigate", destination: "document", wrongHost: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ts, c := testController(t, "")
			uri := s.LoginURL()
			if tc.path != "" {
				uri = ts.URL + tc.path
			}
			req, _ := http.NewRequest(tc.method, uri, strings.NewReader(`{"target":"example"}`))
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			req.Header.Set("Sec-Fetch-Mode", tc.mode)
			req.Header.Set("Sec-Fetch-Dest", tc.destination)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", "application/json")
			// Supply otherwise valid credentials so rejection exercises the origin
			// boundary independently of SameSite cookie or CSRF enforcement.
			req.AddCookie(&http.Cookie{Name: cookieName, Value: s.sessionToken})
			req.Header.Set("X-Relay-CSRF", s.csrf)
			if tc.wrongHost {
				req.Host = "attacker.example:" + s.port
			}
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden || len(res.Cookies()) != 0 {
				t.Fatalf("disallowed launch context: status=%d", res.StatusCode)
			}
			// A blocked request must not consume the secret. Header-free native
			// clients retain the existing one-time login behavior.
			login(t, s, c)
		})
	}
}

func TestExternalLaunchNavigationStillRequiresSecret(t *testing.T) {
	s, ts, c := testController(t, "")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/auth?token=incorrect", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || len(res.Cookies()) != 0 {
		t.Fatalf("external navigation bypassed the login secret: %d", res.StatusCode)
	}
	login(t, s, c)
}

func TestHostInputValidation(t *testing.T) {
	s, ts, c := testController(t, "")
	login(t, s, c)
	for _, body := range []string{`{"target":"a; touch /tmp/bad"}`, `{"target":"host","port":65536}`, `{"target":"host","name":"bad\nname"}`, `{"target":"host","password":"must not be accepted"}`, `{"target":"host"} {"target":"second"}`, `{"target":"` + strings.Repeat("a", 400) + `"}`} {
		if r := request(t, c, "POST", ts.URL+"/api/hosts", body, s.csrf, ts.URL); r.StatusCode != 400 {
			t.Fatalf("body %q accepted: %d", body, r.StatusCode)
		}
	}
	if len(s.store.list()) != 0 {
		t.Fatal("invalid request changed state")
	}
}

func TestUnixRuntimeProxyAndWebsocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local Unix runtime is available on Linux; Windows uses the SSH transport")
	}
	dir, err := os.MkdirTemp("", "relay-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seenPath, seenCookie, seenCSRF string
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions/test/terminal" {
			up := websocket.Upgrader{CheckOrigin: sameOrigin}
			ws, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			kind, b, err := ws.ReadMessage()
			if err == nil {
				_ = ws.WriteMessage(kind, b)
			}
			return
		}
		mu.Lock()
		seenPath = r.URL.Path
		seenCookie = r.Header.Get("Cookie")
		seenCSRF = r.Header.Get("X-Relay-CSRF")
		mu.Unlock()
		writeJSON(w, 200, []string{"session"})
	})}
	go backend.Serve(ln)
	defer backend.Close()
	s, ts, c := testController(t, sock)
	login(t, s, c)
	r := request(t, c, "POST", ts.URL+"/api/hosts/local/runtime/sessions", `{}`, s.csrf, ts.URL)
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	mu.Lock()
	if seenPath != "/api/sessions" || seenCookie != "" || seenCSRF != "" {
		t.Errorf("proxy path/secrets: %q %q %q", seenPath, seenCookie, seenCSRF)
	}
	mu.Unlock()
	u, _ := url.Parse(ts.URL)
	headers := http.Header{"Origin": {ts.URL}}
	for _, cookie := range c.Jar.Cookies(u) {
		headers.Add("Cookie", cookie.String())
	}
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/hosts/local/runtime/sessions/test/terminal"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Fatal(resp.Status, err)
		}
		t.Fatal(err)
	}
	defer ws.Close()
	payload := []byte("terminal output\x1b[32m")
	if err = ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	_, got, err := ws.ReadMessage()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("WS echo %q %v", got, err)
	}
	headers.Set("Origin", "https://evil.example")
	_, resp, err = websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatal("cross-origin WS accepted")
	}
	if resp != nil {
		resp.Body.Close()
	}
}

func TestSavedHostsArePrivateAndRestartDisconnected(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := Host{ID: "host1", Name: "Development", Target: "dev", Status: "online", CreatedAt: time.Now()}
	if err = s.add(h); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "hosts.json"))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("state permissions %v %v", info, err)
	}
	if err = s.add(Host{ID: "host2", Target: "dev"}); err == nil {
		t.Fatal("duplicate target accepted")
	}
	reloaded, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.get("host1")
	if !ok || got.Status != "disconnected" {
		t.Fatalf("restart state %+v", got)
	}
	if err = reloaded.remove("host1"); err != nil {
		t.Fatal(err)
	}
	reloaded, err = openStore(dir)
	if err != nil || len(reloaded.list()) != 0 {
		t.Fatal("deletion not persisted", err)
	}
}

func TestFailedStoreWriteRollsBackInMemory(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(t.TempDir(), "missing", "hosts.json")
	if err = s.add(Host{ID: "host1", Target: "dev"}); err == nil {
		t.Fatal("expected disk error")
	}
	if len(s.list()) != 0 {
		t.Fatal("failed write changed host list")
	}
}

func TestProxyRejectsDisconnectedHostWithoutRetry(t *testing.T) {
	s, ts, c := testController(t, "")
	login(t, s, c)
	r := request(t, c, "POST", ts.URL+"/api/hosts/missing/runtime/sessions", `{}`, s.csrf, ts.URL)
	if r.StatusCode != 503 {
		t.Fatal(r.StatusCode)
	}
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "disconnected") {
		t.Fatal(string(b))
	}
}

func TestMutationAuditDoesNotRecordBodySecrets(t *testing.T) {
	s, ts, c := testController(t, "")
	login(t, s, c)
	r := request(t, c, "POST", ts.URL+"/api/hosts", `{"target":"host","password":"secret-never-in-audit"}`, s.csrf, ts.URL)
	if r.StatusCode != 400 || r.Header.Get("X-Request-ID") == "" {
		t.Fatal("missing rejection/request trace")
	}
	// The request completes before audit append returns through the HTTP handler.
	var data []byte
	for i := 0; i < 30; i++ {
		data, _ = os.ReadFile(filepath.Join(s.config.StateDir, "audit.jsonl"))
		if len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(string(data), "secret-never-in-audit") || strings.Contains(string(data), "password") || !strings.Contains(string(data), `"status":400`) {
		t.Fatalf("invalid audit: %s", data)
	}
	info, err := os.Stat(filepath.Join(s.config.StateDir, "audit.jsonl"))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("audit must be private", err)
	}
}

func TestStaleConnectionAttemptCannotOverwriteNewState(t *testing.T) {
	s, _, _ := testController(t, "")
	if err := s.store.add(Host{ID: "h1", Target: "dev", Status: "online", Stage: "Connected"}); err != nil {
		t.Fatal(err)
	}
	old := &link{}
	current := &link{}
	s.mu.Lock()
	s.links["h1"] = current
	s.mu.Unlock()
	s.connectionStage("h1", old, "installing", "Old attempt")
	h, _ := s.store.get("h1")
	if h.Status != "online" {
		t.Fatal("stale attempt overwrote current connection")
	}
	s.connectionStage("h1", current, "installing", "Current attempt")
	h, _ = s.store.get("h1")
	if h.Stage != "Current attempt" {
		t.Fatal("current attempt lost stage update")
	}
}

func TestForgottenHostCannotStartFromStaleRequest(t *testing.T) {
	s, _, _ := testController(t, "")
	h := Host{ID: "forgotten", Target: "dev"}
	if err := s.store.add(h); err != nil {
		t.Fatal(err)
	}
	if err := s.store.remove(h.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.beginConnect(h); err == nil {
		t.Fatal("stale request started a connection to a forgotten host")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.links) != 0 {
		t.Fatal("forgotten host has an invisible connection")
	}
}
