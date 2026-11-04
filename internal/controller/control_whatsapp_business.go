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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/whatsappbridge"
)

type businessSent struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}
type whatsappBusinessRecord struct {
	Format        int                     `json:"format"`
	Epoch         string                  `json:"epoch"`
	BridgeURL     string                  `json:"bridgeUrl"`
	BridgeToken   string                  `json:"bridgeToken"`
	AccessToken   string                  `json:"accessToken"`
	PhoneNumberID string                  `json:"phoneNumberId"`
	Owner         string                  `json:"owner"`
	GraphVersion  string                  `json:"graphVersion"`
	ConfiguredAt  time.Time               `json:"configuredAt"`
	LastContactAt time.Time               `json:"lastContactAt"`
	Sent          []businessSent          `json:"sent"`
	Tickets       []controlApprovalTicket `json:"tickets"`
}
type whatsappBusinessStore struct {
	mu           sync.Mutex
	path         string
	record       whatsappBusinessRecord
	err          error
	status       string
	connected    bool
	client       *http.Client
	graphBase    string
	limitedUntil time.Time
}

var graphVersionPattern = regexp.MustCompile(`^v[1-9][0-9]{0,2}\.[0-9]{1,2}$`)
var businessPollToken = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validBridgeURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 2048 && !strings.ContainsAny(raw, "\r\n\x00")
}
func openWhatsAppBusinessStore(dir string) *whatsappBusinessStore {
	s := &whatsappBusinessStore{path: filepath.Join(dir, "whatsapp-business-private.json"), record: whatsappBusinessRecord{Format: 1}, graphBase: "https://graph.facebook.com", client: &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	data, err := readWorkspaceFile(s.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		err = strictWorkspaceJSON(data, &s.record)
	}
	r := s.record
	if err == nil && (r.Format != 1 || len(r.Sent) > 512 || len(r.Tickets) > 100 || (r.AccessToken != "" && (!validBridgeURL(r.BridgeURL) || !businessPollToken.MatchString(r.BridgeToken) || !whatsappbridge.Digits.MatchString(r.PhoneNumberID) || !whatsappbridge.Digits.MatchString(r.Owner) || !graphVersionPattern.MatchString(r.GraphVersion) || !validMastermindID(r.Epoch)))) {
		err = errors.New("invalid settings")
	}
	if err != nil {
		s.err = errors.New("WhatsApp Business settings could not be read; preserve whatsapp-business-private.json before recovery")
	}
	return s
}
func (s *whatsappBusinessStore) snapshot() whatsappBusinessRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.record
	r.Sent = append([]businessSent{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	return r
}
func (s *whatsappBusinessStore) update(epoch string, change func(*whatsappBusinessRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if epoch != "" && s.record.Epoch != epoch {
		return errors.New("WhatsApp Business connection changed")
	}
	r := s.record
	r.Sent = append([]businessSent{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	if err := change(&r); err != nil {
		return err
	}
	if err := saveControlFile(s.path, r); err != nil {
		s.err = errors.New("WhatsApp Business save was not confirmed. Reconnect Relay before changing it again")
		return s.err
	}
	s.record = r
	return nil
}
func (s *whatsappBusinessStore) view() map[string]any {
	if s == nil {
		return map[string]any{"configured": false, "connected": false, "status": ""}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := map[string]any{"configured": s.record.AccessToken != "", "connected": s.connected, "status": s.status}
	if !s.record.LastContactAt.IsZero() {
		v["lastContactAt"] = s.record.LastContactAt
	}
	if s.err != nil {
		v["error"] = s.err.Error()
	}
	return v
}
func (s *whatsappBusinessStore) setStatus(epoch, text string, connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Epoch == epoch {
		s.status = text
		s.connected = connected
	}
}
func (s *whatsappBusinessStore) call(ctx context.Context, target, token, method string, body, result any) error {
	s.mu.Lock()
	limited := time.Now().Before(s.limitedUntil)
	s.mu.Unlock()
	if limited {
		return errors.New("WhatsApp requested a pause; retry setup after one minute")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("Could not encode WhatsApp request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if err != nil {
		return errors.New("Invalid WhatsApp endpoint")
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return errors.New("WhatsApp connection failed; the outcome may be unknown")
	}
	defer response.Body.Close()
	if response.StatusCode == 429 {
		s.mu.Lock()
		s.limitedUntil = time.Now().Add(time.Minute)
		s.mu.Unlock()
		return errors.New("WhatsApp requested a pause; an outgoing action will not be retried automatically")
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || response.StatusCode < 200 || response.StatusCode > 299 {
		return errors.New("WhatsApp or its bridge rejected the request; check configuration and account permissions")
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return errors.New("WhatsApp returned an invalid response")
	}
	return nil
}
func (s *Server) RegisterWhatsAppBusinessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/mastermind/control/whatsapp-business", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.whatsappBusiness.view()) })
	mux.HandleFunc("POST /api/mastermind/control/whatsapp-business", s.configureWhatsAppBusiness)
	mux.HandleFunc("DELETE /api/mastermind/control/whatsapp-business", s.disconnectWhatsAppBusiness)
}
func (s *Server) configureWhatsAppBusiness(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BridgeURL     string `json:"bridgeUrl"`
		BridgeToken   string `json:"bridgeToken"`
		AccessToken   string `json:"accessToken"`
		PhoneNumberID string `json:"phoneNumberId"`
		Owner         string `json:"owner"`
		GraphVersion  string `json:"graphVersion"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	body.BridgeURL = strings.TrimRight(strings.TrimSpace(body.BridgeURL), "/")
	if !validBridgeURL(body.BridgeURL) || !businessPollToken.MatchString(body.BridgeToken) || len(body.AccessToken) < 20 || len(body.AccessToken) > 4096 || strings.ContainsAny(body.AccessToken, "\r\n\x00") || !whatsappbridge.Digits.MatchString(body.PhoneNumberID) || !whatsappbridge.Digits.MatchString(body.Owner) || !graphVersionPattern.MatchString(body.GraphVersion) {
		writeError(w, 400, "Enter an HTTPS bridge URL, its 64-character secret, Cloud API access token, exact phone-number ID, allowed sender ID, and Graph API version")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var info struct {
		Protocol      int    `json:"protocol"`
		PhoneNumberID string `json:"phoneNumberId"`
		Owner         string `json:"owner"`
	}
	store := s.whatsappBusiness
	if err := store.call(ctx, body.BridgeURL+"/api/info", body.BridgeToken, "GET", nil, &info); err != nil || info.Protocol != 1 || info.PhoneNumberID != body.PhoneNumberID || info.Owner != body.Owner {
		writeError(w, 502, "Could not verify the bridge's configured phone number and allowed sender")
		return
	}
	var phone struct {
		ID string `json:"id"`
	}
	if err := store.call(ctx, store.graphBase+"/"+body.GraphVersion+"/"+body.PhoneNumberID+"?fields=id", body.AccessToken, "GET", nil, &phone); err != nil || phone.ID != body.PhoneNumberID {
		writeError(w, 502, "Could not verify the Cloud API token and phone-number ID")
		return
	}
	err := store.update("", func(next *whatsappBusinessRecord) error {
		*next = whatsappBusinessRecord{Format: 1, Epoch: randomID()[:32], BridgeURL: body.BridgeURL, BridgeToken: body.BridgeToken, AccessToken: body.AccessToken, PhoneNumberID: body.PhoneNumberID, Owner: body.Owner, GraphVersion: body.GraphVersion, ConfiguredAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	store.setStatus(store.snapshot().Epoch, "Connected. Send a new message from the allowed sender to open the 24-hour reply window", true)
	writeJSON(w, 200, store.view())
}
func (s *Server) disconnectWhatsAppBusiness(w http.ResponseWriter, r *http.Request) {
	err := s.whatsappBusiness.update("", func(next *whatsappBusinessRecord) error {
		*next = whatsappBusinessRecord{Format: 1, Epoch: randomID()[:32]}
		return nil
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.whatsappBusiness.setStatus(s.whatsappBusiness.snapshot().Epoch, "Disconnected; queued instructions remain available in Relay", false)
	writeJSON(w, 200, s.whatsappBusiness.view())
}
func (s *Server) monitorWhatsAppBusiness() {
	defer s.controlWG.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.pollWhatsAppBusiness()
		}
	}
}
func (s *Server) pollWhatsAppBusiness() {
	if s.coordinatorDelegated() {
		return
	}
	store := s.whatsappBusiness
	record := store.snapshot()
	store.mu.Lock()
	blocked := store.err != nil || time.Now().Before(store.limitedUntil)
	store.mu.Unlock()
	if record.AccessToken == "" || blocked {
		return
	}
	var batch struct {
		Protocol int                    `json:"protocol"`
		Events   []whatsappbridge.Event `json:"events"`
	}
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	err := store.call(ctx, record.BridgeURL+"/api/events", record.BridgeToken, "GET", nil, &batch)
	cancel()
	if err != nil || batch.Protocol != 1 || len(batch.Events) > 32 {
		store.setStatus(record.Epoch, "Bridge polling failed. Instructions and outgoing actions were not replayed", false)
		return
	}
	store.setStatus(record.Epoch, "Bridge connected", true)
	for _, event := range batch.Events {
		if store.snapshot().Epoch != record.Epoch || s.coordinatorDelegated() {
			return
		}
		now := time.Now()
		valid := event.Sender == record.Owner && event.PhoneNumberID == record.PhoneNumberID && len(event.ID) > 0 && len(event.ID) <= 256 && event.SentAt.After(record.ConfiguredAt) && !event.SentAt.After(now.Add(time.Minute)) && event.ExpiresAt.Equal(event.SentAt.Add(10*time.Minute)) && now.Before(event.ExpiresAt) && boundedControlText(event.Text, 8000)
		if valid {
			if err = store.update(record.Epoch, func(next *whatsappBusinessRecord) error {
				if event.SentAt.After(next.LastContactAt) {
					next.LastContactAt = event.SentAt
				}
				return nil
			}); err != nil {
				return
			}
			if store.snapshot().Epoch != record.Epoch || s.coordinatorDelegated() {
				return
			}
			if event.SentAt.After(record.LastContactAt) {
				record.LastContactAt = event.SentAt
			}
			if err = s.handleWhatsAppBusiness(record, event); err != nil {
				store.setStatus(record.Epoch, "The instruction could not be saved; it remains queued in the bridge", true)
				return
			}
		}
		if store.snapshot().Epoch != record.Epoch || s.coordinatorDelegated() {
			return
		}
		ctx, cancel = context.WithTimeout(s.ctx, 8*time.Second)
		err = store.call(ctx, record.BridgeURL+"/api/events/ack", record.BridgeToken, "POST", map[string]any{"ids": []string{event.ID}}, nil)
		cancel()
		if err != nil {
			store.setStatus(record.Epoch, "Bridge acknowledgment was not confirmed. Saved instructions retain their exact message identity", true)
			return
		}
	}
	if current := store.snapshot(); current.Epoch == record.Epoch && current.AccessToken != "" {
		s.deliverWhatsAppBusiness(current)
	}
}
func (s *Server) businessSend(record whatsappBusinessRecord, text string) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	store := s.whatsappBusiness
	store.mu.Lock()
	allowed := store.err == nil && store.record.Epoch == record.Epoch && !time.Now().Before(store.limitedUntil) && !store.record.LastContactAt.IsZero() && time.Since(store.record.LastContactAt) < 24*time.Hour
	store.mu.Unlock()
	if !allowed {
		return errors.New("Send a new message from the allowed WhatsApp sender before Relay can reply")
	}
	if len([]rune(text)) > 3800 {
		text = string([]rune(text)[:3700]) + "\n[Truncated. Open Relay for the complete result.]"
	}
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	defer cancel()
	var result struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	err := store.call(ctx, store.graphBase+"/"+record.GraphVersion+"/"+record.PhoneNumberID+"/messages", record.AccessToken, "POST", map[string]any{"messaging_product": "whatsapp", "recipient_type": "individual", "to": record.Owner, "type": "text", "text": map[string]any{"body": text, "preview_url": false}}, &result)
	if err != nil {
		return err
	}
	if len(result.Messages) != 1 || result.Messages[0].ID == "" {
		return errors.New("Cloud API send acknowledgment could not be verified")
	}
	return nil
}
func (s *Server) businessSendOnce(record whatsappBusinessRecord, key, text string) error {
	err := s.whatsappBusiness.update(record.Epoch, func(next *whatsappBusinessRecord) error {
		now := time.Now()
		alive := []businessSent{}
		for _, old := range next.Sent {
			if old.Key == key {
				return errTelegramSent
			}
			if now.Sub(old.At) < 24*time.Hour {
				alive = append(alive, old)
			}
		}
		if len(alive) >= 512 {
			return errors.New("WhatsApp delivery receipt capacity reached; review Relay locally")
		}
		next.Sent = append(alive, businessSent{Key: key, At: now.UTC()})
		return nil
	})
	if errors.Is(err, errTelegramSent) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = s.businessSend(record, text); err != nil {
		s.whatsappBusiness.setStatus(record.Epoch, "A WhatsApp delivery could not be confirmed. Read it in Relay; it will not be repeated automatically", true)
	}
	return nil
}
func (s *Server) handleWhatsAppBusiness(record whatsappBusinessRecord, event whatsappbridge.Event) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	text := strings.TrimSpace(event.Text)
	key := "message:" + event.ID
	if text == "!status" {
		return s.businessSendOnce(record, key, s.remoteControlStatus())
	}
	if text == "!approvals" {
		s.sendBusinessApprovals(record)
		return s.businessSendOnce(record, key, "Current supported approvals are shown above. Review terminal-only prompts in Relay.")
	}
	if strings.HasPrefix(text, "!allow ") || strings.HasPrefix(text, "!deny ") {
		parts := strings.Fields(text)
		if len(parts) != 2 || !validMastermindID(parts[1]) {
			return s.businessSendOnce(record, key, "Use !allow ID or !deny ID from a current permission request.")
		}
		decision := "deny"
		if parts[0] == "!allow" {
			decision = "allow"
		}
		return s.businessSendOnce(record, key, s.businessDecision(record, parts[1], decision))
	}
	if strings.HasPrefix(text, "!") {
		return s.businessSendOnce(record, key, "Send an instruction, !status, !approvals, !allow ID, or !deny ID.")
	}
	digest := sha256.Sum256([]byte(record.Epoch + ":" + event.ID))
	s.whatsappBusiness.mu.Lock()
	if s.whatsappBusiness.record.Epoch != record.Epoch {
		s.whatsappBusiness.mu.Unlock()
		return errors.New("WhatsApp connection changed")
	}
	_, err := s.control.enqueue(hex.EncodeToString(digest[:16]), "whatsapp-business", text, "whatsapp-business:"+record.Epoch+":"+event.ID)
	s.whatsappBusiness.mu.Unlock()
	if err != nil {
		return err
	}
	return s.businessSendOnce(record, key, "Queued for Mastermind. This is an instruction, not terminal input. !status shows whether the coordinator is listening.")
}
func (s *Server) deliverWhatsAppBusiness(record whatsappBusinessRecord) {
	if s.coordinatorDelegated() {
		return
	}
	if record.LastContactAt.IsZero() || time.Since(record.LastContactAt) >= 24*time.Hour {
		s.whatsappBusiness.setStatus(record.Epoch, "Waiting for a new message from the allowed sender to open the 24-hour reply window", true)
		return
	}
	state, _, err := s.control.snapshot()
	if err != nil {
		return
	}
	prefix := "whatsapp-business:" + record.Epoch + ":"
	for _, item := range state.Messages {
		if item.Source != "whatsapp-business" || item.Status != "completed" || item.Delivery != "" || !strings.HasPrefix(item.Key, prefix) {
			continue
		}
		if s.control.claimDelivery(item.ID, "whatsapp-business", prefix) != nil {
			continue
		}
		outcome := "sent"
		if s.businessSend(record, "Mastermind\n"+item.Reply) != nil {
			outcome = "unknown"
			s.whatsappBusiness.setStatus(record.Epoch, "A result delivery was not confirmed. Read it in Relay; no send was repeated", true)
		}
		_ = s.control.finishDelivery(item.ID, outcome)
	}
	s.sendBusinessApprovals(record)
}
func (s *Server) sendBusinessApprovals(record whatsappBusinessRecord) {
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
			err = s.whatsappBusiness.update(record.Epoch, func(next *whatsappBusinessRecord) error {
				alive := []controlApprovalTicket{}
				for _, old := range next.Tickets {
					if old.Host == ticket.Host && old.Request == ticket.Request && old.Session == ticket.Session && old.CreatedAt.Equal(ticket.CreatedAt) {
						return errTelegramSent
					}
					if time.Now().Before(old.ExpiresAt) {
						alive = append(alive, old)
					}
				}
				if len(alive) >= 100 {
					return errors.New("Too many outstanding approvals")
				}
				next.Tickets = append(alive, ticket)
				return nil
			})
			if err != nil {
				continue
			}
			input := string(request.Input)
			if len([]rune(input)) > 2300 {
				input = string([]rune(input)[:2300]) + "\n[Truncated — review in Relay before allowing]"
			}
			text := fmt.Sprintf("Permission needed · %s\nTool: %s\nFolder: %s\nSession: %s\n%s\nDeny: !deny %s", host.Name, request.ToolName, request.Cwd, request.SessionID, input, ticket.ID)
			if ticket.CanAllow {
				text += "\nAllow once: !allow " + ticket.ID
			}
			_ = s.businessSendOnce(record, "approval:"+ticket.ID, text)
		}
	}
}
func (s *Server) businessDecision(record whatsappBusinessRecord, id, decision string) string {
	var ticket controlApprovalTicket
	for _, item := range s.whatsappBusiness.snapshot().Tickets {
		if item.ID == id {
			ticket = item
			break
		}
	}
	if ticket.ID == "" {
		return "This approval is unavailable. Check Relay."
	}
	return s.applyControlDecision(ticket, decision, func() error {
		return s.whatsappBusiness.update(record.Epoch, func(next *whatsappBusinessRecord) error {
			for i := range next.Tickets {
				if next.Tickets[i].ID == id {
					if next.Tickets[i].Used {
						return errTelegramSent
					}
					next.Tickets[i].Used = true
					return nil
				}
			}
			return errors.New("Approval unavailable")
		})
	})
}
