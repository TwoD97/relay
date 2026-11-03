package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageUploadAuthenticatesExactSessionAndNeverSendsInput(t *testing.T) {
	var mu sync.Mutex
	session := imageSession{ID: strings.Repeat("a", 32), CreatedAt: time.Now().UTC(), Status: "running"}
	var mutations atomic.Int32
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/sessions" {
			mutations.Add(1)
			http.Error(w, "unexpected", 500)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, 200, []imageSession{session})
	}))
	defer runtime.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "private-state")
	if err = makeWorkspaceCapabilityDirectory(root); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{StateDir: root, Address: listener.Addr().String(), LocalSocket: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	s.links["local"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", runtime.Listener.Addr().String())
	})
	s.mu.Unlock()
	server := httptest.NewUnstartedServer(s.Handler())
	server.Listener = listener
	server.Start()
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	var content bytes.Buffer
	picture := image.NewNRGBA(image.Rect(0, 0, 7, 5))
	picture.Set(2, 2, color.NRGBA{R: 255, A: 255})
	if err = png.Encode(&content, picture); err != nil {
		t.Fatal(err)
	}
	base := server.URL + "/api/hosts/local/sessions/" + session.ID + "/images?createdAt="
	endpoint := base + url.QueryEscape(session.CreatedAt.Format(time.RFC3339Nano))
	post := func(address, csrf, origin, mime string, data []byte) (int, []byte) {
		t.Helper()
		request, _ := http.NewRequest("POST", address, bytes.NewReader(data))
		request.Header.Set("Content-Type", mime)
		request.Header.Set("X-Relay-CSRF", csrf)
		request.Header.Set("Origin", origin)
		response, e := client.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		out, e := io.ReadAll(response.Body)
		if e != nil {
			t.Fatal(e)
		}
		return response.StatusCode, out
	}
	if status, _ := post(endpoint, "", server.URL, "image/png", content.Bytes()); status != 401 {
		t.Fatalf("anonymous %d", status)
	}
	login(t, s, client)
	for _, bad := range []struct{ csrf, origin string }{{"", server.URL}, {s.csrf, "https://other.example"}} {
		if status, _ := post(endpoint, bad.csrf, bad.origin, "image/png", content.Bytes()); status != 403 {
			t.Fatalf("authorization %d", status)
		}
	}
	if status, _ := post(base+url.QueryEscape(session.CreatedAt.Add(time.Nanosecond).Format(time.RFC3339Nano)), s.csrf, server.URL, "image/png", content.Bytes()); status != 409 {
		t.Fatalf("stale identity %d", status)
	}
	if status, _ := post(endpoint, s.csrf, server.URL, "image/png", []byte("not an image")); status != 400 {
		t.Fatalf("disguised bytes %d", status)
	}
	status, data := post(endpoint, s.csrf, server.URL, "image/png", content.Bytes())
	if status != 201 {
		t.Fatalf("valid image %d: %s", status, data)
	}
	var result struct {
		Path, Mime, Sha256 string
		Bytes              int
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(result.Path)
	if err != nil || !bytes.Equal(actual, content.Bytes()) || result.Mime != "image/png" || result.Bytes != content.Len() || result.Sha256 != fmt.Sprintf("%x", sha256.Sum256(content.Bytes())) {
		t.Fatalf("image result %+v: %v", result, err)
	}
	status, data = post(endpoint, s.csrf, server.URL, "image/png", content.Bytes())
	if status != 201 {
		t.Fatalf("repeat image %d: %s", status, data)
	}
	var repeated map[string]any
	json.Unmarshal(data, &repeated)
	if repeated["path"] != result.Path {
		t.Fatal("duplicate file on same image")
	}
	mu.Lock()
	session.Purpose = "login"
	mu.Unlock()
	if status, _ = post(endpoint, s.csrf, server.URL, "image/png", content.Bytes()); status != 409 {
		t.Fatalf("login accepted %d", status)
	}
	mu.Lock()
	session.Purpose = ""
	session.Status = "exited"
	mu.Unlock()
	if status, _ = post(endpoint, s.csrf, server.URL, "image/png", content.Bytes()); status != 409 {
		t.Fatalf("finished accepted %d", status)
	}
	if mutations.Load() != 0 {
		t.Fatalf("image upload sent %d runtime mutations", mutations.Load())
	}
}
