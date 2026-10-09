package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type handoverBrowser struct {
	client       *http.Client
	origin, csrf string
}

func handoverLogin(t *testing.T, launch desktopLaunch) *handoverBrowser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	b := &handoverBrowser{client: &http.Client{Jar: jar, Timeout: 10 * time.Second}, origin: launch.Address}
	response, err := b.client.Get(launch.URL)
	if err != nil {
		t.Fatal("authenticate upgrade client", err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("upgrade login rejected", response.StatusCode)
	}
	data := b.request(t, http.MethodGet, "/api/bootstrap", nil, 200)
	var bootstrap struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(data, &bootstrap); err != nil || bootstrap.CSRF == "" {
		t.Fatal("missing browser CSRF")
	}
	b.csrf = bootstrap.CSRF
	return b
}

func (b *handoverBrowser) request(t *testing.T, method, path string, body any, status int) []byte {
	t.Helper()
	var input io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.origin+path, input)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", b.origin)
	req.Header.Set("X-Relay-CSRF", b.csrf)
	req.Header.Set("Content-Type", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal("upgrade API request", path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != status {
		t.Fatalf("upgrade API %s returned %d, want %d", path, res.StatusCode, status)
	}
	return data
}

func (b *handoverBrowser) authenticateFixture(t *testing.T, host, target, password, fingerprint string, trust bool) {
	t.Helper()
	address, _ := url.Parse(b.origin)
	headers := http.Header{"Origin": []string{b.origin}}
	var cookies []string
	for _, c := range b.client.Jar.Cookies(address) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	headers.Set("Cookie", strings.Join(cookies, "; "))
	deadline := time.Now().Add(20 * time.Second)
	var ws *websocket.Conn
	for {
		candidate, response, err := websocket.DefaultDialer.Dial("ws://"+address.Host+"/api/hosts/"+host+"/setup-terminal", headers)
		if err == nil {
			ws = candidate
			break
		}
		if response != nil {
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture setup did not become available")
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer ws.Close()
	ws.SetReadLimit(64 << 10)
	output := make(chan []byte, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			select {
			case output <- data:
			case <-time.After(time.Second):
				return
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	transcript := ""
	trusted, sent := false, false
	for time.Now().Before(deadline) {
		select {
		case data := <-output:
			transcript += string(data)
			if len(transcript) > 8192 {
				transcript = transcript[len(transcript)-8192:]
			}
			if strings.Contains(transcript, "(yes/no)?") && !trusted {
				if !trust || !strings.Contains(transcript, fingerprint) {
					t.Fatal("unexpected fixture host-key trust prompt")
				}
				if err := ws.WriteJSON(map[string]string{"type": "input", "data": "yes\r"}); err != nil {
					t.Fatal(err)
				}
				trusted = true
				transcript = ""
			}
			if strings.Contains(transcript, target+"'s password:") && !sent {
				if err := ws.WriteJSON(map[string]string{"type": "input", "data": password + "\r"}); err != nil {
					t.Fatal(err)
				}
				sent = true
				transcript = ""
			}
		case <-ticker.C:
			var state struct{ Hosts []struct{ ID, Status string } }
			if err := json.Unmarshal(b.request(t, "GET", "/api/state", nil, 200), &state); err != nil {
				t.Fatal(err)
			}
			for _, h := range state.Hosts {
				if h.ID == host && h.Status == "online" {
					return
				}
				if h.ID == host && h.Status == "error" {
					t.Fatal("fixture SSH connection failed")
				}
			}
		}
	}
	t.Fatal("fixture SSH authentication timed out")
}

func prepareRemoteUpgradeSentinel(t *testing.T, old desktopLaunch) func(desktopLaunch) {
	t.Helper()
	target := os.Getenv("RELAY_HANDOVER_FIXTURE_TARGET")
	if target == "" {
		return func(desktopLaunch) {}
	}
	passwordData, err := os.ReadFile(os.Getenv("RELAY_HANDOVER_FIXTURE_PASSWORD_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(passwordData))
	fingerprint := os.Getenv("RELAY_HANDOVER_FIXTURE_FINGERPRINT")
	port, err := strconv.Atoi(os.Getenv("RELAY_HANDOVER_FIXTURE_PORT"))
	if err != nil || port < 1 || port > 65535 || password == "" || fingerprint == "" {
		t.Fatal("invalid disposable SSH fixture configuration")
	}
	browser := handoverLogin(t, old)
	var host struct{ ID string }
	if err := json.Unmarshal(browser.request(t, "POST", "/api/hosts", map[string]any{"name": "Disposable upgrade fixture", "target": target, "port": port}, 201), &host); err != nil {
		t.Fatal(err)
	}
	browser.authenticateFixture(t, host.ID, target, password, fingerprint, true)
	prefix := "/api/hosts/" + host.ID + "/runtime"
	before := browser.request(t, "GET", prefix+"/health", nil, 200)
	var session struct{ ID string }
	if err := json.Unmarshal(browser.request(t, "POST", prefix+"/sessions", map[string]any{"harness": "shell", "cwd": "/home/relay", "title": "Controller upgrade sentinel", "workspace": "Disposable upgrade"}, 201), &session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.request(t, "DELETE", prefix+"/sessions/"+session.ID, nil, 204) })
	browser.request(t, "POST", prefix+"/sessions/"+session.ID+"/input", map[string]string{"data": "export RELAY_UPGRADE_SENTINEL=" + session.ID + "\r"}, 204)
	return func(launch desktopLaunch) {
		browser = handoverLogin(t, launch)
		browser.authenticateFixture(t, host.ID, target, password, fingerprint, false)
		if after := browser.request(t, "GET", prefix+"/health", nil, 200); !bytes.Equal(before, after) {
			t.Fatal("controller upgrade changed remote runtime health")
		}
		if !bytes.Contains(browser.request(t, "GET", prefix+"/sessions", nil, 200), []byte(session.ID)) {
			t.Fatal("controller upgrade lost original remote session")
		}
		browser.request(t, "POST", prefix+"/sessions/"+session.ID+"/input", map[string]string{"data": "printf 'UPGRADE_%s\\n' \"$RELAY_UPGRADE_SENTINEL\"\r"}, 204)
		deadline := time.Now().Add(5 * time.Second)
		for {
			data := browser.request(t, "GET", prefix+"/sessions/"+session.ID+"/history", nil, 200)
			if bytes.Contains(data, []byte("UPGRADE_"+session.ID)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("original remote shell state did not survive handover")
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Log(fmt.Sprintf("original remote session %s survived legacy Windows controller migration", session.ID))
	}
}
