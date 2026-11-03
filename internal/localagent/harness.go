package localagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

var harnessNames = map[string]string{"claude": "Claude Code", "codex": "Codex"}

type Harness struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Installed     bool              `json:"installed"`
	Path          string            `json:"path,omitempty"`
	Version       string            `json:"version,omitempty"`
	Authenticated *bool             `json:"authenticated,omitempty"`
	AuthDetail    string            `json:"authDetail"`
	SessionModes  []string          `json:"sessionModes"`
	Management    HarnessManagement `json:"management"`
}
type HarnessManagement struct {
	SupportedActions []string `json:"supportedActions"`
	Source           string   `json:"source"`
	Strategy         string   `json:"strategy"`
	Detail           string   `json:"detail"`
}

func setEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, key) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}
func (s *Server) environment(extra map[string]string) []string {
	// Provider HOME/config/authentication remain where the user configured them.
	// A coordinator must not inherit another Relay session's hook credentials.
	env := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "RELAY_") {
			env = append(env, entry)
		}
	}
	env = setEnv(env, "TERM", "xterm-256color")
	env = setEnv(env, "COLORTERM", "truecolor")
	env = setEnv(env, "RELAY_BIN", s.executable)
	for name, value := range extra {
		env = setEnv(env, name, value)
	}
	return env
}
func (s *Server) providerEnvironment(id, path string, extra map[string]string) []string {
	env := s.environment(extra)
	if id == "claude" && s.managedHarness(path) {
		env = setEnv(env, "DISABLE_AUTOUPDATER", "1")
		env = setEnv(env, "DISABLE_UPDATES", "1")
	}
	if id == "codex" {
		// Older official native packages put rg alongside the native subtree.
		// Newer releases resolve their adjacent runtime resources themselves.
		tools := filepath.Join(filepath.Dir(filepath.Dir(path)), "path")
		if info, err := os.Stat(tools); err == nil && info.IsDir() {
			env = setEnv(env, "PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	return env
}
func (s *Server) managedHarness(path string) bool {
	relative, err := filepath.Rel(filepath.Join(s.stateDir, "tools", "harnesses"), path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Server) discover(id string) (string, error) {
	if !validHarness(id) {
		return "", errors.New("unsupported harness")
	}
	if path, err := currentHarness(s.stateDir, id); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	paths := filepath.SplitList(os.Getenv("PATH"))
	paths = append(paths, filepath.Join(s.home, ".local", "bin"))
	if runtime.GOOS == "windows" {
		paths = append(paths, filepath.Join(os.Getenv("APPDATA"), "npm"))
	}
	for _, dir := range paths {
		if !filepath.IsAbs(dir) {
			continue
		}
		name := id
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(dir, name)
		if executableFile(path) {
			return path, nil
		}
		if id == "codex" && runtime.GOOS == "windows" {
			// npm's .cmd/.ps1 wrappers are never evaluated by a shell. Resolve the
			// official package's native payload with its adjacent helper binaries.
			arch, triple, err := codexPlatform()
			if err != nil {
				continue
			}
			packageRoot := filepath.Join(dir, "node_modules", "@openai", "codex")
			for _, root := range []string{packageRoot, filepath.Join(packageRoot, "node_modules", "@openai", "codex-"+arch)} {
				for _, folder := range []string{"bin", "codex"} {
					candidate := filepath.Join(root, "vendor", triple, folder, "codex.exe")
					if executableFile(candidate) {
						return candidate, nil
					}
				}
			}
		}
	}
	return "", errors.New("harness is not installed")
}
func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0)
}

const localAutoInstruction = "Relay Auto mode was explicitly selected for this session. Complete the user's requested task end-to-end using available tools and permissions. Respect project instructions and organization policy, preserve credentials and unrelated sessions, and do not invent work. If the user has not supplied a task, wait for it."

func providerArguments(id, mode, prompt string) []string {
	if mode == "auto" {
		prompt = localAutoInstruction + "\n\n" + prompt
	}
	if id == "claude" {
		args := []string{}
		if mode == "auto" {
			args = append(args, "--dangerously-skip-permissions")
		}
		return append(args, "--append-system-prompt", prompt)
	}
	args := []string{}
	if mode == "auto" {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	return append(args, "--", prompt)
}
func (s *Server) probe(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	provider := strings.TrimSuffix(filepath.Base(path), ".exe")
	data, err := runProbe(ctx, path, args, s.providerEnvironment(provider, path, nil))
	return ansi.Strip(string(data)), err
}
func advertisedFlag(help, flag string) bool {
	for _, word := range strings.Fields(help) {
		if strings.Trim(word, ",[]") == flag {
			return true
		}
	}
	return false
}
func (s *Server) validateProvider(ctx context.Context, id, path, mode string) error {
	if !validHarness(id) || (mode != "manual" && mode != "auto") {
		return errors.New("invalid provider or permission mode")
	}
	help, err := s.probe(ctx, path, "--help")
	if err != nil {
		return errors.New("Could not verify this local CLI's launch flags. Update or repair it before starting Mastermind")
	}
	flags := []string{}
	if id == "claude" {
		flags = append(flags, "--append-system-prompt")
		if mode == "auto" {
			flags = append(flags, "--dangerously-skip-permissions")
		}
	} else {
		if !strings.Contains(help, "[PROMPT]") {
			return errors.New("This local Codex CLI does not support the coordinator prompt; update or repair it")
		}
		if mode == "auto" {
			flags = append(flags, "--dangerously-bypass-approvals-and-sandbox")
		}
	}
	for _, flag := range flags {
		if !advertisedFlag(help, flag) {
			return fmt.Errorf("This local %s CLI does not advertise the required %s flag. Update or repair it; no session started", harnessNames[id], flag)
		}
	}
	return nil
}
func (s *Server) inspectHarness(ctx context.Context, id string) Harness {
	h := Harness{ID: id, Name: harnessNames[id], SessionModes: []string{"manual", "auto"}, AuthDetail: "Install this harness to continue", Management: HarnessManagement{SupportedActions: []string{"install", "update", "repair"}, Source: "missing", Strategy: "native", Detail: "Verified releases are installed privately. Existing authentication and running sessions are preserved."}}
	path, err := s.discover(id)
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(s.stateDir, "tools", "harnesses", id, "current.json")); statErr == nil {
			h.Management.Source = "relay"
			h.AuthDetail = "Managed installation could not be verified. Repair it before starting a session."
		}
		return h
	}
	h.Installed = true
	h.Path = path
	h.Management.Source = "external"
	if s.managedHarness(path) {
		h.Management.Source = "relay"
	}
	version, err := s.probe(ctx, path, "--version")
	version = strings.TrimSpace(version)
	if err == nil && len(version) < 200 && !strings.ContainsAny(version, "\r\n\x1b") {
		h.Version = version
	} else if s.managedHarness(path) {
		h.Installed = false
		h.AuthDetail = "Managed installation could not run. Repair it before starting a session."
		return h
	}
	args := []string{"login", "status"}
	if id == "claude" {
		args = []string{"auth", "status"}
	}
	status, err := s.probe(ctx, path, args...)
	// Auth-status text can contain account details. Never return or log it.
	h.Authenticated, h.AuthDetail = harnessAuthentication(id, status, err)
	return h
}

func harnessAuthentication(id, output string, err error) (*bool, string) {
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if id == "claude" {
		// Decode only this boolean, never retain or expose other account fields.
		if json.Unmarshal([]byte(output), &status) != nil {
			status.LoggedIn = nil
		}
	}
	if err == nil {
		if status.LoggedIn != nil && !*status.LoggedIn {
			return status.LoggedIn, "The CLI reports no active login"
		}
		yes := true
		return &yes, "The CLI reports an existing login"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, "The CLI login check timed out. Open the sign-in terminal to inspect it."
	}
	if errors.Is(err, context.Canceled) {
		return nil, "The CLI login check was interrupted. Check setup again."
	}
	var exit *probeExitError
	if errors.As(err, &exit) {
		if exit.code == 1 && (id == "codex" || status.LoggedIn != nil && !*status.LoggedIn) {
			no := false
			return &no, "The CLI reports no active login"
		}
		return nil, fmt.Sprintf("The CLI login check exited with code %d without a verified login status. Open the sign-in terminal to inspect it.", exit.code)
	}
	return nil, "Could not run the CLI login check. Open the sign-in terminal to inspect it."
}
func (s *Server) harnesses(w http.ResponseWriter, r *http.Request) {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()
	if s.harnessCache != nil && time.Since(s.harnessCacheAt) < 20*time.Second {
		jsonResponse(w, 200, s.harnessCache)
		return
	}
	result := make([]Harness, 2)
	done := make(chan int, 2)
	for i, id := range []string{"claude", "codex"} {
		go func() { result[i] = s.inspectHarness(r.Context(), id); done <- i }()
	}
	<-done
	<-done
	if r.Context().Err() == nil {
		s.harnessCache, s.harnessCacheAt = result, time.Now()
	}
	jsonResponse(w, 200, result)
}
func (s *Server) harnessAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if !validHarness(id) || (action != "login" && action != "install" && action != "update" && action != "repair") {
		fail(w, 404, errors.New("unknown harness action"))
		return
	}
	meta := Session{Title: map[string]string{"login": "Sign in to ", "install": "Install ", "update": "Update ", "repair": "Repair "}[action] + harnessNames[id], Workspace: "Setup", Cwd: s.home, Harness: id, Purpose: action}
	path, args := s.executable, []string{"local-agent-install", "--state-dir", s.stateDir, "--harness", id, "--action", action}
	if action == "login" {
		var err error
		path, err = s.discover(id)
		if err != nil {
			fail(w, 409, errors.New("Install this local harness before signing in"))
			return
		}
		args = []string{"login", "--device-auth"}
		if id == "claude" {
			args = []string{"auth", "login"}
		}
	}
	result, err := s.start(meta, "", 100, 30, command{Path: path, Args: args, Dir: s.home, Env: s.providerEnvironment(id, path, nil)})
	if err != nil {
		fail(w, 409, err)
		return
	}
	if p, err := s.lookup(result.ID); err == nil {
		go func() { <-p.done; s.harnessMu.Lock(); s.harnessCacheAt = time.Time{}; s.harnessMu.Unlock() }()
	}
	jsonResponse(w, 201, result)
}

type harnessPointer struct {
	Format     int    `json:"format"`
	Harness    string `json:"harness"`
	Release    string `json:"release"`
	Executable string `json:"executable"`
}

func currentHarness(stateDir, id string) (string, error) {
	data, err := readPrivate(filepath.Join(stateDir, "tools", "harnesses", id, "current.json"), 4096)
	if err != nil {
		return "", err
	}
	var current harnessPointer
	if json.Unmarshal(data, &current) != nil || current.Format != 1 || current.Harness != id || !releaseDirectory.MatchString(current.Release) || !safeArchivePath(current.Executable) {
		return "", errors.New("invalid managed harness pointer")
	}
	path := filepath.Join(stateDir, "tools", "harnesses", id, "releases", current.Release, filepath.FromSlash(current.Executable))
	if !executableFile(path) {
		return "", errors.New("managed harness executable is unavailable")
	}
	return path, nil
}
