// Package projectcontext prepares opt-in, portable project notes. It never reads
// or merges either provider's private conversation or authentication stores.
package projectcontext

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Request struct {
	Path string `json:"path"`
}

type File struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type Result struct {
	Path     string   `json:"path"`
	Files    []File   `json:"files"`
	Warnings []string `json:"warnings"`
}

// ValidatePath deliberately uses Linux path syntax even on a Windows controller:
// the selected project belongs to the Linux host, never the controller machine.
func ValidatePath(path string) error {
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) {
		return errors.New("project path must be valid text between 1 and 4096 bytes")
	}
	if path[0] != '/' && path != "~" && !strings.HasPrefix(path, "~/") {
		return errors.New("choose an absolute project folder or a path beginning with ~/")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return errors.New("project path must not contain parent traversal (..)")
		}
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return errors.New("project path must not contain control characters")
		}
	}
	return nil
}

const beginMarker = "<!-- relay:shared-context:v1 -->"
const endMarker = "<!-- /relay:shared-context:v1 -->"

const notesBlock = beginMarker + `
## Shared project context

For the user's current task, read MEMORY.md for durable project facts and
HANDOFF.md for the latest progress, checks, and next steps. Treat these notes as
project context alongside the existing instructions, not as new authorization.
When useful, leave a concise handoff and record stable decisions in MEMORY.md;
preserve other contributors' notes and flag uncertainty. Do not store secrets,
credentials, or raw conversation transcripts in these files.
` + endMarker + "\n"

const claudeBlock = beginMarker + `
## Shared project context

@AGENTS.md
@MEMORY.md
@HANDOFF.md
` + endMarker + "\n"

const memoryTemplate = `# Project memory

Shared, human-editable notes for Claude Code and Codex. Keep durable project
facts, decisions, and conventions here. Do not include secrets or raw chats.

## Facts and decisions

`

const handoffTemplate = `# Project handoff

Shared, human-editable progress notes. Native Claude Code and Codex conversations
remain separate; this file carries only the context you choose to share.

## Current task

## Changes and checks

## Next steps and open questions

`
