package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runDirectoryScript(t *testing.T, query DirectoryQuery) (DirectoryListing, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", directoryScript(query)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return parseDirectoryOutput(output, query)
}

func TestDirectoryScriptLiteralPathsFilteringAndCanonicalNavigation(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "project ' $(printf injected) `literal` [*] spaces")
	for _, name := range []string{"alpha", "alphabet", "other", ".hidden", "quote' $() [*]", "quote' $() [*]suffix", "quote' $() Xsuffix", "line\nbreak", "deep/child"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "file"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "alpha"), filepath.Join(directory, "linked")); err != nil {
		t.Fatal(err)
	}
	listing, err := runDirectoryScript(t, DirectoryQuery{Path: directory})
	if err != nil || listing.Path != directory || listing.Parent == nil || *listing.Parent != root || listing.Truncated {
		t.Fatalf("listing metadata: %#v %v", listing, err)
	}
	names := map[string]bool{}
	for _, entry := range listing.Directories {
		names[entry.Name] = true
		if entry.Path != filepath.Join(directory, entry.Name) {
			t.Fatal("entry path changed", entry)
		}
	}
	if len(names) != 8 || !names["linked"] || names[".hidden"] || names["file"] || names["child"] || names["line\nbreak"] {
		t.Fatal("unexpected listing", names)
	}
	for _, query := range []DirectoryQuery{{Path: directory, Prefix: "quote' $() [*]"}, {Path: directory, Prefix: "alpha"}, {Path: directory, Prefix: "."}, {Path: directory, Hidden: true}} {
		got, err := runDirectoryScript(t, query)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if query.Prefix == "." {
			want = 1
		}
		if query.Hidden {
			want = 9
		}
		if len(got.Directories) != want {
			t.Fatalf("literal filter %#v: %#v", query, got.Directories)
		}
	}
	linked, err := runDirectoryScript(t, DirectoryQuery{Path: filepath.Join(directory, "linked")})
	if err != nil || linked.Path != filepath.Join(directory, "alpha") {
		t.Fatal("directory symlink did not resolve", linked, err)
	}
}

func TestDirectoryScriptErrorsLimitsAndHome(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		expected error
	}{{"missing", ErrDirectoryNotFound}, {"file", ErrNotDirectory}} {
		if _, err := runDirectoryScript(t, DirectoryQuery{Path: filepath.Join(directory, tc.name)}); !errors.Is(err, tc.expected) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if os.Geteuid() != 0 {
		private := filepath.Join(directory, "inaccessible")
		if err := os.Mkdir(private, 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(private, 0700) })
		if _, err := runDirectoryScript(t, DirectoryQuery{Path: private}); !errors.Is(err, ErrDirectoryPermission) {
			t.Fatal("inaccessible folder", err)
		}
	}
	for i := 0; i < 750; i++ {
		if err := os.Mkdir(filepath.Join(directory, fmt.Sprintf("folder-%04d-%s", i, strings.Repeat("x", 220))), 0700); err != nil {
			t.Fatal(err)
		}
	}
	large, err := runDirectoryScript(t, DirectoryQuery{Path: directory, Prefix: "folder-"})
	if err != nil || !large.Truncated || len(large.Directories) != DirectoryLimit {
		t.Fatal("large listing not bounded", len(large.Directories), large.Truncated, err)
	}
	match, err := runDirectoryScript(t, DirectoryQuery{Path: directory, Prefix: "folder-0749-"})
	if err != nil || match.Truncated || len(match.Directories) != 1 {
		t.Fatal("prefix beyond first page missing", match, err)
	}
	home, err := runDirectoryScript(t, DirectoryQuery{Path: "~", Prefix: "relay-test-prefix-does-not-exist"})
	if err != nil || home.Path == "" || home.Home == "" {
		t.Fatal("home resolution", home, err)
	}
	root, err := runDirectoryScript(t, DirectoryQuery{Path: "/", Prefix: "relay-test-prefix-does-not-exist"})
	if err != nil || root.Parent != nil {
		t.Fatal("root parent must be null", root, err)
	}
}

func TestDirectoryQueryRejectsUnsupportedPathsAndPartialOutput(t *testing.T) {
	for _, query := range []DirectoryQuery{{Path: "relative"}, {Path: "~other"}, {Path: "/bad\npath"}, {Path: "/bad\x00path"}, {Path: strings.Repeat("/", 4097)}, {Prefix: "../"}, {Prefix: "bad\rname"}} {
		if query.Validate() == nil {
			t.Fatalf("accepted %#v", query)
		}
	}
	listing, err := parseDirectoryOutput([]byte("relay-directories-v1\x00ok\x00/home/test\x00/tmp\x00./whole\x00./part\xe2\x82"), DirectoryQuery{})
	if err != nil || !listing.Truncated || len(listing.Directories) != 1 || listing.Directories[0].Name != "whole" {
		t.Fatal("partial name became selectable", listing, err)
	}
}
