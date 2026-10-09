//go:build linux

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

type ObserverConfig struct {
	Enabled         bool   `json:"enabled"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	IntervalSeconds int    `json:"intervalSeconds"`
}

type ObserverContent struct {
	Summary   string   `json:"summary"`
	Steps     []string `json:"steps"`
	NextSteps []string `json:"nextSteps"`
	Blockers  []string `json:"blockers"`
}

type SessionSummary struct {
	ObserverContent
	SessionID        string     `json:"sessionId"`
	SessionCreatedAt time.Time  `json:"sessionCreatedAt"`
	Provider         string     `json:"provider"`
	Model            string     `json:"model"`
	Status           string     `json:"status"`
	SampledAt        *time.Time `json:"sampledAt,omitempty"`
	GeneratedAt      *time.Time `json:"generatedAt,omitempty"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	SourceStatus     string     `json:"sourceStatus"`
	Stale            bool       `json:"stale"`
	Error            string     `json:"error,omitempty"`
}

type observerRecord struct {
	SessionSummary
	Digest      string    `json:"digest"`
	LastAttempt time.Time `json:"lastAttempt"`
}

type observerStore struct {
	Version  int                       `json:"version"`
	Config   ObserverConfig            `json:"config"`
	Records  map[string]observerRecord `json:"records"`
	Attempts []time.Time               `json:"attempts"`
}

type observerManager struct {
	mu       sync.Mutex
	s        *Server
	state    observerStore
	err      string
	active   string
	cancel   context.CancelFunc
	wake     chan struct{}
	done     chan struct{}
	run      func(context.Context, ObserverConfig, []byte) (ObserverContent, error)
	validate func(context.Context, ObserverConfig) error
}

const observerHourlyLimit = 60
const observerStoreLimit = 4 << 20

func defaultObserverConfig() ObserverConfig {
	return ObserverConfig{Provider: "claude", Model: "haiku", IntervalSeconds: 300}
}

func validObserverConfig(cfg ObserverConfig) bool {
	return cfg.Provider == "claude" && cfg.Model != "" && len(cfg.Model) <= 200 &&
		strings.TrimSpace(cfg.Model) == cfg.Model && !strings.HasPrefix(cfg.Model, "-") &&
		utf8.ValidString(cfg.Model) && strings.IndexFunc(cfg.Model, unicode.IsControl) < 0 &&
		cfg.IntervalSeconds >= 60 && cfg.IntervalSeconds <= 3600
}

func newObserver(s *Server) *observerManager {
	o := &observerManager{s: s, state: observerStore{Version: 1, Config: defaultObserverConfig(), Records: make(map[string]observerRecord)}, wake: make(chan struct{}, 1), done: make(chan struct{}), run: s.runObserver, validate: s.validateObserverProvider}
	data, err := readPrivateFile(o.path(), observerStoreLimit)
	if errors.Is(err, os.ErrNotExist) {
		return o
	}
	var saved observerStore
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Version != 1 || !validObserverConfig(saved.Config) || len(saved.Records) > maxSessions || len(saved.Attempts) > observerHourlyLimit {
		o.err = "Summary state could not be loaded. Summaries are disabled; save settings to start again."
		return o
	}
	if saved.Records == nil {
		saved.Records = make(map[string]observerRecord)
	}
	for id, rec := range saved.Records {
		if !validID(id) || id != rec.SessionID || rec.SessionCreatedAt.IsZero() || (rec.Status != "queued" && rec.Status != "running" && rec.Status != "ready" && rec.Status != "error") {
			o.err = "Summary state could not be loaded. Summaries are disabled; save settings to start again."
			return o
		}
		if rec.Status == "queued" || rec.Status == "running" {
			rec.Status, rec.Error, rec.Stale = "error", "Summary was interrupted. Refresh explicitly to try again.", true
			saved.Records[id] = rec
		}
	}
	o.state = saved
	return o
}

func (o *observerManager) path() string { return filepath.Join(o.s.stateDir, "observer.json") }

func (o *observerManager) saveLocked() error {
	data, err := json.Marshal(o.state)
	if err == nil && len(data) > observerStoreLimit {
		err = errors.New("summary state size limit exceeded")
	}
	if err == nil {
		err = atomicPrivateWrite(o.path(), data)
	}
	if err != nil {
		return errors.New("Cannot save summary state; no further model requests will start until settings are saved")
	}
	return nil
}

func (o *observerManager) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *observerManager) loop(ctx context.Context) {
	defer close(o.done)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if o.runNext(ctx, time.Now().UTC()) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-tick.C:
		}
	}
}

type observerSource struct {
	Session     Session `json:"session"`
	Text        string  `json:"terminalTail"`
	digest      string
	unavailable string
}

// Capture only normal agent sessions. Provider login and maintenance terminals
// never become observation inputs; no terminal transcript is stored here.
func (s *Server) observerSources() map[string]observerSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	sources := make(map[string]observerSource)
	for id, p := range s.sessions {
		p.mu.Lock()
		meta := p.meta
		if meta.Purpose != "" || (meta.Harness != "claude" && meta.Harness != "codex") {
			p.mu.Unlock()
			continue
		}
		var text, unavailable string
		if p.screen != nil {
			// The emulator tracks parser state across PTY chunks and exposes
			// visible cells only, never OSC clipboard/title or DCS payloads.
			text = boundedObserverText(p.screen.emulator.String())
		} else if p.history.size < maxHistory {
			// A complete historical stream starts in a known parser state.
			// Never strip a tail that may begin inside an invisible string.
			text = observerPlainText(p.history.bytes())
		} else {
			unavailable = "This recovered session has truncated history and no trustworthy screen snapshot. Its terminal remains available, but it cannot be summarized safely."
		}
		p.mu.Unlock()
		source := observerSource{Session: meta, Text: text, unavailable: unavailable}
		// Process identity and metadata timestamps do not represent new evidence.
		fingerprint, _ := json.Marshal(struct {
			ID                  string
			Created             time.Time
			Status, Title, Text string
			Exit                *int
		}{id, meta.CreatedAt, meta.Status, meta.Title, text, meta.ExitCode})
		sum := sha256.Sum256(fingerprint)
		source.digest = hex.EncodeToString(sum[:])
		sources[id] = source
	}
	return sources
}

// This must receive a complete stream, never an arbitrary byte-ring tail.
func observerPlainText(data []byte) string {
	return boundedObserverText(ansi.Strip(string(data)))
}

func boundedObserverText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, ""))
	if len(text) > 10<<10 {
		text = strings.ToValidUTF8(text[len(text)-(10<<10):], "")
	}
	return strings.TrimSpace(text)
}

func observerPrompt(source observerSource, previous string) ([]byte, error) {
	if len(previous) > 2000 {
		previous = strings.ToValidUTF8(previous[:2000], "")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		Title           string `json:"title"`
		Harness         string `json:"harness"`
		ProcessStatus   string `json:"processStatus"`
		ExitCode        *int   `json:"exitCode,omitempty"`
		TerminalTail    string `json:"terminalTail"`
		PreviousSummary string `json:"previousSummary,omitempty"`
		Partial         bool   `json:"partialObservation"`
	}{source.Session.Title, source.Session.Harness, source.Session.Status, source.Session.ExitCode, source.Text, previous, true})
	if err != nil || out.Len() > observerPromptLimit {
		return nil, errors.New("Terminal context exceeds the summary limit")
	}
	return out.Bytes(), nil
}

func (o *observerManager) runNext(ctx context.Context, now time.Time) bool {
	// Disabled hosts must not capture terminal context as background work.
	o.mu.Lock()
	idle := ctx.Err() != nil || !o.state.Config.Enabled || o.err != "" || o.active != ""
	o.mu.Unlock()
	if idle {
		return false
	}
	sources := o.s.observerSources()
	o.mu.Lock()
	if ctx.Err() != nil || !o.state.Config.Enabled || o.err != "" || o.active != "" {
		o.mu.Unlock()
		return false
	}
	for id, rec := range o.state.Records {
		if src, ok := sources[id]; !ok || !src.Session.CreatedAt.Equal(rec.SessionCreatedAt) {
			delete(o.state.Records, id)
		}
	}
	attempts := o.state.Attempts[:0]
	for _, attempt := range o.state.Attempts {
		if attempt.After(now.Add(-time.Hour)) {
			attempts = append(attempts, attempt)
		}
	}
	o.state.Attempts = attempts
	if len(attempts) >= observerHourlyLimit {
		o.mu.Unlock()
		return false
	}
	ids := make([]string, 0, len(sources))
	for id, source := range sources {
		rec, exists := o.state.Records[id]
		if source.Text == "" {
			continue
		}
		if exists && rec.Status != "queued" {
			if rec.Status == "error" || rec.Status == "running" || now.Sub(rec.LastAttempt) < time.Duration(o.state.Config.IntervalSeconds)*time.Second {
				continue
			}
			if rec.Digest == source.digest && rec.Model == o.state.Config.Model && rec.Provider == o.state.Config.Provider {
				continue
			}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		o.mu.Unlock()
		return false
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := o.state.Records[ids[i]], o.state.Records[ids[j]]
		if (a.Status == "queued") != (b.Status == "queued") {
			return a.Status == "queued"
		}
		if a.LastAttempt.Equal(b.LastAttempt) {
			return ids[i] < ids[j]
		}
		return a.LastAttempt.Before(b.LastAttempt)
	})
	id := ids[0]
	source := sources[id]
	rec := o.state.Records[id]
	rec.SessionID, rec.SessionCreatedAt = id, source.Session.CreatedAt
	rec.Status, rec.Error, rec.UpdatedAt, rec.Stale = "running", "", now, true
	rec.LastAttempt = now
	if rec.GeneratedAt == nil {
		rec.Provider, rec.Model = o.state.Config.Provider, o.state.Config.Model
	}
	if rec.Steps == nil {
		rec.Steps = []string{}
		rec.NextSteps = []string{}
		rec.Blockers = []string{}
	}
	o.state.Records[id] = rec
	o.state.Attempts = append(o.state.Attempts, now)
	if err := o.saveLocked(); err != nil {
		// The provider was never dispatched. Keep this recoverable with an
		// explicit refresh after storage is repaired, not a phantom worker.
		rec.Status, rec.Error = "error", "Summary could not start because its state could not be saved. Save settings, then refresh explicitly."
		o.state.Records[id] = rec
		o.err = err.Error()
		o.mu.Unlock()
		return false
	}
	workerCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	o.active, o.cancel = id, cancel
	cfg := o.state.Config
	o.mu.Unlock()
	prompt, err := observerPrompt(source, rec.Summary)
	var content ObserverContent
	if p, lookupErr := o.s.lookup(id); lookupErr != nil || !p.snapshot().CreatedAt.Equal(source.Session.CreatedAt) {
		cancel()
	}
	if err == nil {
		content, err = o.run(workerCtx, cfg, prompt)
	}
	cancelled := workerCtx.Err() != nil
	cancel()
	current := o.s.observerSources()
	o.finishRun(source, cfg, content, err, cancelled, now, current)
	return true
}

func (o *observerManager) finishRun(source observerSource, cfg ObserverConfig, content ObserverContent, err error, cancelled bool, sampledAt time.Time, current map[string]observerSource) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active, o.cancel = "", nil
	id := source.Session.ID
	latest, exists := current[id]
	rec, owned := o.state.Records[id]
	// Sources were sampled outside this mutex. A concurrent session deletion
	// may already have removed the owned record; never recreate it from a
	// stale source snapshot or publish into a replacement record.
	if !owned || !rec.SessionCreatedAt.Equal(source.Session.CreatedAt) {
		return
	}
	if !exists || !latest.Session.CreatedAt.Equal(source.Session.CreatedAt) {
		delete(o.state.Records, id)
	} else {
		rec.UpdatedAt = time.Now().UTC()
		if err != nil || cancelled {
			rec.Status, rec.Stale = "error", true
			rec.Error = "Summary failed or was interrupted. Check the provider sign-in and model, then refresh explicitly."
		} else {
			rec.ObserverContent = content
			rec.Status, rec.Error = "ready", ""
			rec.Provider, rec.Model = cfg.Provider, cfg.Model
			rec.SampledAt, rec.GeneratedAt = &sampledAt, &rec.UpdatedAt
			rec.SourceStatus, rec.Digest = source.Session.Status, source.digest
			rec.Stale = latest.digest != source.digest
		}
		o.state.Records[id] = rec
	}
	if err := o.saveLocked(); err != nil {
		o.err = err.Error()
	}
}

type observerResponse struct {
	Config          ObserverConfig     `json:"config"`
	Running         bool               `json:"running"`
	ActiveSessionID string             `json:"activeSessionId,omitempty"`
	Summaries       []SessionSummary   `json:"summaries"`
	Error           string             `json:"error,omitempty"`
	Providers       []observerProvider `json:"providers"`
	Limits          struct {
		RequestsPerHour int `json:"requestsPerHour"`
		Remaining       int `json:"remaining"`
	} `json:"limits"`
}

type observerProvider struct {
	ID        string `json:"id"`
	Supported bool   `json:"supported"`
	Detail    string `json:"detail,omitempty"`
}

func (o *observerManager) snapshot() observerResponse {
	sources := o.s.observerSources()
	o.mu.Lock()
	defer o.mu.Unlock()
	result := observerResponse{Config: o.state.Config, Running: o.active != "", ActiveSessionID: o.active, Error: o.err, Summaries: []SessionSummary{}, Providers: []observerProvider{{ID: "claude", Supported: true}, {ID: "codex", Supported: false, Detail: "Codex sessions can be summarized by Claude Code. A tool-disabled Codex summary worker is not available yet."}}}
	result.Limits.RequestsPerHour = observerHourlyLimit
	result.Limits.Remaining = observerHourlyLimit
	for _, attempt := range o.state.Attempts {
		if time.Since(attempt) < time.Hour {
			result.Limits.Remaining--
		}
	}
	for id, rec := range o.state.Records {
		src, ok := sources[id]
		if !ok || !src.Session.CreatedAt.Equal(rec.SessionCreatedAt) {
			continue
		}
		rec.Stale = rec.Status != "ready" || rec.Digest != src.digest || rec.Model != o.state.Config.Model || rec.Provider != o.state.Config.Provider
		result.Summaries = append(result.Summaries, rec.SessionSummary)
	}
	sort.Slice(result.Summaries, func(i, j int) bool { return result.Summaries[i].UpdatedAt.After(result.Summaries[j].UpdatedAt) })
	return result
}

func (s *Server) getObserver(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, s.observer.snapshot())
}

func (o *observerManager) removeSession(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.active == id && o.cancel != nil {
		o.cancel()
	}
	if _, ok := o.state.Records[id]; ok {
		delete(o.state.Records, id)
		if err := o.saveLocked(); err != nil {
			o.err = err.Error()
		}
	}
}

func (s *Server) configureObserver(w http.ResponseWriter, r *http.Request) {
	var cfg ObserverConfig
	if err := decode(w, r, &cfg); err != nil {
		fail(w, 400, err)
		return
	}
	if !validObserverConfig(cfg) {
		fail(w, 400, errors.New("Choose Claude Code, a valid model, and an interval from 60 to 3600 seconds"))
		return
	}
	o := s.observer
	if cfg.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := o.validate(ctx, cfg)
		cancel()
		if err != nil {
			fail(w, 409, err)
			return
		}
	}
	o.mu.Lock()
	previous := o.state.Config
	o.state.Config = cfg
	if err := o.saveLocked(); err != nil {
		o.state.Config = previous
		o.err = err.Error()
		o.mu.Unlock()
		fail(w, 500, err)
		return
	}
	o.err = ""
	if cfg != previous && o.cancel != nil {
		o.cancel()
	}
	o.mu.Unlock()
	o.signal()
	jsonResponse(w, 200, o.snapshot())
}

func (s *Server) refreshObserver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID        string    `json:"sessionId"`
		SessionCreatedAt time.Time `json:"sessionCreatedAt"`
	}
	if err := decode(w, r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	sources := s.observerSources()
	source, ok := sources[req.SessionID]
	if !ok || !source.Session.CreatedAt.Equal(req.SessionCreatedAt) {
		fail(w, 409, errors.New("This agent session no longer exists"))
		return
	}
	if source.Text == "" {
		detail := source.unavailable
		if detail == "" {
			detail = "This agent session has no terminal output to summarize yet"
		}
		fail(w, 409, errors.New(detail))
		return
	}
	o := s.observer
	// Pair admission with session deletion. Removal happens under s.mu and
	// then clears observer state; holding it until o.mu is acquired closes
	// the stale-source window without reversing either lock order.
	s.mu.Lock()
	p := s.sessions[req.SessionID]
	if p == nil || !p.snapshot().CreatedAt.Equal(req.SessionCreatedAt) {
		s.mu.Unlock()
		fail(w, 409, errors.New("This agent session no longer exists"))
		return
	}
	o.mu.Lock()
	s.mu.Unlock()
	if !o.state.Config.Enabled || o.err != "" {
		o.mu.Unlock()
		fail(w, 409, errors.New("Enable and save summary settings on this host first"))
		return
	}
	remaining := observerHourlyLimit
	for _, attempt := range o.state.Attempts {
		if time.Since(attempt) < time.Hour {
			remaining--
		}
	}
	if remaining <= 0 {
		o.mu.Unlock()
		fail(w, 429, errors.New("This host has reached its summary limit of 60 requests per hour"))
		return
	}
	rec, exists := o.state.Records[req.SessionID]
	if rec.Status == "queued" || rec.Status == "running" {
		o.mu.Unlock()
		fail(w, 409, errors.New("A summary is already queued or running for this session"))
		return
	}
	previous := rec
	rec.SessionID, rec.SessionCreatedAt = req.SessionID, req.SessionCreatedAt
	rec.Status, rec.Error, rec.Stale, rec.UpdatedAt = "queued", "", true, time.Now().UTC()
	if !exists {
		rec.ObserverContent = ObserverContent{Steps: []string{}, NextSteps: []string{}, Blockers: []string{}}
		rec.Provider, rec.Model = o.state.Config.Provider, o.state.Config.Model
	}
	o.state.Records[req.SessionID] = rec
	if err := o.saveLocked(); err != nil {
		if exists {
			o.state.Records[req.SessionID] = previous
		} else {
			delete(o.state.Records, req.SessionID)
		}
		o.err = err.Error()
		o.mu.Unlock()
		fail(w, 500, err)
		return
	}
	o.mu.Unlock()
	o.signal()
	jsonResponse(w, 202, o.snapshot())
}
