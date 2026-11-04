package controller

import (
	"errors"
	"time"

	"github.com/TwoD97/relay/internal/workspacestatus"
)

const statusTemplate = "status-v1"

// Seed once in the controller startup transaction. User pages/tasks are copied
// unchanged; edit converts ownership, and the durable marker prevents deleted
// status pages from being recreated on a later launch.
func (s *Server) ensureStatusTemplate() error {
	if s.workspace == nil {
		return errors.New(workspaceUnavailable)
	}
	store := s.workspace
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.err != nil {
		return store.err
	}
	if store.record.StatusTemplateSeeded || len(store.record.Pages) >= maxWorkspacePages || store.record.Revision >= maxMastermindRevision {
		return nil
	}
	next := cloneWorkspace(store.record)
	next.StatusTemplateSeeded = true
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	id := randomID()[:32]
	for {
		found := false
		for _, page := range next.Pages {
			if page.ID == id {
				found = true
				break
			}
		}
		if !found {
			break
		}
		id = randomID()[:32]
	}
	next.Pages = append(next.Pages, WorkspacePage{WorkspacePageSummary: WorkspacePageSummary{ID: id, Title: "Status", UpdatedAt: next.UpdatedAt, Template: statusTemplate}, Revision: next.Revision})
	if err := store.saveLocked(next); err != nil {
		if store.err == nil {
			store.err = errors.New("The live Status page could not be initialized. Existing workspace content was preserved; check workspace storage and reopen Relay.")
		}
		return store.err
	}
	return nil
}

func (s *Server) statusMastermind() MastermindState {
	if s.mastermind == nil {
		return MastermindState{Status: "unconfigured"}
	}
	return s.mastermind.current().MastermindState
}

func statusUpdatedAt(record workspaceRecord, mastermind MastermindState, page WorkspacePageSummary) time.Time {
	updated := page.UpdatedAt
	if record.UpdatedAt.After(updated) {
		updated = record.UpdatedAt
	}
	if mastermind.UpdatedAt.After(updated) {
		updated = mastermind.UpdatedAt
	}
	return updated
}

func liveWorkspaceState(record workspaceRecord, mastermind MastermindState) WorkspaceState {
	state := workspaceState(record)
	for i, page := range state.Pages {
		if page.Template == statusTemplate {
			state.Pages[i].UpdatedAt = statusUpdatedAt(record, mastermind, page)
		}
	}
	return state
}

func renderStatusPage(record workspaceRecord, mastermind MastermindState, page WorkspacePage) (WorkspacePage, error) {
	if page.Template != statusTemplate {
		return page, nil
	}
	input := workspacestatus.Input{Live: true, Title: page.Title, CoordinatorStatus: mastermind.Status, CoordinatorEnabled: mastermind.Enabled, CoordinatorConfigured: mastermind.Config != nil, UpdatedAt: statusUpdatedAt(record, mastermind, page.WorkspacePageSummary)}
	for _, task := range record.Tasks {
		input.Tasks = append(input.Tasks, workspacestatus.Task{Title: task.Title, Detail: task.Detail, Status: task.Status, HostID: task.HostID, SessionID: task.SessionID})
	}
	html, err := workspacestatus.Render(input)
	if err != nil {
		return page, err
	}
	page.HTML = html
	page.UpdatedAt = input.UpdatedAt
	return page, nil
}
