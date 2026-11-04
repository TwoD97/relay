package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/personalwhatsapp"
)

type personalSeen struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}
type personalWhatsAppRecord struct {
	Format    int                     `json:"format"`
	Epoch     string                  `json:"epoch"`
	Enabled   bool                    `json:"enabled"`
	StartedAt time.Time               `json:"startedAt"`
	Seen      []personalSeen          `json:"seen"`
	Sent      []string                `json:"sent"`
	Tickets   []controlApprovalTicket `json:"tickets"`
}
type personalWhatsAppView struct {
	Configured  bool       `json:"configured"`
	Paired      bool       `json:"paired"`
	Connected   bool       `json:"connected"`
	Status      string     `json:"status"`
	PairExpires *time.Time `json:"pairExpires,omitempty"`
	Error       string     `json:"error,omitempty"`
}
type personalInbound struct {
	epoch   string
	message personalwhatsapp.Message
}
type personalWhatsAppStore struct {
	mu              sync.Mutex
	lifecycle       sync.Mutex
	directory, path string
	record          personalWhatsAppRecord
	err             error
	status          string
	pairExpires     time.Time
	client          personalwhatsapp.Client
	factory         personalwhatsapp.Factory
	bundleDir       string
	incoming        chan personalInbound
	lastConnect     time.Time
}

func openPersonalWhatsAppStore(dir string) *personalWhatsAppStore {
	dir = filepath.Join(dir, "whatsapp-personal-private")
	s := &personalWhatsAppStore{directory: dir, path: filepath.Join(dir, "settings.json"), record: personalWhatsAppRecord{Format: 1}, incoming: make(chan personalInbound, 32)}
	data, err := readWorkspaceFile(s.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		err = strictWorkspaceJSON(data, &s.record)
	}
	if err == nil {
		err = validatePersonalRecord(s.record)
	}
	if err != nil {
		s.err = errors.New("Personal WhatsApp settings could not be read. Its private files were preserved.")
	}
	return s
}
func validatePersonalRecord(r personalWhatsAppRecord) error {
	if r.Format != 1 || (r.Epoch != "" && !validMastermindID(r.Epoch)) || (r.Enabled && (r.Epoch == "" || r.StartedAt.IsZero())) || len(r.Seen) > 256 || len(r.Sent) > 512 || len(r.Tickets) > 100 {
		return errors.New("invalid personal WhatsApp record")
	}
	for _, item := range r.Seen {
		if !personalwhatsapp.ValidMessageID(item.ID) || item.At.IsZero() {
			return errors.New("invalid personal WhatsApp receipt")
		}
	}
	for _, item := range r.Sent {
		if len(item) > 256 {
			return errors.New("invalid personal WhatsApp delivery")
		}
	}
	for _, ticket := range r.Tickets {
		if !validMastermindID(ticket.ID) || !validMastermindID(ticket.Request) || !validMastermindID(ticket.Session) || ticket.Host == "" || ticket.CreatedAt.IsZero() || ticket.ExpiresAt.IsZero() {
			return errors.New("invalid personal WhatsApp approval")
		}
	}
	return nil
}
func (s *personalWhatsAppStore) snapshot() personalWhatsAppRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.record
	r.Seen = append([]personalSeen{}, r.Seen...)
	r.Sent = append([]string{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	return r
}
func (s *personalWhatsAppStore) update(epoch string, change func(*personalWhatsAppRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if epoch != "" && s.record.Epoch != epoch {
		return errors.New("Personal WhatsApp configuration changed")
	}
	r := s.record
	r.Seen = append([]personalSeen{}, r.Seen...)
	r.Sent = append([]string{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	if err := change(&r); err != nil {
		return err
	}
	if err := validatePersonalRecord(r); err != nil {
		return err
	}
	if err := personalwhatsapp.PrepareDirectory(s.directory); err != nil {
		return errors.New("Personal WhatsApp private directory is unavailable")
	}
	if err := saveControlFile(s.path, r); err != nil {
		s.err = errors.New("Personal WhatsApp settings save was not confirmed. Restart Relay before another attempt.")
		return s.err
	}
	s.record = r
	return nil
}
func (s *personalWhatsAppStore) view() personalWhatsAppView {
	if s == nil {
		return personalWhatsAppView{Status: "Not connected"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	view := personalWhatsAppView{Configured: s.record.Enabled, Status: s.status}
	if s.client != nil {
		state := s.client.State()
		view.Paired = state.Linked
		view.Connected = state.Connected
		if state.Problem != "" {
			view.Error = state.Problem
		}
	}
	if s.err != nil {
		view.Error = s.err.Error()
	}
	if !s.pairExpires.IsZero() && !view.Paired {
		at := s.pairExpires
		view.PairExpires = &at
	}
	return view
}
func (s *personalWhatsAppStore) setStatus(value string) { s.mu.Lock(); s.status = value; s.mu.Unlock() }
func (s *personalWhatsAppStore) setEpochStatus(epoch, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Epoch == epoch && s.record.Enabled {
		s.status = value
	}
}
func (s *personalWhatsAppStore) current(epoch string) personalwhatsapp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.record.Enabled || s.record.Epoch != epoch {
		return nil
	}
	return s.client
}
func (s *personalWhatsAppStore) replaceClient(client personalwhatsapp.Client) {
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}
func (s *personalWhatsAppStore) openClient(ctx context.Context, epoch string) (personalwhatsapp.Client, error) {
	factory := s.factory
	if factory == nil {
		factory = func(ctx context.Context, directory string, receive func(personalwhatsapp.Message)) (personalwhatsapp.Client, error) {
			return personalwhatsapp.OpenWithBundle(ctx, directory, s.bundleDir, receive)
		}
	}
	return factory(ctx, filepath.Join(s.directory, epoch), func(message personalwhatsapp.Message) {
		select {
		case s.incoming <- personalInbound{epoch, message}:
		default:
			s.setEpochStatus(epoch, "Personal WhatsApp received too many commands. Review Relay before sending another.")
		}
	})
}
func (s *Server) registerPersonalWhatsApp(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/mastermind/control/whatsapp-personal", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.personalWhatsApp.view()) })
	mux.HandleFunc("POST /api/mastermind/control/whatsapp-personal", s.configurePersonalWhatsApp)
	mux.HandleFunc("DELETE /api/mastermind/control/whatsapp-personal", s.disconnectPersonalWhatsApp)
}

var personalPhone = regexp.MustCompile(`^[1-9][0-9]{6,14}$`)
var personalPairCode = regexp.MustCompile(`^[A-Z1-9]{4}-[A-Z1-9]{4}$`)

func (s *Server) configurePersonalWhatsApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if decodeJSON(w, r, &body) != nil {
		writeError(w, 400, "Enter an international phone number")
		return
	}
	body.Phone = strings.TrimPrefix(strings.TrimSpace(body.Phone), "+")
	if !personalPhone.MatchString(body.Phone) {
		writeError(w, 400, "Enter the international phone number, including country code and digits only")
		return
	}
	p := s.personalWhatsApp
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	view := p.view()
	if view.Configured {
		writeError(w, 409, "Personal WhatsApp is already configured. Use the pending code or disconnect before pairing again.")
		return
	}
	if view.PairExpires != nil && time.Now().Before(*view.PairExpires) {
		writeError(w, 409, "A phone-link code is already pending. Use it or disconnect before starting another pairing.")
		return
	}
	old := p.snapshot()
	if client := p.current(old.Epoch); client != nil {
		_ = client.Close()
		p.replaceClient(nil)
	}
	epoch := randomID()[:32]
	if err := p.update("", func(next *personalWhatsAppRecord) error {
		*next = personalWhatsAppRecord{Format: 1, Epoch: epoch, Enabled: true, StartedAt: time.Now().UTC()}
		return nil
	}); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	client, err := p.openClient(s.ctx, epoch)
	if err != nil {
		p.setStatus("Personal WhatsApp could not open its optional connector or private storage. Install a Relay package containing the connector. No pairing was started.")
		writeError(w, 502, p.view().Status)
		return
	}
	p.replaceClient(client)
	expires := time.Now().UTC().Add(2 * time.Minute)
	p.mu.Lock()
	p.pairExpires = expires
	p.status = "Waiting for the phone-link code to be entered in Linked devices"
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	code, err := client.Pair(ctx, body.Phone)
	if err != nil || !personalPairCode.MatchString(code) {
		p.setStatus("Pairing could not be confirmed. Disconnect before requesting a new phone-link code.")
		writeError(w, 502, p.view().Status)
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "expiresAt": expires, "status": p.view()})
}
func (s *Server) disconnectPersonalWhatsApp(w http.ResponseWriter, r *http.Request) {
	p := s.personalWhatsApp
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	old := p.snapshot()
	client := p.current(old.Epoch)
	if err := p.update("", func(next *personalWhatsAppRecord) error {
		*next = personalWhatsAppRecord{Format: 1, Epoch: randomID()[:32]}
		return nil
	}); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	p.replaceClient(nil)
	p.mu.Lock()
	p.pairExpires = time.Time{}
	p.mu.Unlock()
	var unlinkErr error
	if client != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		unlinkErr = client.Logout(ctx)
		cancel()
		_ = client.Close()
	} else if old.Enabled {
		unlinkErr = errors.New("The saved WhatsApp device was not connected for remote unlinking")
	}
	if old.Epoch != "" {
		if err := personalwhatsapp.Forget(filepath.Join(p.directory, old.Epoch)); err != nil && unlinkErr == nil {
			unlinkErr = err
		}
	}
	if unlinkErr != nil {
		p.setStatus("Connector stopped. Unlinking or local cleanup could not be fully confirmed; remove Relay from Linked devices on your phone and inspect its private directory.")
	} else {
		p.setStatus("Personal WhatsApp disconnected")
	}
	writeJSON(w, 200, p.view())
}
func (s *Server) monitorPersonalWhatsApp() {
	defer s.controlWG.Done()
	p := s.personalWhatsApp
	defer func() {
		p.lifecycle.Lock()
		defer p.lifecycle.Unlock()
		record := p.snapshot()
		if client := p.current(record.Epoch); client != nil {
			_ = client.Close()
		}
		p.replaceClient(nil)
	}()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case incoming := <-p.incoming:
			if err := s.handlePersonalWhatsApp(incoming.epoch, incoming.message); err != nil {
				p.setEpochStatus(incoming.epoch, err.Error())
			}
		case <-ticker.C:
			s.connectPersonalWhatsApp()
			record := p.snapshot()
			if client := p.current(record.Epoch); client != nil && client.State().Connected {
				s.deliverPersonalWhatsApp(record)
			}
		}
	}
}
func (s *Server) connectPersonalWhatsApp() {
	p := s.personalWhatsApp
	if !p.lifecycle.TryLock() {
		return
	}
	defer p.lifecycle.Unlock()
	record := p.snapshot()
	if s.coordinatorDelegated() {
		if client := p.current(record.Epoch); client != nil {
			_ = client.Close()
			p.replaceClient(nil)
		}
		p.setStatus("Mastermind moved; link WhatsApp on its current controller")
		return
	}
	if !record.Enabled {
		return
	}
	client := p.current(record.Epoch)
	if client != nil && client.State().Stopped {
		_ = client.Close()
		p.replaceClient(nil)
		client = nil
	}
	p.mu.Lock()
	pairExpired := !p.pairExpires.IsZero() && time.Now().After(p.pairExpires)
	p.mu.Unlock()
	if client == nil && pairExpired {
		return
	}
	if client == nil {
		var err error
		client, err = p.openClient(s.ctx, record.Epoch)
		if err != nil {
			p.setStatus("Personal WhatsApp optional connector or private storage is unavailable")
			return
		}
		p.replaceClient(client)
	}
	state := client.State()
	if state.Problem != "" {
		p.setStatus(state.Problem)
		return
	}
	if !state.Linked {
		p.mu.Lock()
		expired := !p.pairExpires.IsZero() && time.Now().After(p.pairExpires)
		p.mu.Unlock()
		if expired {
			_ = client.Close()
			p.replaceClient(nil)
			p.setStatus("Phone-link code expired. Disconnect, then pair again.")
		}
		return
	}
	if state.Connected {
		p.setStatus("Connected to Message yourself; commands begin with !relay ")
		return
	}
	if time.Since(p.lastConnect) < 15*time.Second {
		return
	}
	p.lastConnect = time.Now()
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		p.setStatus("Personal WhatsApp is offline. Relay will reconnect without repeating commands or unknown deliveries.")
	}
}

var errPersonalDuplicate = errors.New("personal WhatsApp command already recorded")

func (s *Server) handlePersonalWhatsApp(epoch string, message personalwhatsapp.Message) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	p := s.personalWhatsApp
	record := p.snapshot()
	now := time.Now().UTC()
	if !record.Enabled || record.Epoch != epoch || !personalwhatsapp.ValidMessageID(message.ID) || message.At.Before(record.StartedAt) || message.At.Before(now.Add(-10*time.Minute)) || message.At.After(now.Add(time.Minute)) || !boundedControlText(message.Text, 8000) {
		return nil
	}
	err := p.update(epoch, func(next *personalWhatsAppRecord) error {
		if !next.Enabled {
			return errPersonalDuplicate
		}
		alive := []personalSeen{}
		recent := 0
		for _, seen := range next.Seen {
			if seen.ID == message.ID {
				return errPersonalDuplicate
			}
			if seen.At.After(now.Add(-11 * time.Minute)) {
				alive = append(alive, seen)
				if seen.At.After(now.Add(-time.Minute)) {
					recent++
				}
			}
		}
		if len(alive) >= 256 || recent >= 8 {
			return errors.New("Personal WhatsApp command limit reached. Wait a minute and review Relay before sending more.")
		}
		next.Seen = append(alive, personalSeen{message.ID, now})
		return nil
	})
	if errors.Is(err, errPersonalDuplicate) {
		return nil
	}
	if err != nil {
		return err
	}
	text := strings.TrimSpace(message.Text)
	reply := ""
	switch {
	case text == "status":
		reply = s.remoteControlStatus()
	case text == "approvals":
		s.sendPersonalApprovals(record)
		reply = "Current permission requests are listed when available. Open Relay if a machine is unavailable."
	case strings.HasPrefix(text, "allow ") || strings.HasPrefix(text, "deny "):
		fields := strings.Fields(text)
		if len(fields) != 2 || !validMastermindID(fields[1]) {
			reply = "Use !relay allow ID or !relay deny ID with the exact current request ID."
		} else {
			reply = s.personalDecision(record, fields[1], fields[0])
		}
	default:
		digest := sha256.Sum256([]byte(epoch + ":" + message.ID))
		if err = s.enqueuePersonalInstruction(epoch, hex.EncodeToString(digest[:16]), text, "whatsapp-personal:"+epoch+":"+message.ID); err != nil {
			return err
		}
		reply = "Queued for Mastermind. This is an instruction, not terminal input. Use !relay status to check whether the coordinator is listening."
	}
	return s.sendPersonalOnce(record, "message:"+message.ID, reply)
}
func (s *Server) enqueuePersonalInstruction(epoch, id, text, key string) error {
	// Revocation and inbox admission share one lock boundary. A message that was
	// received before Disconnect cannot queue new work after its epoch is revoked.
	p := s.personalWhatsApp
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.record.Enabled || p.record.Epoch != epoch || p.err != nil {
		return errors.New("Personal WhatsApp configuration changed; no instruction was queued")
	}
	_, err := s.control.enqueue(id, "whatsapp-personal", text, key)
	return err
}
func (s *Server) personalSend(record personalWhatsAppRecord, key, text string) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	client := s.personalWhatsApp.current(record.Epoch)
	if client == nil {
		return errors.New("Personal WhatsApp configuration changed")
	}
	if len([]rune(text)) > 3700 {
		text = string([]rune(text)[:3700]) + "\n[Open Relay for the full result]"
	}
	id := sha256.Sum256([]byte(record.Epoch + ":" + key))
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	return client.Send(ctx, "3EB0"+strings.ToUpper(hex.EncodeToString(id[:20])), text)
}
func (s *Server) sendPersonalOnce(record personalWhatsAppRecord, key, text string) error {
	err := s.personalWhatsApp.update(record.Epoch, func(next *personalWhatsAppRecord) error {
		for _, old := range next.Sent {
			if old == key {
				return errTelegramSent
			}
		}
		next.Sent = append(next.Sent, key)
		if len(next.Sent) > 512 {
			next.Sent = next.Sent[len(next.Sent)-512:]
		}
		return nil
	})
	if errors.Is(err, errTelegramSent) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = s.personalSend(record, key, text); err != nil {
		s.personalWhatsApp.setEpochStatus(record.Epoch, "A personal WhatsApp delivery could not be confirmed. Review Relay; it will not be repeated automatically.")
	}
	return nil
}
func (s *Server) deliverPersonalWhatsApp(record personalWhatsAppRecord) {
	if s.coordinatorDelegated() {
		return
	}
	state, _, err := s.control.snapshot()
	if err != nil {
		return
	}
	prefix := "whatsapp-personal:" + record.Epoch + ":"
	for _, item := range state.Messages {
		if item.Source != "whatsapp-personal" || item.Status != "completed" || item.Delivery != "" || !strings.HasPrefix(item.Key, prefix) {
			continue
		}
		if s.control.claimDelivery(item.ID, "whatsapp-personal", prefix) != nil {
			continue
		}
		outcome := "sent"
		if err = s.personalSend(record, "reply:"+item.ID, "Mastermind\n"+item.Reply); err != nil {
			outcome = "unknown"
			s.personalWhatsApp.setEpochStatus(record.Epoch, "A result delivery could not be confirmed. It remains in Relay and will not be repeated automatically.")
		}
		_ = s.control.finishDelivery(item.ID, outcome)
	}
	s.sendPersonalApprovals(record)
}
func (s *Server) sendPersonalApprovals(record personalWhatsAppRecord) {
	if s.coordinatorDelegated() {
		return
	}
	hosts := s.store.list()
	if s.hasLocalMastermind() {
		hosts = append(hosts, Host{ID: "local", Name: "This computer", Status: "online"})
	}
	for _, host := range hosts {
		if host.Status != "online" {
			continue
		}
		requests, err := s.controlApprovals(host.ID)
		if err != nil {
			continue
		}
		for _, request := range requests {
			if request.Status != "pending" || !time.Now().Before(request.ExpiresAt) {
				continue
			}
			ticket := controlApprovalTicket{ID: randomID()[:32], Host: host.ID, Request: request.ID, Session: request.SessionID, CreatedAt: request.SessionCreatedAt, ExpiresAt: request.ExpiresAt, CanAllow: approvalFits(request, host.Name, 3400) && len([]rune(string(request.Input))) <= 2300}
			err = s.personalWhatsApp.update(record.Epoch, func(next *personalWhatsAppRecord) error {
				alive := []controlApprovalTicket{}
				for _, old := range next.Tickets {
					if old.Host == ticket.Host && old.Request == ticket.Request {
						return errTelegramSent
					}
					if time.Now().Before(old.ExpiresAt) {
						alive = append(alive, old)
					}
				}
				if len(alive) >= 100 {
					return errors.New("Too many pending personal WhatsApp approvals")
				}
				next.Tickets = append(alive, ticket)
				return nil
			})
			if err != nil {
				continue
			}
			input := string(request.Input)
			if len([]rune(input)) > 2300 {
				input = string([]rune(input)[:2300]) + "\n[Truncated: review the complete request in Relay]"
			}
			text := fmt.Sprintf("Permission needed · %s\nTool: %s\nFolder: %s\nSession: %s\n%s\nExpires: %s\nDeny: !relay deny %s", host.Name, request.ToolName, request.Cwd, request.SessionID, input, request.ExpiresAt.Format(time.RFC3339), ticket.ID)
			if ticket.CanAllow {
				text += "\nAllow once: !relay allow " + ticket.ID
			}
			_ = s.sendPersonalOnce(record, "approval:"+ticket.ID, text)
		}
	}
}
func (s *Server) personalDecision(record personalWhatsAppRecord, id, decision string) string {
	var ticket controlApprovalTicket
	found := false
	for _, candidate := range s.personalWhatsApp.snapshot().Tickets {
		if candidate.ID == id {
			ticket = candidate
			found = true
			break
		}
	}
	if !found || ticket.Used || !time.Now().Before(ticket.ExpiresAt) {
		return "This approval expired or was already handled. Check Relay."
	}
	return s.applyControlDecision(ticket, decision, func() error {
		return s.personalWhatsApp.update(record.Epoch, func(next *personalWhatsAppRecord) error {
			for i := range next.Tickets {
				if next.Tickets[i].ID == id {
					if next.Tickets[i].Used {
						return errTelegramSent
					}
					next.Tickets[i].Used = true
					return nil
				}
			}
			return errors.New("Approval disappeared")
		})
	})
}
