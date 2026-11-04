// Package workspacestatus renders a script-free status document from recorded
// workspace facts. It does not inspect terminals or infer agent progress.
package workspacestatus

import (
	"bytes"
	_ "embed"
	"errors"
	"html/template"
	"time"
)

//go:embed status.html
var source string
var document = template.Must(template.New("status").Parse(source))

type Task struct{ Title, Detail, Status, HostID, SessionID string }
type Input struct {
	Live                                      bool
	Title                                     string
	CoordinatorStatus                         string
	CoordinatorEnabled, CoordinatorConfigured bool
	UpdatedAt                                 time.Time
	Tasks                                     []Task
}
type group struct {
	Key, Title, Empty string
	Count             int
	Tasks             []Task
}
type view struct {
	Input
	Coordinator, Timestamp string
	Total, Visible         int
	Groups                 []group
}

func clip(value string, limit int) string {
	r := []rune(value)
	if len(r) <= limit {
		return value
	}
	return string(r[:limit]) + "…"
}

func Render(input Input) (string, error) {
	if len(input.Tasks) > 200 {
		return "", errors.New("status page exceeds task limit")
	}
	if input.Title == "" {
		input.Title = "Project status"
	}
	input.Title = clip(input.Title, 160)
	v := view{Input: input, Total: len(input.Tasks), Coordinator: "Not configured", Groups: []group{{Key: "doing", Title: "Current work", Empty: "No work is marked in progress."}, {Key: "blocked", Title: "Needs attention", Empty: "No blockers are recorded."}, {Key: "todo", Title: "Next steps", Empty: "No next steps are recorded yet."}, {Key: "done", Title: "Completed", Empty: "No completed tasks are recorded yet."}}}
	if input.CoordinatorConfigured {
		if !input.CoordinatorEnabled {
			v.Coordinator = "Paused"
		} else {
			v.Coordinator = map[string]string{"unconfigured": "Not configured", "waiting": "Waiting for connection", "starting": "Starting", "running": "Process running", "exited": "Process exited", "interrupted": "Process interrupted", "missing": "Session unavailable", "blocked": "Setup needs attention", "uncertain": "Session state unconfirmed", "disabled": "Paused"}[input.CoordinatorStatus]
			if v.Coordinator == "" {
				v.Coordinator = "Status unavailable"
			}
		}
	}
	if !input.UpdatedAt.IsZero() {
		v.Timestamp = input.UpdatedAt.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	for index := range v.Groups {
		for _, task := range input.Tasks {
			if task.Status != v.Groups[index].Key {
				continue
			}
			v.Groups[index].Count++
			if v.Visible >= 40 {
				continue
			}
			task.Title = clip(task.Title, 120)
			task.Detail = clip(task.Detail, 180)
			task.HostID = clip(task.HostID, 64)
			task.SessionID = clip(task.SessionID, 32)
			v.Groups[index].Tasks = append(v.Groups[index].Tasks, task)
			v.Visible++
		}
	}
	var out bytes.Buffer
	if err := document.Execute(&out, v); err != nil {
		return "", err
	}
	if out.Len() > 128<<10 {
		return "", errors.New("status page exceeds HTML limit")
	}
	return out.String(), nil
}

func Starter() (string, error) { return Render(Input{Title: "Project status"}) }
