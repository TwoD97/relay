//go:build linux

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/TwoD97/relay/internal/conversations"
)

// ConversationOperation does not instantiate a daemon, load session metadata,
// recover processes, authenticate, or start/resume a provider conversation.
// It can run beside a compatible older daemon using a refreshed Relay binary.
func ConversationOperation(ctx context.Context, stateDir string, request conversations.Request) (any, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	s := &Server{stateDir: stateDir, home: home}
	cwd, err := s.resolveCwd(request.Cwd)
	if err != nil {
		return nil, errors.New("The selected project folder is unavailable")
	}
	request.Cwd = cwd
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if request.Harness == "claude" {
		return s.claudeConversations(ctx, request)
	}
	path, err := s.findCommand("codex")
	if err != nil {
		return nil, errors.New("Install Codex to read its saved conversations")
	}
	return s.codexConversations(ctx, path, request)
}

func (s *Server) conversationLookup(ctx context.Context, harness, cwd, id string) (conversations.Entry, error) {
	value, err := ConversationOperation(ctx, s.stateDir, conversations.Request{Operation: "lookup", Harness: harness, Cwd: cwd, ID: id})
	if err != nil {
		return conversations.Entry{}, err
	}
	entry, ok := value.(conversations.Entry)
	if !ok || entry.ID != id || entry.Cwd != cwd || entry.Harness != harness {
		return conversations.Entry{}, errors.New("Conversation does not belong to the selected project")
	}
	return entry, nil
}

func conversationConfigHome(env, home, fallback string) string {
	if value := os.Getenv(env); filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(home, fallback)
}
