package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/TwoD97/relay/internal/workspacestatus"
	agentskill "github.com/TwoD97/relay/skills"
)

func workspaceCommand(args []string) error {
	if len(args) == 2 && args[0] == "template" && args[1] == "status" {
		html, err := workspacestatus.Starter()
		if err != nil {
			return err
		}
		_, err = io.WriteString(os.Stdout, html)
		return err
	}
	if len(args) == 1 && args[0] == "skill" {
		_, err := io.WriteString(os.Stdout, agentskill.MastermindContent)
		return err
	}
	if len(args) == 1 && args[0] == "show" {
		data, err := agentRequest("GET", "/agent/mastermind/workspace", nil)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	}
	if len(args) < 2 || args[0] != "pages" && args[0] != "tasks" {
		return errors.New("usage: relay workspace show|skill|template status|pages|tasks; pages/tasks: get|create|update|delete --help")
	}
	collection, action := args[0], args[1]
	f := flag.NewFlagSet("workspace "+collection+" "+action, flag.ContinueOnError)
	id := f.String("id", "", "Exact item ID (optional new ID for create)")
	revision := f.Uint64("revision", 0, "Required current workspace revision for a write")
	title := f.String("title", "", "Item title (required for create/update)")
	html := f.String("html-file", "", "UTF-8 HTML file (pages create/update, maximum 128 KiB)")
	detail := f.String("detail", "", "Task details or evidence")
	status := f.String("status", "todo", "Task status: todo, doing, blocked, done")
	page := f.String("page", "", "Related page ID")
	host := f.String("host", "", "Exact related machine ID")
	session := f.String("session", "", "Related session ID")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("Unexpected arguments; use the named options")
	}
	provided := map[string]bool{}
	f.Visit(func(option *flag.Flag) { provided[option.Name] = true })
	if action != "get" && action != "create" && action != "update" && action != "delete" {
		return errors.New("Use get, create, update or delete")
	}
	if action != "create" && !agentID(*id) || *id != "" && !agentID(*id) {
		return errors.New("An exact 32-character lowercase hexadecimal --id is required")
	}
	if action != "get" && !provided["revision"] {
		return errors.New("Read workspace show first and supply its --revision")
	}
	if action == "create" || action == "update" {
		if !provided["title"] || *title == "" {
			return errors.New("--title is required")
		}
	}
	if collection == "pages" && (provided["detail"] || provided["status"] || provided["page"] || provided["host"] || provided["session"]) {
		return errors.New("Task options cannot be used for a page")
	}
	if collection == "tasks" && provided["html-file"] {
		return errors.New("--html-file is only valid for pages")
	}
	method := map[string]string{"get": "GET", "create": "POST", "update": "PUT", "delete": "DELETE"}[action]
	path := "/agent/mastermind/workspace/" + collection
	if action != "create" {
		path += "/" + *id
	}
	var body map[string]any
	if action != "get" {
		body = map[string]any{"revision": *revision}
	}
	if action == "create" || action == "update" {
		body["title"] = *title
		if collection == "pages" {
			if *html == "" {
				return errors.New("--html-file is required")
			}
			file, err := openAgentRead(*html, false)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(file, (128<<10)+1))
			file.Close()
			if err != nil {
				return err
			}
			if len(data) > 128<<10 || !utf8.Valid(data) {
				return errors.New("HTML must be UTF-8 and at most 128 KiB")
			}
			body["html"] = string(data)
		} else {
			body["detail"], body["status"], body["pageId"], body["hostId"], body["sessionId"] = *detail, *status, *page, *host, *session
		}
	}
	if action == "create" {
		if *id == "" {
			var value [16]byte
			if _, err := rand.Read(value[:]); err != nil {
				return err
			}
			*id = hex.EncodeToString(value[:])
		}
		body["id"] = *id
	}
	data, err := agentRequest(method, path, body)
	if err != nil {
		if action == "create" {
			return fmt.Errorf("%w (requested item ID: %s; inspect this ID before repeating)", err, *id)
		}
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}
