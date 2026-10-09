//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureNode = `#!/bin/sh
if [ "$1" = --version ]; then printf 'v24.21.0\n'; exit 0; fi
case "$1" in */bin/npm) exec /bin/sh "$@" ;; *) exit 1 ;; esac
`
const fixtureNPM = "#!/bin/sh\n[ \"$1\" = --version ] || exit 1\nprintf '11.0.0\\n'\n"

func privateNodeFixture(t *testing.T, mode string) *atomic.Int32 {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tarball := tar.NewWriter(gz)
	npm := fixtureNPM
	if mode == "bad-npm" {
		npm = "#!/bin/sh\nexit 1\n"
	}
	for name, body := range map[string]string{"node": fixtureNode, "npm": npm} {
		header := &tar.Header{Name: nodeArchiveRoot() + "/bin/" + name, Typeflag: tar.TypeReg, Mode: 0700, Size: int64(len(body))}
		if err := tarball.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarball.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tarball.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	digest := sha256.Sum256(data)
	oldDigest := nodeDigests[goruntime.GOARCH]
	nodeDigests[goruntime.GOARCH] = hex.EncodeToString(digest[:])
	original := http.DefaultTransport
	requests := &atomic.Int32{}
	http.DefaultTransport = harnessFixtureTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.String() != "https://nodejs.org/dist/"+nodeVersion+"/"+nodeArchiveRoot()+".tar.gz" {
			return nil, errors.New("unexpected private Node download")
		}
		body := data
		if mode == "checksum" {
			body = []byte("wrong archive digest")
		}
		if mode == "cancel" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original; nodeDigests[goruntime.GOARCH] = oldDigest })
	return requests
}

func nodeOnlyServer(t *testing.T, state string) *Server {
	t.Helper()
	if state == "" {
		state = t.TempDir()
	}
	return &Server{stateDir: state, home: t.TempDir(), toolsMu: make(chan struct{}, 1)}
}

func writeNodeFixture(t *testing.T, s *Server, node, npm string) string {
	t.Helper()
	dir := filepath.Join(s.stateDir, "tools", nodeArchiveRoot(), "bin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"node": node, "npm": npm} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Dir(dir)
}

func TestPrivateNodeRepairVerifiesBothToolsAndRetainsOldFiles(t *testing.T) {
	for _, broken := range []string{"node", "npm"} {
		t.Run(broken, func(t *testing.T) {
			s := nodeOnlyServer(t, "")
			t.Setenv("PATH", t.TempDir())
			requests := privateNodeFixture(t, "ok")
			node, npm := fixtureNode, fixtureNPM
			if broken == "node" {
				node = "#!/bin/sh\nexit 1\n"
			} else {
				npm = "#!/bin/sh\nexit 1\n"
			}
			destination := writeNodeFixture(t, s, node, npm)
			marker := filepath.Join(destination, "existing-files")
			if err := os.WriteFile(marker, []byte("retain this tree"), 0600); err != nil {
				t.Fatal(err)
			}
			previous, err := os.Open(marker)
			if err != nil {
				t.Fatal(err)
			}
			defer previous.Close()
			actual, err := s.ensureNPM(context.Background(), io.Discard)
			if err != nil || actual != filepath.Join(destination, "bin", "npm") {
				t.Fatal("private Node repair failed", actual, err)
			}
			if requests.Load() != 1 {
				t.Fatal("unexpected number of downloads", requests.Load())
			}
			if _, err := s.verifyPrivateNode(context.Background(), destination, actual); err != nil {
				t.Fatal("published toolchain is not usable", err)
			}
			preserved, _ := filepath.Glob(filepath.Join(s.stateDir, "tools", ".node-prepare-*", nodeArchiveRoot(), "existing-files"))
			if len(preserved) != 1 {
				t.Fatal("previous Node files were removed", preserved)
			}
			data, err := io.ReadAll(previous)
			if err != nil || string(data) != "retain this tree" {
				t.Fatal("existing open file changed", string(data), err)
			}
			if _, err := s.ensureNPM(context.Background(), io.Discard); err != nil || requests.Load() != 1 {
				t.Fatal("healthy published toolchain was downloaded again", err)
			}
		})
	}
}

func TestPrivateNodePreparationFailurePreservesPreviousTree(t *testing.T) {
	for _, mode := range []string{"checksum", "bad-npm", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := nodeOnlyServer(t, "")
			t.Setenv("PATH", t.TempDir())
			privateNodeFixture(t, mode)
			destination := writeNodeFixture(t, s, "#!/bin/sh\nexit 1\n", fixtureNPM)
			before, _ := os.Stat(destination)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "cancel" {
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			if _, err := s.ensureNPM(ctx, io.Discard); err == nil {
				t.Fatal("invalid or cancelled preparation succeeded")
			}
			after, _ := os.Stat(destination)
			if !os.SameFile(before, after) {
				t.Fatal("failed preparation replaced the current Node tree")
			}
			leftovers, _ := filepath.Glob(filepath.Join(s.stateDir, "tools", ".node-prepare-*"))
			if len(leftovers) != 0 {
				t.Fatal("failed staged release was not removed", leftovers)
			}
		})
	}
}

func TestIndependentWorkersSerializePrivateNodePublication(t *testing.T) {
	first := nodeOnlyServer(t, "")
	second := nodeOnlyServer(t, first.stateDir)
	t.Setenv("PATH", t.TempDir())
	requests := privateNodeFixture(t, "ok")
	writeNodeFixture(t, first, "#!/bin/sh\nexit 1\n", fixtureNPM)
	results := make(chan error, 2)
	for _, server := range []*Server{first, second} {
		go func(s *Server) { _, err := s.ensureNPM(context.Background(), io.Discard); results <- err }(server)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("independent workers replaced the same private Node tree twice", requests.Load())
	}
}

func TestPrivateNodeLockWaitHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.lock")
	lock, err := lockNodePreparation(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if second, err := lockNodePreparation(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		if second != nil {
			second.Close()
		}
		t.Fatal("lock wait ignored cancellation", err)
	}
}

func TestPrivateNodeRejectsLinkedDestinationAndLock(t *testing.T) {
	for _, name := range []string{nodeArchiveRoot(), ".node.lock"} {
		t.Run(name, func(t *testing.T) {
			s := nodeOnlyServer(t, "")
			tools := filepath.Join(s.stateDir, "tools")
			if err := os.Mkdir(tools, 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(tools, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ensureNPM(context.Background(), io.Discard); err == nil {
				t.Fatal("accepted a linked private Node location")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("wrote through private Node symlink", entries, err)
			}
		})
	}
}

func TestHealthyExternalNodeAndNPMArePreserved(t *testing.T) {
	s := nodeOnlyServer(t, "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	requests := privateNodeFixture(t, "ok")
	for name, body := range map[string]string{"node": fixtureNode, "npm": fixtureNPM} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	npm, err := s.ensureNPM(context.Background(), io.Discard)
	if err != nil || npm != filepath.Join(bin, "npm") || requests.Load() != 0 {
		t.Fatal("healthy external tools were replaced", npm, requests.Load(), err)
	}
	if data, _ := os.ReadFile(npm); !strings.Contains(string(data), "11.0.0") {
		t.Fatal("external npm changed")
	}
}
