// Package conversations defines the bounded, metadata-only saved-conversation API.
package conversations

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const Limit = 100

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidID(id string) bool { return uuid.MatchString(id) }

type Entry struct {
	ID          string    `json:"id"`
	Harness     string    `json:"harness"`
	Title       string    `json:"title"`
	TitleSource string    `json:"titleSource"`
	Cwd         string    `json:"cwd"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
type Listing struct {
	Cwd             string  `json:"cwd"`
	Harness         string  `json:"harness"`
	Conversations   []Entry `json:"conversations"`
	Truncated       bool    `json:"truncated"`
	ResumeSupported bool    `json:"resumeSupported"`
	RenameMode      string  `json:"renameMode"`
	Detail          string  `json:"detail,omitempty"`
}
type Request struct {
	Operation string `json:"operation"`
	Harness   string `json:"harness"`
	Cwd       string `json:"cwd"`
	ID        string `json:"id,omitempty"`
	Title     string `json:"title,omitempty"`
}

func ValidTitle(title string) bool {
	return strings.TrimSpace(title) != "" && len(title) <= 120 && utf8.ValidString(title) && !strings.ContainsFunc(title, unicode.IsControl)
}
func (r Request) Validate() error {
	if r.Harness != "claude" && r.Harness != "codex" {
		return errors.New("Select Claude Code or Codex")
	}
	if r.Cwd == "" || len(r.Cwd) > 4096 || !utf8.ValidString(r.Cwd) || strings.ContainsAny(r.Cwd, "\x00\r\n") {
		return errors.New("Select a project folder before reading conversations")
	}
	if r.Operation != "list" && r.Operation != "lookup" && r.Operation != "rename" {
		return errors.New("Unknown conversation operation")
	}
	if r.Operation != "list" && !ValidID(r.ID) {
		return errors.New("Conversation ID must be a lowercase UUID")
	}
	if r.Operation == "rename" && !ValidTitle(r.Title) {
		return errors.New("Conversation name must contain 1–120 bytes without control characters")
	}
	return nil
}
