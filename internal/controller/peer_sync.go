package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/TwoD97/relay/internal/peers"
)

type localGroupPane struct {
	Host      string    `json:"host"`
	Session   string    `json:"session"`
	CreatedAt time.Time `json:"createdAt"`
}
type localTerminalGroup struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	HostName  string           `json:"hostName"`
	Workspace string           `json:"workspace"`
	Collapsed bool             `json:"collapsed"`
	Panes     []localGroupPane `json:"panes"`
}
type peerPane struct {
	RuntimeID   string    `json:"runtimeId,omitempty"`
	Session     string    `json:"session"`
	CreatedAt   time.Time `json:"createdAt"`
	HostName    string    `json:"hostName"`
	Unavailable string    `json:"unavailable,omitempty"`
}
type peerGroup struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Workspace string     `json:"workspace"`
	Panes     []peerPane `json:"panes"`
}
type peerTask struct {
	WorkspaceTask
	RuntimeID        string     `json:"runtimeId,omitempty"`
	SessionCreatedAt *time.Time `json:"sessionCreatedAt,omitempty"`
}
type peerDocument struct {
	Format  int             `json:"format"`
	Groups  []peerGroup     `json:"groups"`
	Pages   []WorkspacePage `json:"pages"`
	Tasks   []peerTask      `json:"tasks"`
	Lessons []ControlLesson `json:"lessons"`
}
type peerImport struct {
	SourceID string `json:"sourceId"`
	Digest   string `json:"digest"`
	Status   string `json:"status"`
	Revision uint64 `json:"revision"`
}
type peerInput struct {
	Imports    map[string]peerImport `json:"imports,omitempty"`
	Format     int                   `json:"format"`
	Ready      bool                  `json:"ready"`
	Revision   uint64                `json:"revision"`
	Groups     []localTerminalGroup  `json:"groups"`
	Identities map[string]string     `json:"identities"`
	Paused     bool                  `json:"paused"`
}
type peerObservation struct {
	Host       Host
	Link       *link
	RuntimeID  string
	InstanceID string
	Sessions   map[string]time.Time
	ObservedAt time.Time
}
type peerCached struct {
	Record   peers.Record
	Hosts    []string
	Conflict string
}
type peerSync struct {
	mu        sync.Mutex
	cache     *peers.Store
	input     peerInput
	inputPath string
	records   map[string]peerCached
	observed  map[string]peerObservation
	warnings  []string
	err       string
	wake      chan struct{}
	done      chan struct{}
}

func (s *Server) initializePeerSync() {
	p := &peerSync{inputPath: filepath.Join(s.config.StateDir, "peer-groups.json"), input: peerInput{Format: 1, Groups: []localTerminalGroup{}, Identities: map[string]string{}}, records: map[string]peerCached{}, observed: map[string]peerObservation{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.peerSync = p
	var err error
	p.cache, err = peers.Open(filepath.Join(s.config.StateDir, "peer-workspaces"))
	if err != nil {
		p.err = "Peer workspace cache is unavailable"
		return
	}
	data, err := readWorkspaceFile(p.inputPath, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || strictWorkspaceJSON(data, &p.input) != nil || p.input.Format != 1 || !validLocalGroups(p.input.Groups) || len(p.input.Identities) > 128 || len(p.input.Imports) > 128 {
		p.err = "Saved group publication state is invalid and was preserved"
		return
	}
	if p.input.Identities == nil {
		p.input.Identities = map[string]string{}
	}
	for id, intent := range p.input.Imports {
		if !validMastermindID(id) || !peers.ValidSource(intent.SourceID) || len(intent.Digest) != 64 || (intent.Status != "accepted" && intent.Status != "completed") {
			p.err = "Saved peer import journal is invalid"
			return
		}
	}
	for _, id := range p.input.Identities {
		if !validMaintenanceSessionID(id) {
			p.err = "Saved runtime identity is invalid"
			return
		}
	}
}
func textBound(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
func groupID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func validLocalGroups(groups []localTerminalGroup) bool {
	if len(groups) > 32 {
		return false
	}
	ids := map[string]bool{}
	for _, g := range groups {
		if !groupID(g.ID) || ids[g.ID] || !textBound(g.Title, 1024) || !textBound(g.HostName, 1024) || !textBound(g.Workspace, 1024) || len(g.Panes) < 1 || len(g.Panes) > 4 {
			return false
		}
		ids[g.ID] = true
		seen := map[string]bool{}
		for _, p := range g.Panes {
			key := p.Host + "\x00" + p.Session
			if !textBound(p.Host, 256) || !textBound(p.Session, 256) || p.CreatedAt.IsZero() || seen[key] {
				return false
			}
			seen[key] = true
		}
	}
	return true
}
func peerPaneKey(p localGroupPane) string {
	return p.Host + "/" + p.Session + "/" + p.CreatedAt.UTC().Format(time.RFC3339Nano)
}
func (p *peerSync) saveInputLocked() error {
	data, err := json.Marshal(p.input)
	if err != nil {
		return err
	}
	if len(data) > 256<<10 {
		return errors.New("Saved terminal group publication exceeds its limit")
	}
	if err = writeRestartFile(p.inputPath, data); err != nil {
		p.err = "Saving peer publication state was not confirmed; reopen Relay before publishing another snapshot"
	}
	return err
}
func (s *Server) publishPeerGroups(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Revision uint64               `json:"revision"`
		Groups   []localTerminalGroup `json:"groups"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil || strictWorkspaceJSON(data, &value) != nil || !validLocalGroups(value.Groups) || value.Revision >= 1<<53 {
		writeError(w, 400, "Invalid confirmed terminal group snapshot")
		return
	}
	p := s.peerSync
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != "" {
		writeError(w, 503, p.err)
		return
	}
	old, _ := json.Marshal(p.input.Groups)
	next, _ := json.Marshal(value.Groups)
	if p.input.Ready && (value.Revision < p.input.Revision || (value.Revision == p.input.Revision && !bytes.Equal(old, next))) {
		writeError(w, 409, "Saved groups changed in another desktop state; no snapshot was overwritten")
		return
	}
	if p.input.Ready && value.Revision == p.input.Revision {
		writeJSON(w, 200, map[string]uint64{"revision": value.Revision})
		return
	}
	p.input.Ready = true
	p.input.Revision = value.Revision
	p.input.Groups = value.Groups
	retained := map[string]string{}
	for _, g := range value.Groups {
		for _, pane := range g.Panes {
			key := peerPaneKey(pane)
			if id := p.input.Identities[key]; id != "" {
				retained[key] = id
			}
		}
	}
	p.input.Identities = retained
	if err = p.saveInputLocked(); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
	writeJSON(w, 200, map[string]uint64{"revision": value.Revision})
}
func peerRequest(ctx context.Context, client *http.Client, method, path string, body any, limit int64) ([]byte, error) {
	var data []byte
	var err error
	if body != nil {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		err = encoder.Encode(body)
		data = encoded.Bytes()
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://runtime"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	copy := *client
	copy.Jar = nil
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := copy.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(result)) > limit {
		return nil, errors.New("peer response exceeds its bound")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("peer operation returned HTTP %d", response.StatusCode)
	}
	return result, nil
}
func (s *Server) peerSourceID() string {
	s.fleet.mu.Lock()
	defer s.fleet.mu.Unlock()
	return s.fleet.identity.ID
}
func (p *peerSync) note(message string) {
	p.mu.Lock()
	if len(p.warnings) < 64 {
		p.warnings = append(p.warnings, message)
	}
	p.mu.Unlock()
}
func (s *Server) peerLoop() {
	defer close(s.peerSync.done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		s.refreshPeers()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.peerSync.wake:
		}
	}
}
func (s *Server) refreshPeers() {
	p := s.peerSync
	p.mu.Lock()
	if p.err != "" || p.cache == nil {
		p.mu.Unlock()
		return
	}
	p.warnings = nil
	p.records = map[string]peerCached{}
	p.observed = map[string]peerObservation{}
	p.mu.Unlock()
	index, err := p.cache.List()
	if err != nil {
		p.note("Saved peer cache could not be read")
		return
	}
	for _, summary := range index.Sources {
		if record, err := p.cache.Get(summary.SourceID); err == nil {
			p.mu.Lock()
			p.records[record.Digest] = peerCached{Record: record}
			p.mu.Unlock()
		}
	}
	hosts := s.store.list()
	if s.config.LocalSocket != "" {
		hosts = append(hosts, Host{ID: "local", Name: "This computer", Target: s.config.LocalSocket, Status: "online"})
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, 4)
	for _, host := range hosts {
		if host.Status != "online" {
			continue
		}
		s.mu.Lock()
		l := s.links[host.ID]
		s.mu.Unlock()
		if l == nil || l.httpTransport == nil {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-s.ctx.Done():
			return
		}
		wg.Add(1)
		go func(host Host, l *link) { defer wg.Done(); defer func() { <-slots }(); s.inspectPeer(host, l) }(host, l)
	}
	wg.Wait()
	if s.ctx.Err() != nil {
		return
	}
	s.publishPeerSnapshot()
}
func (s *Server) inspectPeer(host Host, l *link) {
	p := s.peerSync
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	defer cancel()
	client := &http.Client{Transport: l.httpTransport}
	data, err := peerRequest(ctx, client, "GET", "/api/health", nil, 64<<10)
	var health struct {
		RuntimeID    string          `json:"runtimeId"`
		InstanceID   string          `json:"instanceId"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err != nil || json.Unmarshal(data, &health) != nil || !validMaintenanceSessionID(health.RuntimeID) || !validMaintenanceSessionID(health.InstanceID) || !health.Capabilities["workspaceSyncV1"] {
		return
	}
	data, err = peerRequest(ctx, client, "GET", "/api/sessions", nil, 512<<10)
	var sessions []struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err != nil || json.Unmarshal(data, &sessions) != nil || len(sessions) > 32 {
		return
	}
	observation := peerObservation{ObservedAt: time.Now(), Host: host, Link: l, RuntimeID: health.RuntimeID, InstanceID: health.InstanceID, Sessions: map[string]time.Time{}}
	for _, session := range sessions {
		observation.Sessions[session.ID] = session.CreatedAt
	}
	s.mu.Lock()
	stillLinked := s.links[host.ID] == l
	s.mu.Unlock()
	if !stillLinked {
		return
	}
	p.mu.Lock()
	p.observed[host.ID] = observation
	p.mu.Unlock()
	data, err = peerRequest(ctx, client, "GET", "/api/peers", nil, 64<<10)
	var index peers.Index
	if err != nil || json.Unmarshal(data, &index) != nil || len(index.Sources) > peers.MaxSources {
		p.note(host.Name + ": saved workspace snapshots could not be read")
		return
	}
	for _, issue := range index.Errors {
		p.note(host.Name + ": " + issue.Error)
	}
	for _, summary := range index.Sources {
		if !peers.ValidSource(summary.SourceID) {
			continue
		}
		p.mu.Lock()
		known, exists := p.records[summary.Digest]
		p.mu.Unlock()
		if !exists {
			data, err = peerRequest(ctx, client, "GET", "/api/peers/"+summary.SourceID, nil, peers.MaxBytes+1024)
			if err != nil {
				p.note(host.Name + ": a saved workspace could not be read")
				continue
			}
			var record peers.Record
			if json.Unmarshal(data, &record) != nil || record.SourceID != summary.SourceID || record.Digest != summary.Digest || peers.ValidateRecord(record) != nil {
				p.note(host.Name + ": a snapshot digest could not be verified")
				continue
			}
			var doc peerDocument
			if decodePeerDocument(record.Data, &doc) != nil {
				p.note(host.Name + ": an unsupported workspace snapshot was preserved without importing it")
				continue
			}
			known = peerCached{Record: record}
			// The common store recalculates the digest; a remote claim alone is never trusted.
			p.mu.Lock()
			cached, readErr := p.cache.Get(record.SourceID)
			expected := ""
			if readErr == nil {
				expected = cached.Digest
			}
			if readErr == nil && cached.Revision == record.Revision && cached.Digest != record.Digest {
				known.Conflict = "Two machines hold different snapshots at the same source revision. Restore only the copy you intend."
			} else if errors.Is(readErr, peers.ErrNotFound) || (readErr == nil && cached.Revision < record.Revision) {
				stored, putErr := p.cache.Put(expected, record.Snapshot)
				if putErr == nil && stored.Digest == record.Digest {
					known.Record = stored
				} else if putErr != nil {
					p.warnings = append(p.warnings, "A peer snapshot could not be cached: "+putErr.Error())
				}
			}
			if record.SourceID == s.peerSourceID() && (errors.Is(readErr, peers.ErrNotFound) || (readErr == nil && (record.Revision > cached.Revision || record.Revision == cached.Revision && record.Digest != cached.Digest))) {
				p.input.Paused = true
				_ = p.saveInputLocked()
				known.Conflict = "A newer or different backup of this desktop was found. Restore it explicitly before publishing local changes."
			}
			if len(p.records) < 16 || p.records[record.Digest].Record.Digest != "" {
				p.records[record.Digest] = known
			}
			p.mu.Unlock()
		}
		p.mu.Lock()
		known = p.records[summary.Digest]
		if known.Record.Digest != "" {
			known.Hosts = append(known.Hosts, host.ID)
			p.records[summary.Digest] = known
		}
		p.mu.Unlock()
	}
}
func decodePeerDocument(data []byte, doc *peerDocument) error {
	if len(data) > peers.MaxBytes || strictWorkspaceJSON(data, doc) != nil || doc.Format != 1 || len(doc.Groups) > 32 || len(doc.Pages) > 16 || len(doc.Tasks) > 200 || len(doc.Lessons) > 100 {
		return errors.New("unsupported peer workspace")
	}
	record := workspaceRecord{Format: 1, Revision: maxMastermindRevision, UpdatedAt: time.Now().UTC(), Pages: doc.Pages, Tasks: []WorkspaceTask{}, StatusTemplateSeeded: true}
	for _, task := range doc.Tasks {
		if task.HostID != "" || (task.RuntimeID != "" && !validMaintenanceSessionID(task.RuntimeID)) || (task.SessionID != "" && (task.RuntimeID == "" || task.SessionCreatedAt == nil || task.SessionCreatedAt.IsZero())) {
			return errors.New("invalid peer task identity")
		}
		item := task.WorkspaceTask
		if item.SessionID != "" {
			item.HostID = "runtime"
		}
		record.Tasks = append(record.Tasks, item)
	}
	if err := validateWorkspaceRecord(record); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, g := range doc.Groups {
		if !groupID(g.ID) || seen[g.ID] || !textBound(g.Title, 1024) || !textBound(g.Workspace, 1024) || len(g.Panes) < 1 || len(g.Panes) > 4 {
			return errors.New("invalid peer terminal group")
		}
		seen[g.ID] = true
		panes := map[string]bool{}
		for _, pane := range g.Panes {
			key := pane.RuntimeID + "/" + pane.Session + "/" + pane.CreatedAt.UTC().Format(time.RFC3339Nano)
			if !validMaintenanceSessionID(pane.Session) || pane.CreatedAt.IsZero() || !textBound(pane.HostName, 1024) || len(pane.Unavailable) > 256 || panes[key] || (pane.RuntimeID != "" && !validMaintenanceSessionID(pane.RuntimeID)) {
				return errors.New("invalid peer terminal identity")
			}
			panes[key] = true
		}
	}
	return validateControlRecord(controlRecord{Format: 1, Lessons: doc.Lessons})
}
func (s *Server) publishPeerSnapshot() {
	p := s.peerSync
	p.mu.Lock()
	if p.err != "" || p.input.Paused {
		p.mu.Unlock()
		return
	}
	input := p.input
	input.Groups = append([]localTerminalGroup{}, input.Groups...)
	observed := make(map[string]peerObservation, len(p.observed))
	for id, o := range p.observed {
		observed[id] = o
	}
	p.mu.Unlock()
	source := s.peerSourceID()
	old, oldErr := p.cache.Get(source)
	if oldErr != nil && !errors.Is(oldErr, peers.ErrNotFound) {
		p.note("Local workspace publication is blocked by an unreadable saved source")
		return
	}
	doc := peerDocument{Format: 1, Groups: []peerGroup{}, Pages: []WorkspacePage{}, Tasks: []peerTask{}, Lessons: []ControlLesson{}}
	if !input.Ready && oldErr == nil {
		var previous peerDocument
		if decodePeerDocument(old.Data, &previous) == nil {
			doc.Groups = previous.Groups
		}
	}
	if input.Ready {
		for _, g := range input.Groups {
			group := peerGroup{ID: g.ID, Title: g.Title, Workspace: g.Workspace, Panes: []peerPane{}}
			for _, pane := range g.Panes {
				key := peerPaneKey(pane)
				p.mu.Lock()
				runtimeID := p.input.Identities[key]
				if runtimeID == "" {
					if o, ok := observed[pane.Host]; ok && o.Sessions[pane.Session].Equal(pane.CreatedAt) {
						runtimeID = o.RuntimeID
						p.input.Identities[key] = runtimeID
						_ = p.saveInputLocked()
					}
				}
				p.mu.Unlock()
				member := peerPane{RuntimeID: runtimeID, Session: pane.Session, CreatedAt: pane.CreatedAt, HostName: g.HostName}
				if runtimeID == "" {
					member.Unavailable = "Connect the original compatible runtime before sharing this pane"
				}
				group.Panes = append(group.Panes, member)
			}
			doc.Groups = append(doc.Groups, group)
		}
	}
	s.workspace.mu.Lock()
	if s.workspace.err != nil {
		s.workspace.mu.Unlock()
		p.note("Workspace state is unavailable; its backup was not overwritten")
		return
	}
	doc.Pages = append(doc.Pages, s.workspace.record.Pages...)
	for _, task := range s.workspace.record.Tasks {
		copy := peerTask{WorkspaceTask: task}
		copy.HostID = ""
		if o, ok := observed[task.HostID]; ok {
			copy.RuntimeID = o.RuntimeID
			if created, ok := o.Sessions[task.SessionID]; ok {
				copy.SessionCreatedAt = &created
			} else {
				copy.SessionID = ""
			}
		} else {
			copy.SessionID = ""
		}
		doc.Tasks = append(doc.Tasks, copy)
	}
	s.workspace.mu.Unlock()
	s.control.mu.Lock()
	if s.control.err != nil {
		s.control.mu.Unlock()
		p.note("Lessons are unavailable; the backup was not overwritten")
		return
	}
	doc.Lessons = append(doc.Lessons, s.control.record.Lessons...)
	s.control.mu.Unlock()
	p.mu.Lock()
	blocked := p.err != "" || p.input.Paused
	p.mu.Unlock()
	if blocked {
		return
	}
	if err := decodePeerDocumentMust(doc); err != nil {
		p.note("Workspace snapshot failed validation; no remote metadata was overwritten")
		return
	}
	raw, _ := json.Marshal(doc)
	var canonical any
	json.Unmarshal(raw, &canonical)
	data, _ := json.Marshal(canonical)
	snapshot := old.Snapshot
	expected := old.Digest
	if oldErr != nil || !bytes.Equal(data, old.Data) {
		name, _ := os.Hostname()
		if !textBound(name, 80) {
			name = "Relay desktop"
		}
		snapshot = peers.Snapshot{SourceID: source, Name: name, Revision: old.Revision + 1, UpdatedAt: time.Now().UTC(), Data: data}
		stored, err := p.cache.Put(expected, snapshot)
		if err != nil {
			p.note("Local workspace snapshot could not be saved: " + err.Error())
			return
		}
		old = stored
	}
	p.mu.Lock()
	for digest, cached := range p.records {
		if cached.Record.SourceID == source {
			delete(p.records, digest)
		}
	}
	p.records[old.Digest] = peerCached{Record: old}
	p.mu.Unlock()
	for _, o := range observed {
		ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
		client := &http.Client{Transport: o.Link.httpTransport}
		indexData, err := peerRequest(ctx, client, "GET", "/api/peers", nil, 64<<10)
		var index peers.Index
		if err != nil || json.Unmarshal(indexData, &index) != nil {
			cancel()
			continue
		}
		remoteDigest := ""
		var revision uint64
		for _, summary := range index.Sources {
			if summary.SourceID == source {
				remoteDigest = summary.Digest
				revision = summary.Revision
			}
		}
		if remoteDigest == old.Digest {
			cancel()
			continue
		}
		if revision >= old.Revision {
			p.note(o.Host.Name + ": a different or newer copy exists; it was not overwritten")
			cancel()
			continue
		}
		_, err = peerRequest(ctx, client, "PUT", "/api/peers/"+source, map[string]any{"expectedDigest": remoteDigest, "snapshot": old.Snapshot}, peers.MaxBytes+1024)
		cancel()
		if err != nil {
			p.note(o.Host.Name + ": publication was not confirmed; the next check will read the saved digest before any new metadata write")
		}
	}
}
func (s *Server) getPeerSync(w http.ResponseWriter, r *http.Request) {
	p := s.peerSync
	p.mu.Lock()
	defer p.mu.Unlock()
	sources := []map[string]any{}
	own := s.peerSourceID()
	warnings := append([]string{}, p.warnings...)
	if p.err != "" {
		warnings = append(warnings, p.err)
	}
	for _, cached := range p.records {
		if cached.Record.SourceID == own && !p.input.Paused {
			continue
		}
		var doc peerDocument
		if decodePeerDocument(cached.Record.Data, &doc) != nil {
			continue
		}
		sources = append(sources, map[string]any{"sourceId": cached.Record.SourceID, "name": cached.Record.Name, "revision": cached.Record.Revision, "digest": cached.Record.Digest, "updatedAt": cached.Record.UpdatedAt, "groups": len(doc.Groups), "pages": len(doc.Pages), "tasks": len(doc.Tasks), "lessons": len(doc.Lessons), "hosts": cached.Hosts, "conflict": cached.Conflict})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i]["sourceId"].(string) < sources[j]["sourceId"].(string) })
	s.workspace.mu.Lock()
	revision := s.workspace.record.Revision
	s.workspace.mu.Unlock()
	writeJSON(w, 200, map[string]any{"sources": sources, "warnings": warnings, "workspaceRevision": revision})
}
func restoredID(importID, kind, id string) string {
	digest := sha256.Sum256([]byte(importID + "/" + kind + "/" + id))
	return hex.EncodeToString(digest[:16])
}

func decodePeerDocumentMust(doc peerDocument) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	var checked peerDocument
	return decodePeerDocument(data, &checked)
}

// A cloned runtime identity is ambiguous. Two aliases for the same current
// instance are equivalent; choose one existing controller host deterministically.
func (s *Server) resolvePeerRuntimeLocked(runtimeID string) (peerObservation, bool) {
	var chosen peerObservation
	for _, o := range s.peerSync.observed {
		if o.RuntimeID != runtimeID || !s.currentPeerObservation(o) {
			continue
		}
		if chosen.RuntimeID != "" && chosen.InstanceID != o.InstanceID {
			return peerObservation{}, false
		}
		if chosen.RuntimeID == "" || o.Host.ID < chosen.Host.ID {
			chosen = o
		}
	}
	return chosen, chosen.RuntimeID != ""
}
func restoredGroupID(id, source string) string {
	raw := restoredID(id, "group", source)
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
}
func (s *Server) importPeerSync(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID       string `json:"id"`
		SourceID string `json:"sourceId"`
		Digest   string `json:"digest"`
		Revision uint64 `json:"revision"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || strictWorkspaceJSON(data, &request) != nil || !validMastermindID(request.ID) || !peers.ValidSource(request.SourceID) || len(request.Digest) != 64 {
		writeError(w, 400, "Invalid exact snapshot import identity")
		return
	}
	p := s.peerSync
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != "" {
		writeError(w, 503, p.err)
		return
	}
	if _, exists := p.input.Imports[request.ID]; exists {
		writeError(w, 409, "This import was already dispatched. Inspect the current workspace; it will not be repeated.")
		return
	}
	if len(p.input.Imports) >= 128 {
		writeError(w, 409, "The import journal reached its 128-operation limit. Preserve peer-groups.json before explicit maintenance.")
		return
	}
	cached, ok := p.records[request.Digest]
	if !ok || cached.Record.SourceID != request.SourceID || peers.ValidateRecord(cached.Record) != nil {
		writeError(w, 409, "The selected snapshot is no longer available. Refresh and review it again.")
		return
	}
	var doc peerDocument
	if decodePeerDocument(cached.Record.Data, &doc) != nil {
		writeError(w, 422, "Unsupported workspace snapshot; nothing was imported")
		return
	}
	groups := []localTerminalGroup{}
	warnings := []string{}
	for _, source := range doc.Groups {
		group := localTerminalGroup{ID: restoredGroupID(request.ID, source.ID), Title: source.Title, Workspace: source.Workspace, Panes: []localGroupPane{}}
		valid := true
		for _, pane := range source.Panes {
			o, found := s.resolvePeerRuntimeLocked(pane.RuntimeID)
			if !found {
				valid = false
				break
			}
			if current, exists := o.Sessions[pane.Session]; exists && !current.Equal(pane.CreatedAt) {
				valid = false
				break
			}
			if group.HostName == "" {
				group.HostName = o.Host.Name
			}
			group.Panes = append(group.Panes, localGroupPane{Host: o.Host.ID, Session: pane.Session, CreatedAt: pane.CreatedAt})
		}
		if valid {
			groups = append(groups, group)
		} else {
			warnings = append(warnings, "Terminal group "+source.Title+" was not imported: its original runtime or exact session identity could not be verified.")
		}
	}
	s.workspace.mu.Lock()
	if s.workspace.record.FrozenBy != "" {
		s.workspace.mu.Unlock()
		writeError(w, 409, "This workspace is hosted elsewhere. Import from its authoritative controller; the archived local workspace was not changed.")
		return
	}
	if s.workspace.err != nil {
		s.workspace.mu.Unlock()
		writeError(w, 503, workspaceUnavailable)
		return
	}
	if s.workspace.record.Revision != request.Revision {
		s.workspace.mu.Unlock()
		writeError(w, 409, "Workspace changed. Refresh and review before importing.")
		return
	}
	if len(s.workspace.record.Pages)+len(doc.Pages) > maxWorkspacePages || len(s.workspace.record.Tasks)+len(doc.Tasks) > maxWorkspaceTasks {
		s.workspace.mu.Unlock()
		writeError(w, 409, "The separate copy would exceed the workspace page or task limit")
		return
	}
	record := cloneWorkspace(s.workspace.record)
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	for _, page := range doc.Pages {
		page.ID = restoredID(request.ID, "page", page.ID)
		page.Template = ""
		page.Revision = record.Revision
		page.UpdatedAt = record.UpdatedAt
		record.Pages = append(record.Pages, page)
	}
	for _, source := range doc.Tasks {
		task := source.WorkspaceTask
		task.ID = restoredID(request.ID, "task", task.ID)
		task.UpdatedAt = record.UpdatedAt
		if task.PageID != "" {
			task.PageID = restoredID(request.ID, "page", task.PageID)
		}
		task.HostID = ""
		task.SessionID = ""
		if source.RuntimeID != "" {
			if o, found := s.resolvePeerRuntimeLocked(source.RuntimeID); found {
				task.HostID = o.Host.ID
				if source.SessionID != "" && source.SessionCreatedAt != nil && o.Sessions[source.SessionID].Equal(*source.SessionCreatedAt) {
					task.SessionID = source.SessionID
				} else if source.SessionID != "" {
					warnings = append(warnings, "Task "+task.Title+" was copied without its unverified terminal link.")
				}
			} else {
				warnings = append(warnings, "Task "+task.Title+" was copied without its unavailable machine link.")
			}
		}
		record.Tasks = append(record.Tasks, task)
	}
	if err = validateWorkspaceRecord(record); err != nil {
		s.workspace.mu.Unlock()
		writeError(w, 422, "Imported content failed validation; no changes were saved")
		return
	}
	encoded, _ := json.Marshal(record)
	if len(encoded) > maxWorkspaceBytes {
		s.workspace.mu.Unlock()
		writeError(w, 409, "The separate copy would exceed the workspace storage limit")
		return
	}
	if p.input.Imports == nil {
		p.input.Imports = map[string]peerImport{}
	}
	p.input.Imports[request.ID] = peerImport{SourceID: request.SourceID, Digest: request.Digest, Status: "accepted", Revision: request.Revision}
	if err = p.saveInputLocked(); err != nil {
		s.workspace.mu.Unlock()
		writeError(w, 500, "Import intent could not be saved. No workspace changes were dispatched.")
		return
	}
	err = s.workspace.saveLocked(record)
	state := workspaceState(s.workspace.record)
	s.workspace.mu.Unlock()
	if err != nil {
		writeError(w, 500, "Workspace copy was not confirmed. Inspect Pages and Tasks before any new import; this request will not be repeated.")
		return
	}
	if len(doc.Lessons) > 0 {
		err = s.control.update(func(control *controlRecord) error {
			if len(control.Lessons)+len(doc.Lessons) > 100 {
				return errors.New("lesson limit reached")
			}
			for _, lesson := range doc.Lessons {
				lesson.ID = restoredID(request.ID, "lesson", lesson.ID)
				control.Lessons = append(control.Lessons, lesson)
			}
			return nil
		})
		if err != nil {
			warnings = append(warnings, "Workspace content was copied, but lesson storage was not confirmed. Review Lessons before making another copy.")
		}
	}
	intent := p.input.Imports[request.ID]
	intent.Status = "completed"
	p.input.Imports[request.ID] = intent
	if err = p.saveInputLocked(); err != nil {
		writeError(w, 500, "Workspace copy was saved, but its completion receipt was not confirmed. Inspect the current workspace; this request will not be repeated.")
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
	writeJSON(w, 201, map[string]any{"workspace": state, "groups": groups, "warnings": warnings})
}

func (s *Server) currentPeerObservation(o peerObservation) bool {
	if o.ObservedAt.IsZero() || time.Since(o.ObservedAt) > 45*time.Second {
		return false
	}
	s.mu.Lock()
	current := s.links[o.Host.ID] == o.Link && o.Link != nil
	s.mu.Unlock()
	if !current {
		return false
	}
	if o.Host.ID == "local" {
		return s.config.LocalSocket != ""
	}
	host, found := s.store.get(o.Host.ID)
	return found && host.Status == "online" && host.CreatedAt.Equal(o.Host.CreatedAt) && host.Target == o.Host.Target && host.Port == o.Host.Port
}
