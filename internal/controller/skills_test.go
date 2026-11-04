package controller

import (
	"bytes"
	"encoding/json"
	"github.com/TwoD97/relay/internal/skillmanager"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const skillMarkdown = "---\nname: fixture-skill\ndescription: Test-owned instructions.\n---\nNever execute during import.\n"

func TestSkillsImportExportCASAndUnavailableHost(t *testing.T) {
	s := &Server{config: Config{StateDir: t.TempDir()}}
	mux := http.NewServeMux()
	s.RegisterSkillRoutes(mux)
	call := func(method, path, body, typ string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", typ)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	inspect := call("POST", "/api/skills/inspect", skillMarkdown, "text/markdown")
	if inspect.Code != 200 {
		t.Fatal(inspect.Body.String())
	}
	before := call("GET", "/api/skills", "", "")
	var empty skillmanager.Catalog
	_ = json.Unmarshal(before.Body.Bytes(), &empty)
	if empty.Revision != 0 || len(empty.Entries) != 0 {
		t.Fatal("inspection persisted data")
	}
	saved := call("POST", "/api/skills/import?revision=0", skillMarkdown, "text/markdown")
	if saved.Code != 200 {
		t.Fatal(saved.Body.String())
	}
	var catalog skillmanager.Catalog
	_ = json.Unmarshal(saved.Body.Bytes(), &catalog)
	if catalog.Revision != 1 || len(catalog.Entries) != 1 {
		t.Fatal(catalog)
	}
	digest := catalog.Entries[0].Digest
	stale := call("POST", "/api/skills/import?revision=0", skillMarkdown, "text/markdown")
	if stale.Code != 409 {
		t.Fatal(stale.Code)
	}
	export := call("GET", "/api/skills/fixture-skill/export?digest="+digest, "", "")
	if export.Code != 200 || export.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(export.Code)
	}
	pkg, e := skillmanager.Parse(export.Body.Bytes(), true)
	if e != nil || pkg.Digest != digest {
		t.Fatal(pkg, e)
	}
	bad := call("DELETE", "/api/skills/fixture-skill", `{"revision":1,"digest":"`+digest+`","extra":true}`, "application/json")
	if bad.Code != 400 {
		t.Fatal(bad.Code)
	}
	gone := call("DELETE", "/api/skills/fixture-skill", `{"revision":1,"digest":"`+digest+`"}`, "application/json")
	if gone.Code != 200 {
		t.Fatal(gone.Body.String())
	}
	if got := call("POST", "/api/hosts/local/skills/check", "", ""); got.Code != 404 {
		t.Fatal(got.Code)
	}
}
func TestSkillsRequireCookieCSRFAndSameOrigin(t *testing.T) {
	s, ts, client := testController(t, "")
	do := func(csrf, origin string) *http.Response {
		r, _ := http.NewRequest("POST", ts.URL+"/api/skills/import?revision=0", strings.NewReader(skillMarkdown))
		r.Header.Set("Content-Type", "text/markdown")
		if csrf != "" {
			r.Header.Set("X-Relay-CSRF", csrf)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		res, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	if got := do("", ts.URL).StatusCode; got != 401 {
		t.Fatal(got)
	}
	login(t, s, client)
	if got := do("", ts.URL).StatusCode; got != 403 {
		t.Fatal(got)
	}
	if got := do(s.csrf, "https://untrusted.invalid").StatusCode; got != 403 {
		t.Fatal(got)
	}
	if got := do(s.csrf, ts.URL).StatusCode; got != 200 {
		t.Fatal(got)
	}
	catalog, e := skillmanager.List(s.skillLibrary())
	if e != nil || catalog.Revision != 1 {
		t.Fatal(catalog, e)
	}
}
func TestSkillsRejectOversizedImportsBeforePublication(t *testing.T) {
	s := &Server{config: Config{StateDir: t.TempDir()}}
	mux := http.NewServeMux()
	s.RegisterSkillRoutes(mux)
	r := httptest.NewRequest("POST", "/api/skills/import?revision=0", bytes.NewReader(make([]byte, skillmanager.MaxBytes+1)))
	r.Header.Set("Content-Type", "application/zip")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	c, e := skillmanager.List(s.skillLibrary())
	if e != nil || c.Revision != 0 {
		t.Fatal(c, e)
	}
}
