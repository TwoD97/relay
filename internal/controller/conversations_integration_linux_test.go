package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/TwoD97/relay/internal/conversations"
)

func TestConversationFacadeAuthenticationCapabilityAndNoProviderLaunch(t *testing.T) {
	const id = "1a2b3c4d-1234-4567-89ab-0123456789ab"
	cwd := t.TempDir()
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	var encoded strings.Builder
	for _, c := range cwd {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			encoded.WriteRune(c)
		} else {
			encoded.WriteByte('-')
		}
	}
	dir := filepath.Join(config, "projects", encoded.String())
	os.MkdirAll(dir, 0700)
	header, _ := json.Marshal(map[string]string{"type": "user", "sessionId": id, "cwd": cwd, "message": "PRIVATE TRANSCRIPT"})
	os.WriteFile(filepath.Join(dir, id+".jsonl"), append(header, '\n'), 0600)
	var supported atomic.Bool
	var mutations atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations.Add(1)
		}
		if r.URL.Path == "/api/health" {
			writeJSON(w, 200, map[string]any{"capabilities": map[string]bool{"conversationResumeV1": supported.Load()}})
			return
		}
		writeJSON(w, 200, []any{})
	}))
	defer backend.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	s, err := New(Config{StateDir: state, Address: listener.Addr().String(), LocalSocket: filepath.Join(state, "runtime", "run", "daemon.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	s.links["local"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Listener.Addr().String())
	})
	s.mu.Unlock()
	server := httptest.NewUnstartedServer(s.Handler())
	server.Listener = listener
	server.Start()
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	endpoint := server.URL + "/api/hosts/local/conversations?harness=claude&cwd=" + url.QueryEscape(cwd)
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("anonymous history access allowed")
	}
	login(t, s, client)
	read := func() conversations.Listing {
		t.Helper()
		r, e := client.Get(endpoint)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		var list conversations.Listing
		if r.StatusCode != 200 || json.Unmarshal(data, &list) != nil {
			t.Fatalf("lookup %d: %s", r.StatusCode, data)
		}
		if bytes.Contains(data, []byte("PRIVATE")) {
			t.Fatal("transcript exposed")
		}
		return list
	}
	list := read()
	if list.ResumeSupported || list.Detail == "" || len(list.Conversations) != 1 {
		t.Fatalf("old capability falsely available: %+v", list)
	}
	supported.Store(true)
	if !read().ResumeSupported {
		t.Fatal("new runtime capability ignored")
	}
	payload, _ := json.Marshal(map[string]string{"harness": "claude", "cwd": cwd, "title": "Saved name"})
	patch := func(csrf string) int {
		req, _ := http.NewRequest("PATCH", server.URL+"/api/hosts/local/conversations/"+id, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", server.URL)
		req.Header.Set("X-Relay-CSRF", csrf)
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if patch("") != 403 || patch(s.csrf) != 200 {
		t.Fatal("rename CSRF or valid mutation failed")
	}
	if got := read().Conversations[0]; got.Title != "Saved name" || got.TitleSource != "relay" {
		t.Fatalf("alias not surfaced: %+v", got)
	}
	if mutations.Load() != 0 {
		t.Fatal("history operation mutated runtime sessions")
	}
	s.mu.Lock()
	delete(s.links, "local")
	s.mu.Unlock()
	response, _ = client.Get(endpoint)
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("offline lookup did not fail closed")
	}
}
