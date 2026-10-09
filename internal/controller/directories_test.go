package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TwoD97/relay/internal/transport"
)

func TestDirectoryEndpointRequiresAuthenticatedSameOriginReadyHost(t *testing.T) {
	s, ts, client := testController(t, "")
	endpoint := ts.URL + "/api/hosts/missing/directories"
	if response := request(t, client, "GET", endpoint, "", "", ""); response.StatusCode != 401 {
		t.Fatal("unauthenticated folder disclosure", response.StatusCode)
	}
	login(t, s, client)
	if response := request(t, client, "GET", endpoint, "", "", "https://other.example"); response.StatusCode != 403 {
		t.Fatal("cross-origin folder disclosure", response.StatusCode)
	}
	if response := request(t, client, "GET", endpoint, "", "", ts.URL); response.StatusCode != 503 {
		t.Fatal("missing host did not fail closed", response.StatusCode)
	}
	if response := request(t, client, "GET", ts.URL+"/api/hosts/local/directories", "", "", ts.URL); response.StatusCode != 503 {
		t.Fatal("disabled local host browsed filesystem", response.StatusCode)
	}
	for _, query := range []string{"path=relative", "path=%2Fbad%0Aname", "prefix=..%2F", "hidden=1", "path=%2F&path=%2Ftmp", "unknown=value"} {
		if response := request(t, client, "GET", endpoint+"?"+query, "", "", ts.URL); response.StatusCode != 400 {
			t.Fatalf("invalid query %s: %d", query, response.StatusCode)
		}
	}
}

func TestLocalDirectoryBrowsingDoesNotContactOrRestartRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local filesystem sessions are Linux only")
	}
	// Deliberately absent runtime socket: browsing must be independent from the
	// daemon's API/version and must not need to start or replace it.
	s, ts, client := testController(t, filepath.Join(t.TempDir(), "missing.sock"))
	login(t, s, client)
	directory := t.TempDir()
	for _, name := range []string{"project ' $[]", "other", ".hidden", "line\nbreak"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(directory, "other"), filepath.Join(directory, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := ts.URL + "/api/hosts/local/directories?path="
	get := func(path, extra string, expected int) transport.DirectoryListing {
		t.Helper()
		response := request(t, client, "GET", endpoint+url.QueryEscape(path)+extra, "", "", ts.URL)
		if response.StatusCode != expected {
			t.Fatalf("folder response: %d != %d", response.StatusCode, expected)
		}
		var result transport.DirectoryListing
		if expected == 200 {
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	listing := get(directory, "", 200)
	if listing.Path != directory || len(listing.Directories) != 3 || listing.Truncated || listing.Parent == nil {
		t.Fatal("unexpected local listing", listing)
	}
	if got := get(directory, "&prefix="+url.QueryEscape("project ' $["), 200); len(got.Directories) != 1 {
		t.Fatal("literal prefix mismatch", got)
	}
	if got := get(directory, "&hidden=true", 200); len(got.Directories) != 4 {
		t.Fatal("hidden policy", got)
	}
	if got := get(filepath.Join(directory, "linked"), "", 200); got.Path != filepath.Join(directory, "other") {
		t.Fatal("link was not canonicalized", got)
	}
	get(filepath.Join(directory, "missing"), "", 404)
	get(filepath.Join(directory, "file"), "", 400)
	for i := 0; i < cap(s.directorySlots); i++ {
		s.directorySlots <- struct{}{}
	}
	get(directory, "", http.StatusTooManyRequests)
	for i := 0; i < cap(s.directorySlots); i++ {
		<-s.directorySlots
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := localDirectories(ctx, transport.DirectoryQuery{Path: directory}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled lookup ignored cancellation", err)
	}
}
