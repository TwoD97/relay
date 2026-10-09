//go:build linux

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var harnessNames = map[string]string{"codex": "Codex", "claude": "Claude Code"}

type Harness struct {
	Management    *HarnessManagement `json:"management,omitempty"`
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Installed     bool               `json:"installed"`
	Path          string             `json:"path,omitempty"`
	Version       string             `json:"version,omitempty"`
	Authenticated *bool              `json:"authenticated,omitempty"`
	AuthDetail    string             `json:"authDetail,omitempty"`
}

func (s *Server) environment() []string {
	env := os.Environ()
	paths := []string{filepath.Join(s.stateDir, "tools", nodeArchiveRoot(), "bin")}
	paths = append(paths, filepath.Join(s.stateDir, "bin"))
	paths = append(paths, filepath.SplitList(os.Getenv("PATH"))...)
	paths = append(paths, filepath.Join(s.home, ".local", "bin"), filepath.Join(s.home, ".npm-global", "bin"), filepath.Join(s.stateDir, "tools", "harnesses", "codex", "bin"), filepath.Join(s.stateDir, "tools", "harnesses", "claude", "bin"), "/usr/local/bin", "/usr/bin", "/bin")
	safe := make([]string, 0, len(paths))
	seen := make(map[string]bool)
	for _, p := range paths {
		if filepath.IsAbs(p) && !seen[p] {
			safe = append(safe, p)
			seen[p] = true
		}
	}
	env = setEnv(env, "PATH", strings.Join(safe, string(os.PathListSeparator)))
	env = setEnv(env, "TERM", "xterm-256color")
	env = setEnv(env, "COLORTERM", "truecolor")
	if executable, err := os.Executable(); err == nil {
		env = setEnv(env, "RELAY_BIN", executable)
	}
	return env
}
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, v := range env {
		if !strings.HasPrefix(v, key+"=") {
			out = append(out, v)
		}
	}
	return append(out, key+"="+value)
}
func envValue(env []string, key string) string {
	for _, v := range env {
		if strings.HasPrefix(v, key+"=") {
			return strings.TrimPrefix(v, key+"=")
		}
	}
	return ""
}
func (s *Server) findCommand(name string) (string, error) {
	for _, dir := range filepath.SplitList(envValue(s.environment(), "PATH")) {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not installed", name)
}
func (s *Server) harnesses(w http.ResponseWriter, r *http.Request) {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()
	if time.Since(s.harnessCacheAt) > 20*time.Second || s.harnessCache == nil {
		list := make([]Harness, 2)
		var wg sync.WaitGroup
		for i, id := range []string{"claude", "codex"} {
			wg.Add(1)
			go func(i int, id string) { defer wg.Done(); list[i] = s.inspectHarness(r.Context(), id) }(i, id)
		}
		wg.Wait()
		s.harnessCache = list
		s.harnessCacheAt = time.Now()
	}
	jsonResponse(w, 200, s.harnessCache)
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		_, _ = b.buffer.Write(data[:min(len(data), remaining)])
	}
	return n, nil
}
func (s *Server) probe(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = s.environment()
	cmd.Dir = s.home
	out := &boundedOutput{limit: 8192}
	cmd.Stdout = out
	cmd.Stderr = out
	err := runIsolatedCommand(ctx, cmd)
	if cancelErr := ctx.Err(); cancelErr != nil {
		if !errors.Is(err, cancelErr) {
			err = errors.Join(cancelErr, err)
		}
		return "", err
	}
	return out.buffer.String(), err
}
func (s *Server) inspectHarness(ctx context.Context, id string) Harness {
	strategy := "npm"
	if id == "claude" {
		strategy = "native"
	}
	h := Harness{ID: id, Name: harnessNames[id], AuthDetail: "Install this harness to continue", Management: &HarnessManagement{SupportedActions: []string{"install", "update", "repair"}, Source: "missing", Strategy: strategy, Detail: "Verified releases are installed alongside existing versions; authentication and running sessions are preserved."}}
	path, err := s.findCommand(id)
	if err != nil {
		return h
	}
	h.Installed = true
	h.Path = path
	h.Management.Source = "external"
	if s.managedHarnessPath(id, path) {
		h.Management.Source = "relay"
	} else {
		h.Management.Detail = "Update or Repair creates a Relay-managed release for future sessions. Your existing installation, authentication, and running sessions are preserved."
	}
	if version, err := s.probe(ctx, path, "--version"); err == nil {
		version = strings.TrimSpace(version)
		if len(version) < 200 && !strings.ContainsAny(version, "\x1b\r\n") {
			h.Version = version
		}
	} else if s.managedHarnessPath(id, path) {
		h.Installed = false
		h.AuthDetail = "Managed installation is incomplete. Install again to repair it."
		return h
	}
	args := []string{"login", "status"}
	if id == "claude" {
		args = []string{"auth", "status"}
	}
	// Deliberately discard provider status output. It can include account details
	// or API-key fragments; only the documented process exit status is exposed.
	_, err = s.probe(ctx, path, args...)
	if err == nil {
		v := true
		h.Authenticated = &v
		h.AuthDetail = "The CLI reports an existing login"
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			v := false
			h.Authenticated = &v
			h.AuthDetail = "The CLI reports no active login"
		} else {
			h.AuthDetail = "Could not verify login; open the login terminal"
		}
	}
	return h
}

func (s *Server) managedHarnessPath(id, path string) bool {
	prefix := filepath.Join(s.stateDir, "tools", "harnesses", id)
	if path == filepath.Join(s.stateDir, "bin", id) {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
	}
	rel, err := filepath.Rel(prefix, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Server) installHarness(w http.ResponseWriter, r *http.Request) {
	s.maintenanceHarness(w, r, "install")
}
func (s *Server) updateHarness(w http.ResponseWriter, r *http.Request) {
	s.maintenanceHarness(w, r, "update")
}
func (s *Server) repairHarness(w http.ResponseWriter, r *http.Request) {
	s.maintenanceHarness(w, r, "repair")
}
func (s *Server) maintenanceHarness(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")
	if _, ok := harnessNames[id]; !ok {
		fail(w, 404, errors.New("unknown harness"))
		return
	}
	s.mu.Lock()
	if s.installing == nil {
		s.installing = make(map[string]bool)
	}
	if s.installing[id] {
		s.mu.Unlock()
		fail(w, 409, errors.New("harness maintenance is already running"))
		return
	}
	s.installing[id] = true
	s.mu.Unlock()
	verb := map[string]string{"install": "Install", "update": "Update", "repair": "Repair"}[action]
	meta, err := s.start(Session{Purpose: action, Title: verb + " " + harnessNames[id], Workspace: "Setup", Cwd: s.home, Harness: id}, 100, 30, func(ctx context.Context, out io.Writer) (*exec.Cmd, error) {
		if err := s.maintainHarness(ctx, id, action, out); err != nil {
			return nil, err
		}
		return exec.Command("/bin/true"), nil
	})
	if err != nil {
		s.mu.Lock()
		delete(s.installing, id)
		s.mu.Unlock()
		fail(w, 409, err)
		return
	}
	p, _ := s.lookup(meta.ID)
	go func() {
		if p != nil {
			<-p.done
		}
		s.mu.Lock()
		delete(s.installing, id)
		s.mu.Unlock()
		s.harnessMu.Lock()
		s.harnessCacheAt = time.Time{}
		s.harnessMu.Unlock()
	}()
	jsonResponse(w, 201, meta)
}
func (s *Server) loginHarness(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := harnessNames[id]; !ok {
		fail(w, 404, errors.New("unknown harness"))
		return
	}
	path, err := s.findCommand(id)
	if err != nil {
		fail(w, 409, err)
		return
	}
	args := []string{"login", "--device-auth"}
	if id == "claude" {
		args = []string{"auth", "login"}
	}
	meta, err := s.start(Session{Purpose: "login", Title: "Sign in to " + harnessNames[id], Workspace: "Setup", Cwd: s.home, Harness: id}, 100, 30, func(_ context.Context, out io.Writer) (*exec.Cmd, error) {
		fmt.Fprintln(out, "Complete the provider login below. Credentials stay with the provider CLI on this machine.\r")
		cmd := exec.Command(path, args...)
		cmd.Env = setEnv(s.environment(), "BROWSER", "/bin/true")
		return cmd, nil
	})
	if err != nil {
		fail(w, 409, err)
		return
	}
	p, _ := s.lookup(meta.ID)
	if p == nil {
		jsonResponse(w, 201, meta)
		return
	}
	go func() { <-p.done; s.harnessMu.Lock(); s.harnessCacheAt = time.Time{}; s.harnessMu.Unlock() }()
	jsonResponse(w, 201, meta)
}
func nodeMajor(version string) int {
	part := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")[0]
	n, _ := strconv.Atoi(part)
	return n
}
