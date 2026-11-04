//go:build linux

package runtime

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/peers"
)

func peerCall(s *Server, method, path string, value any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(value)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(data)))
	return w
}
func TestPeerHTTPPreservesInertSnapshotsAndEnforcesCAS(t *testing.T) {
	s := testServer(t)
	id := strings.Repeat("a", 48)
	snapshot := peers.Snapshot{SourceID: id, Name: "Fixture desktop", Revision: 1, UpdatedAt: time.Now().UTC(), Data: json.RawMessage(`{"pages":[{"html":"<script>doNotExecute()</script>"}],"groups":[],"lessons":[]}`)}
	put := func(expected string, value peers.Snapshot) *httptest.ResponseRecorder {
		return peerCall(s, "PUT", "/api/peers/"+id, map[string]any{"expectedDigest": expected, "snapshot": value})
	}
	w := put("", snapshot)
	var saved peers.Record
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved.Digest == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(s.sessions) != 0 {
		t.Fatal("snapshot started a session")
	}
	w = peerCall(s, "GET", "/api/peers/"+id, nil)
	var returned peers.Record
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &returned) != nil || !bytes.Contains(returned.Data, []byte("doNotExecute")) {
		t.Fatal(w.Code, w.Body.String())
	}
	wrong := snapshot
	wrong.SourceID = strings.Repeat("b", 48)
	if w = put("", wrong); w.Code != 400 {
		t.Fatal("path/source mismatch accepted", w.Code)
	}
	snapshot.Revision++
	if w = put("", snapshot); w.Code != 409 {
		t.Fatal("stale write accepted", w.Code)
	}
	if w = put(saved.Digest, snapshot); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var next peers.Record
	json.Unmarshal(w.Body.Bytes(), &next)
	if w = peerCall(s, "DELETE", "/api/peers/"+id, map[string]string{"expectedDigest": saved.Digest}); w.Code != 409 {
		t.Fatal("stale delete accepted", w.Code)
	}
	if w = peerCall(s, "DELETE", "/api/peers/"+id, map[string]string{"expectedDigest": next.Digest}); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = peerCall(s, "GET", "/api/peers/"+id, nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = peerCall(s, "GET", "/api/health", nil)
	var health struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	json.Unmarshal(w.Body.Bytes(), &health)
	if !health.Capabilities["workspaceSyncV1"] {
		t.Fatal("peer capability missing")
	}
}
func TestPeerHTTPRejectsUnknownFieldsAndReportsIndividualCorruption(t *testing.T) {
	s := testServer(t)
	id := strings.Repeat("a", 48)
	w := peerCall(s, "PUT", "/api/peers/"+id, map[string]any{"expectedDigest": "", "snapshot": peers.Snapshot{}, "command": "do not execute"})
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	bad := filepath.Join(s.stateDir, "peers", id+".json")
	if err := os.WriteFile(bad, []byte("not a valid snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	w = peerCall(s, "GET", "/api/peers", nil)
	var index peers.Index
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &index) != nil || len(index.Errors) != 1 || index.Errors[0].SourceID != id {
		t.Fatal(w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(bad)
	if string(data) != "not a valid snapshot" {
		t.Fatal("corruption modified")
	}
	s.peerStore = nil
	if w = peerCall(s, "GET", "/api/peers", nil); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if w = peerCall(s, "GET", "/api/health", nil); w.Code != 200 {
		t.Fatal("bad peer store disabled unrelated runtime API", w.Code)
	}
}
