//go:build linux

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/TwoD97/relay/internal/conversations"
)

// Claude's official SDK uses bounded head/tail reads for metadata. This reader
// follows that documented storage layout, but never extracts prompts/messages:
// https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/sessions.py
// Exact cwd evidence also rejects collisions in Claude's sanitized project names.
func claudeProjectPrefix(cwd string) string {
	var b strings.Builder
	for _, c := range cwd {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (s *Server) claudeConversations(ctx context.Context, q conversations.Request) (any, error) {
	listing := conversations.Listing{Cwd: q.Cwd, Harness: "claude", Conversations: []conversations.Entry{}, RenameMode: "relay"}
	if configured := os.Getenv("CLAUDE_CONFIG_DIR"); configured != "" && !filepath.IsAbs(configured) {
		return nil, errors.New("CLAUDE_CONFIG_DIR must be absolute to safely browse saved conversations")
	}
	projects := filepath.Join(conversationConfigHome("CLAUDE_CONFIG_DIR", s.home, ".claude"), "projects")
	root, err := os.OpenRoot(projects)
	if errors.Is(err, os.ErrNotExist) {
		if q.Operation == "list" {
			return listing, nil
		}
		return nil, errors.New("Saved conversation was not found in this project")
	}
	if err != nil {
		return nil, errors.New("Could not read Claude Code's saved conversation metadata")
	}
	defer root.Close()
	prefix := claudeProjectPrefix(q.Cwd)
	dirs := []string{prefix}
	if len(prefix) > 200 {
		prefix = prefix[:200] + "-"
		dirs = nil
		f, e := root.Open(".")
		if e != nil {
			return nil, e
		}
		defer f.Close()
		entries, e := f.ReadDir(2049)
		if e != nil && e != io.EOF {
			return nil, e
		}
		if len(entries) > 2048 {
			listing.Truncated = true
			entries = entries[:2048]
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
				if len(dirs) == 64 {
					listing.Truncated = true
					break
				}
				dirs = append(dirs, entry.Name())
			}
		}
	}
	type candidate struct {
		path string
		info os.FileInfo
	}
	var files []candidate
	scanned := 0
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if info, e := root.Lstat(dir); e != nil || !info.IsDir() {
			continue
		}
		if q.Operation != "list" {
			path := filepath.Join(dir, q.ID+".jsonl")
			if info, e := root.Lstat(path); e == nil && info.Mode().IsRegular() {
				files = append(files, candidate{path, info})
			}
			continue
		}
		if scanned >= 4096 {
			listing.Truncated = true
			break
		}
		f, e := root.Open(dir)
		if e != nil {
			continue
		}
		entries, e := f.ReadDir(4097 - scanned)
		f.Close()
		if e != nil && e != io.EOF {
			return nil, e
		}
		if len(entries) > 4096-scanned {
			listing.Truncated = true
			entries = entries[:4096-scanned]
		}
		scanned += len(entries)
		for _, entry := range entries {
			name := entry.Name()
			id := strings.TrimSuffix(name, ".jsonl")
			if !strings.HasSuffix(name, ".jsonl") || !conversations.ValidID(id) || entry.Type()&os.ModeSymlink != 0 || (q.Operation != "list" && id != q.ID) {
				continue
			}
			info, e := entry.Info()
			if e == nil && info.Mode().IsRegular() {
				files = append(files, candidate{filepath.Join(dir, name), info})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	// At most 256 files x 128 KiB are inspected, even for very large projects.
	if len(files) > 256 {
		files = files[:256]
		listing.Truncated = true
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, ok := readClaudeMetadata(root, file.path, q.Cwd)
		if !ok {
			continue
		}
		if q.Operation == "rename" {
			if err = s.writeConversationAlias(entry, q.Title); err != nil {
				return nil, err
			}
			entry.Title, entry.TitleSource = q.Title, "relay"
			return entry, nil
		}
		if err = s.applyConversationAlias(&entry); err != nil {
			return nil, err
		}
		if q.Operation == "lookup" {
			return entry, nil
		}
		if len(listing.Conversations) == conversations.Limit {
			listing.Truncated = true
			break
		}
		listing.Conversations = append(listing.Conversations, entry)
	}
	if q.Operation != "list" {
		return nil, errors.New("Saved conversation was not found in this project")
	}
	return listing, nil
}

func readClaudeMetadata(root *os.Root, path, cwd string) (conversations.Entry, bool) {
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return conversations.Entry{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return conversations.Entry{}, false
	}
	entry := conversations.Entry{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Harness: "claude", Cwd: cwd, UpdatedAt: info.ModTime().UTC(), TitleSource: "fallback"}
	entry.Title = "Claude Code · " + entry.ID[:8]
	var actualCwd, custom, generated string
	sidechain := false
	for index, offset := range []int64{0, max(0, info.Size()-(64<<10))} {
		if index == 1 && offset == 0 {
			break
		}
		data := make([]byte, 64<<10)
		n, _ := f.ReadAt(data, offset)
		data = data[:n]
		if offset > 0 {
			if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
				data = data[cut+1:]
			} else {
				continue
			}
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var rec struct {
				Type        string `json:"type"`
				Cwd         string `json:"cwd"`
				SessionID   string `json:"sessionId"`
				IsSidechain bool   `json:"isSidechain"`
				CustomTitle string `json:"customTitle"`
				AITitle     string `json:"aiTitle"`
			}
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			if rec.SessionID != "" && strings.ToLower(rec.SessionID) != entry.ID {
				return entry, false
			}
			if actualCwd == "" && rec.Cwd != "" {
				actualCwd = filepath.Clean(rec.Cwd)
			}
			sidechain = sidechain || rec.IsSidechain
			if rec.Type == "custom-title" && conversations.ValidTitle(rec.CustomTitle) {
				custom = rec.CustomTitle
			}
			if rec.Type == "ai-title" && conversations.ValidTitle(rec.AITitle) {
				generated = rec.AITitle
			}
		}
	}
	if sidechain || actualCwd != cwd {
		return entry, false
	}
	if custom != "" {
		entry.Title, entry.TitleSource = custom, "provider"
	} else if generated != "" {
		entry.Title, entry.TitleSource = generated, "provider"
	}
	return entry, true
}

func (s *Server) conversationAliasPath(entry conversations.Entry) string {
	h := sha256.Sum256([]byte(entry.Cwd))
	return filepath.Join(s.stateDir, "conversation-aliases", entry.Harness+"-"+entry.ID+"-"+hex.EncodeToString(h[:])+".json")
}
func (s *Server) applyConversationAlias(entry *conversations.Entry) error {
	path := s.conversationAliasPath(*entry)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Could not read the saved Relay conversation name")
	}
	f := os.NewFile(uintptr(fd), "conversation-alias")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return errors.New("Invalid Relay conversation name file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return errors.New("Could not read the saved Relay conversation name")
	}
	var saved conversations.Entry
	if json.Unmarshal(data, &saved) != nil || saved.ID != entry.ID || saved.Cwd != entry.Cwd || saved.Harness != entry.Harness || !conversations.ValidTitle(saved.Title) {
		return errors.New("Saved Relay conversation name is invalid; the provider history was preserved")
	}
	entry.Title, entry.TitleSource = saved.Title, "relay"
	return nil
}
func (s *Server) writeConversationAlias(entry conversations.Entry, title string) error {
	if err := privateDir(s.stateDir); err != nil {
		return err
	}
	path := s.conversationAliasPath(entry)
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("Relay conversation name path is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entry.Title, entry.TitleSource = title, "relay"
	data, _ := json.Marshal(entry)
	return atomicPrivateWrite(path, data)
}
