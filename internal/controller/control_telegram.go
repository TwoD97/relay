package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type controlApprovalTicket struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Request   string    `json:"request"`
	Session   string    `json:"session"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Used      bool      `json:"used"`
	CanAllow  bool      `json:"canAllow"`
}

// This private file is deliberately outside workspace/sync exports.
type telegramRecord struct {
	Format      int                     `json:"format"`
	Epoch       string                  `json:"epoch"`
	Token       string                  `json:"token"`
	Bot         string                  `json:"bot"`
	Owner       int64                   `json:"owner"`
	Chat        int64                   `json:"chat"`
	PairHash    string                  `json:"pairHash"`
	PairExpires time.Time               `json:"pairExpires"`
	Offset      int64                   `json:"offset"`
	Sent        []string                `json:"sent"`
	Tickets     []controlApprovalTicket `json:"tickets"`
}
type telegramStore struct {
	mu     sync.Mutex
	path   string
	record telegramRecord
	err    error
	status string
	base   string
	client *http.Client
}

func openTelegramStore(dir string) *telegramStore {
	s := &telegramStore{path: filepath.Join(dir, "telegram-private.json"), record: telegramRecord{Format: 1, Sent: []string{}, Tickets: []controlApprovalTicket{}}, base: "https://api.telegram.org", client: &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	data, err := readWorkspaceFile(s.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		err = strictWorkspaceJSON(data, &s.record)
	}
	if err == nil && (s.record.Format != 1 || len(s.record.Sent) > 512 || len(s.record.Tickets) > 100) {
		err = errors.New("invalid state")
	}
	if err != nil {
		s.err = errors.New("Telegram settings could not be read; preserve telegram-private.json before recovery")
	}
	return s
}
func (s *telegramStore) snapshot() telegramRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.record
	r.Sent = append([]string{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	return r
}
func (s *telegramStore) update(epoch string, change func(*telegramRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if epoch != "" && s.record.Epoch != epoch {
		return errors.New("Telegram configuration changed")
	}
	r := s.record
	r.Sent = append([]string{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	if err := change(&r); err != nil {
		return err
	}
	if err := saveControlFile(s.path, r); err != nil {
		s.err = errors.New("Telegram settings save could not be confirmed. Reconnect Relay before changing them again.")
		return s.err
	}
	s.record = r
	return nil
}
func (s *telegramStore) view() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	problem := s.status
	if s.err != nil {
		problem = s.err.Error()
	}
	return map[string]any{"configured": s.record.Token != "", "paired": s.record.Owner != 0, "bot": s.record.Bot, "status": problem, "pairExpires": s.record.PairExpires}
}
func (s *telegramStore) setStatus(message string) { s.mu.Lock(); s.status = message; s.mu.Unlock() }
func (s *telegramStore) call(ctx context.Context, token, method string, body any, result any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return errors.New("Could not encode Telegram request")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.base+"/bot"+token+"/"+method, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("Could not create Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return errors.New("Telegram connection failed; delivery may be unknown")
	}
	defer response.Body.Close()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil || response.StatusCode != 200 || json.Unmarshal(data, &envelope) != nil || !envelope.OK {
		return errors.New("Telegram rejected the request; check the bot token, connection, and bot ownership")
	}
	if result != nil && json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("Telegram returned an invalid response")
	}
	return nil
}

var telegramTokenPattern = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{25,100}$`)

func (s *Server) configureTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if !telegramTokenPattern.MatchString(body.Token) {
		writeError(w, 400, "Enter the bot token supplied by BotFather")
		return
	}
	var bot struct {
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.telegram.call(ctx, body.Token, "getMe", map[string]any{}, &bot); err != nil || !bot.IsBot {
		writeError(w, 502, "Could not verify this Telegram bot. Check its token and connection.")
		return
	}
	code := randomID()
	digest := sha256.Sum256([]byte(code))
	epoch := randomID()
	err := s.telegram.update("", func(record *telegramRecord) error {
		*record = telegramRecord{Format: 1, Epoch: epoch, Token: body.Token, Bot: bot.Username, PairHash: hex.EncodeToString(digest[:]), PairExpires: time.Now().UTC().Add(10 * time.Minute), Sent: []string{}, Tickets: []controlApprovalTicket{}}
		return nil
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.telegram.setStatus("Waiting for private-chat pairing")
	writeJSON(w, 200, map[string]any{"bot": bot.Username, "pairCommand": "/start " + code, "expiresAt": time.Now().UTC().Add(10 * time.Minute)})
}
func (s *Server) disconnectTelegram(w http.ResponseWriter, r *http.Request) {
	if err := s.telegram.update("", func(record *telegramRecord) error {
		*record = telegramRecord{Format: 1, Epoch: randomID(), Sent: []string{}, Tickets: []controlApprovalTicket{}}
		return nil
	}); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.telegram.setStatus("")
	s.controlState(w, r)
}

type telegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}
type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type telegramMessage struct {
	From telegramUser `json:"from"`
	Chat telegramChat `json:"chat"`
	Text string       `json:"text"`
	Date int64        `json:"date"`
}
type telegramUpdate struct {
	ID       int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
	Callback *struct {
		ID      string          `json:"id"`
		From    telegramUser    `json:"from"`
		Message telegramMessage `json:"message"`
		Data    string          `json:"data"`
	} `json:"callback_query"`
}

func (s *Server) monitorTelegram() {
	defer s.controlWG.Done()
	for {
		if s.ctx.Err() != nil {
			return
		}
		record := s.telegram.snapshot()
		if record.Token == "" || s.coordinatorDelegated() {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		var updates []telegramUpdate
		err := s.telegram.call(s.ctx, record.Token, "getUpdates", map[string]any{"offset": record.Offset, "timeout": 15, "limit": 30, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if err != nil {
			s.telegram.setStatus(err.Error())
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		for _, update := range updates {
			current := s.telegram.snapshot()
			if current.Epoch != record.Epoch || s.coordinatorDelegated() {
				break
			}
			if update.ID < current.Offset {
				continue
			}
			if err := s.handleTelegram(current, update); err != nil {
				s.telegram.setStatus(err.Error())
				break
			}
			if err := s.telegram.update(record.Epoch, func(next *telegramRecord) error { next.Offset = update.ID + 1; return nil }); err != nil {
				break
			}
		}
		current := s.telegram.snapshot()
		if current.Epoch == record.Epoch && current.Owner != 0 {
			s.deliverTelegram(current)
		}
	}
}
func (s *Server) telegramSend(record telegramRecord, text string, markup any) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	if s.telegram.snapshot().Epoch != record.Epoch {
		return errors.New("Telegram configuration changed")
	}
	if len([]rune(text)) > 3800 {
		text = string([]rune(text)[:3800]) + "\n… Open Relay for the full result."
	}
	body := map[string]any{"chat_id": record.Chat, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if markup != nil {
		body["reply_markup"] = markup
	}
	return s.telegram.call(s.ctx, record.Token, "sendMessage", body, nil)
}
func (s *Server) handleTelegram(record telegramRecord, update telegramUpdate) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	if message := update.Message; message != nil {
		if message.Chat.Type != "private" || message.From.IsBot || message.From.ID <= 0 || message.Chat.ID != message.From.ID {
			return nil
		}
		if record.Owner == 0 {
			if time.Now().After(record.PairExpires) || !strings.HasPrefix(message.Text, "/start ") {
				return nil
			}
			candidate := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(message.Text, "/start "))))
			expected, err := hex.DecodeString(record.PairHash)
			if err != nil || subtle.ConstantTimeCompare(expected, candidate[:]) != 1 {
				return nil
			}
			if err := s.telegram.update(record.Epoch, func(next *telegramRecord) error {
				next.Owner = message.From.ID
				next.Chat = message.Chat.ID
				next.PairHash = ""
				next.PairExpires = time.Time{}
				return nil
			}); err != nil {
				return err
			}
			s.telegram.setStatus("Connected to your private chat")
			record = s.telegram.snapshot()
			_ = s.telegramSend(record, "Relay is paired. Send an instruction to queue it for Mastermind. /status shows the inbox; /approvals shows current permission requests. Keep Relay running on this computer. Replies arrive after Mastermind claims and completes your instruction.", nil)
			return nil
		}
		if message.From.ID != record.Owner || message.Chat.ID != record.Chat {
			return nil
		}
		text := strings.TrimSpace(message.Text)
		if text == "" {
			return nil
		}
		// Queue commits are idempotent by Telegram update ID. Outbound receipts
		// are recorded before sending so a network loss never repeats a decision.
		if text == "/status" {
			return s.sendTelegramOnce(record, "update:"+strconv.FormatInt(update.ID, 10), s.remoteControlStatus(), nil)
		}
		if text == "/approvals" {
			s.sendTelegramApprovals(record)
			return nil
		}
		if strings.HasPrefix(text, "/") {
			return s.sendTelegramOnce(record, "update:"+strconv.FormatInt(update.ID, 10), "Send a plain-text instruction, /status, or /approvals. Destructive recovery still needs your explicit decision.", nil)
		}
		if !boundedControlText(text, 8000) {
			return s.sendTelegramOnce(record, "update:"+strconv.FormatInt(update.ID, 10), "Instruction is too long. Keep it under 8000 characters.", nil)
		}
		sum := sha256.Sum256([]byte(record.Epoch + ":" + strconv.FormatInt(update.ID, 10)))
		id := hex.EncodeToString(sum[:16])
		s.telegram.mu.Lock()
		if s.telegram.record.Epoch != record.Epoch || s.telegram.record.Owner != record.Owner {
			s.telegram.mu.Unlock()
			return errors.New("Telegram connection changed; instruction was not queued")
		}
		_, enqueueErr := s.control.enqueue(id, "telegram", text, "telegram:"+record.Epoch+":"+strconv.FormatInt(update.ID, 10))
		s.telegram.mu.Unlock()
		if err := enqueueErr; err != nil {
			return err
		}
		return s.sendTelegramOnce(record, "update:"+strconv.FormatInt(update.ID, 10), "Queued for Mastermind. This is an instruction, not terminal input. Use /status to check whether the coordinator is listening.", nil)
	}
	if callback := update.Callback; callback != nil && callback.From.ID == record.Owner && !callback.From.IsBot && callback.Message.Chat.Type == "private" && callback.Message.Chat.ID == record.Chat {
		parts := strings.Split(callback.Data, ":")
		if len(parts) != 3 || parts[0] != "approval" || (parts[2] != "allow" && parts[2] != "deny") {
			return nil
		}
		message := s.telegramDecision(record, parts[1], parts[2])
		_ = s.telegram.call(s.ctx, record.Token, "answerCallbackQuery", map[string]any{"callback_query_id": callback.ID, "text": message, "show_alert": true}, nil)
		return nil
	}
	return nil
}
func (s *Server) remoteControlStatus() string {
	state, listener, err := s.control.snapshot()
	if err != nil {
		return err.Error()
	}
	queued, processing := 0, 0
	for _, item := range state.Messages {
		if item.Status == "queued" {
			queued++
		}
		if item.Status == "processing" {
			processing++
		}
	}
	status := "Mastermind has not connected its inbox. Open Mastermind in Relay and ask it to run relay control wait."
	if !listener.IsZero() && time.Since(listener) < 45*time.Second {
		status = "Mastermind is listening."
	}
	return fmt.Sprintf("%s\nQueued: %d · Processing: %d\nRecovery: %t (diagnose and bounded recovery; ask before destructive actions)", status, queued, processing, state.Recovery)
}
func (s *Server) sendTelegramOnce(record telegramRecord, key, text string, markup any) error {
	err := s.telegram.update(record.Epoch, func(next *telegramRecord) error {
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
	if err = s.telegramSend(record, text, markup); err != nil {
		s.telegram.setStatus("A Telegram delivery could not be confirmed. Review the inbox in Relay; it will not be sent again automatically.")
	}
	return nil
}

var errTelegramSent = errors.New("delivery already attempted")

func (s *Server) deliverTelegram(record telegramRecord) {
	if s.coordinatorDelegated() {
		return
	}
	state, _, err := s.control.snapshot()
	if err != nil {
		return
	}
	for _, item := range state.Messages {
		if item.Source == "telegram" && item.Status == "completed" && item.Delivery == "" && strings.HasPrefix(item.Key, "telegram:"+record.Epoch+":") {
			err := s.control.update(func(next *controlRecord) error {
				for i := range next.Messages {
					if next.Messages[i].ID == item.ID {
						if next.Messages[i].Delivery != "" {
							return errTelegramSent
						}
						next.Messages[i].Delivery = "attempted"
						return nil
					}
				}
				return errors.New("Instruction no longer exists")
			})
			if err != nil {
				continue
			}
			outcome := "sent"
			if err = s.telegramSend(record, "Mastermind\n"+item.Reply, nil); err != nil {
				outcome = "unknown"
				s.telegram.setStatus("A result delivery could not be confirmed. Review the inbox in Relay; it will not be repeated automatically.")
			}
			_ = s.control.update(func(next *controlRecord) error {
				for i := range next.Messages {
					if next.Messages[i].ID == item.ID {
						next.Messages[i].Delivery = outcome
						return nil
					}
				}
				return nil
			})
		}
	}
	s.sendTelegramApprovals(record)
}

type controlApproval struct {
	ID               string          `json:"id"`
	SessionID        string          `json:"sessionId"`
	SessionCreatedAt time.Time       `json:"sessionCreatedAt"`
	ToolName         string          `json:"toolName"`
	Cwd              string          `json:"cwd"`
	Input            json.RawMessage `json:"input"`
	Status           string          `json:"status"`
	ExpiresAt        time.Time       `json:"expiresAt"`
}

func (s *Server) controlRuntimeClient(hostID string) (*http.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link := s.links[hostID]
	host, exists := s.store.get(hostID)
	if hostID == "local" && s.hasLocalMastermind() {
		host, exists = Host{Status: "online"}, true
	}
	if !exists || host.Status != "online" || link == nil || link.httpTransport == nil || link.repairing {
		return nil, errors.New("Machine is unavailable")
	}
	return &http.Client{Transport: link.httpTransport}, nil
}
func (s *Server) controlApprovals(hostID string) ([]controlApproval, error) {
	client, err := s.controlRuntimeClient(hostID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	data, err := runtimeJSON(ctx, client, "GET", "/api/approvals", nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Requests []controlApproval `json:"requests"`
	}
	if err = json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	valid := []controlApproval{}
	for _, item := range response.Requests {
		if validMastermindID(item.ID) && validMastermindID(item.SessionID) && !item.SessionCreatedAt.IsZero() {
			valid = append(valid, item)
		}
	}
	return valid, nil
}
func (s *Server) sendTelegramApprovals(record telegramRecord) {
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
			key := "approval:" + host.ID + ":" + request.ID
			ticket := controlApprovalTicket{ID: randomID()[:32], Host: host.ID, Request: request.ID, Session: request.SessionID, CreatedAt: request.SessionCreatedAt, ExpiresAt: request.ExpiresAt, CanAllow: approvalFits(request, host.Name, 3700) && len([]rune(string(request.Input))) <= 2000}
			err = s.telegram.update(record.Epoch, func(next *telegramRecord) error {
				for _, old := range next.Tickets {
					if old.Host == host.ID && old.Request == request.ID && old.Session == request.SessionID && old.CreatedAt.Equal(request.SessionCreatedAt) {
						return errTelegramSent
					}
				}
				for _, old := range next.Sent {
					if old == key {
						return errTelegramSent
					}
				}
				alive := []controlApprovalTicket{}
				for _, old := range next.Tickets {
					if time.Now().Before(old.ExpiresAt) {
						alive = append(alive, old)
					}
				}
				if len(alive) >= 100 {
					return errors.New("Too many pending Telegram approvals")
				}
				next.Tickets = append(alive, ticket)
				return nil
			})
			if err != nil {
				continue
			}
			input := string(request.Input)
			if len([]rune(input)) > 2000 {
				input = string([]rune(input)[:2000]) + "\n[Truncated — review the complete request in Relay before allowing]"
			}
			text := fmt.Sprintf("Permission needed · %s\nTool: %s\nFolder: %s\nSession: %s\n%s\nExpires: %s", host.Name, request.ToolName, request.Cwd, request.SessionID, input, request.ExpiresAt.Format(time.RFC3339))
			buttons := [][]map[string]string{{{"text": "Deny", "callback_data": "approval:" + ticket.ID + ":deny"}}}
			if ticket.CanAllow && len([]rune(string(request.Input))) <= 2000 {
				buttons[0] = append(buttons[0], map[string]string{"text": "Allow once", "callback_data": "approval:" + ticket.ID + ":allow"})
			}
			_ = s.sendTelegramOnce(record, key, text, map[string]any{"inline_keyboard": buttons})
		}
	}
}
func (s *Server) telegramDecision(record telegramRecord, id, decision string) string {
	var ticket controlApprovalTicket
	found := false
	for _, candidate := range s.telegram.snapshot().Tickets {
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
		return s.telegram.update(record.Epoch, func(next *telegramRecord) error {
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

func approvalFits(request controlApproval, host string, limit int) bool {
	return len([]rune(string(request.Input)+request.Cwd+request.ToolName+host))+300 <= limit
}

func (s *Server) applyControlDecision(ticket controlApprovalTicket, decision string, consume func() error) string {
	// The durable migration fence cannot overtake an approval already in flight.
	// A decision begun after the fence never consumes its ticket or reaches a runtime.
	if s.migration != nil {
		s.migration.mu.Lock()
		defer s.migration.mu.Unlock()
		if s.migration.record.SourceFrozen || s.migration.err != "" {
			return "Mastermind moved. Review approvals on its current controller; no decision was sent."
		}
	}
	if ticket.Used || !time.Now().Before(ticket.ExpiresAt) {
		return "This approval expired or was already handled. Check Relay."
	}
	if decision != "allow" && decision != "deny" {
		return "Choose allow or deny for this request."
	}
	if decision == "allow" && !ticket.CanAllow {
		return "Review the complete permission request in Relay before allowing it."
	}
	requests, err := s.controlApprovals(ticket.Host)
	if err != nil {
		return "Machine unavailable. No decision was sent."
	}
	valid := false
	for _, request := range requests {
		if request.ID == ticket.Request && request.SessionID == ticket.Session && request.SessionCreatedAt.Equal(ticket.CreatedAt) && request.Status == "pending" && time.Now().Before(request.ExpiresAt) {
			valid = true
		}
	}
	if !valid {
		return "This request is no longer pending. No decision was sent."
	}
	if err := consume(); err != nil {
		return "Decision already attempted or settings changed. Check Relay."
	}
	client, err := s.controlRuntimeClient(ticket.Host)
	if err != nil {
		return "Decision outcome unknown. Check Relay; it will not be repeated."
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	_, err = runtimeJSON(ctx, client, "POST", "/api/approvals/"+ticket.Request+"/decision", map[string]any{"sessionId": ticket.Session, "sessionCreatedAt": ticket.CreatedAt, "decision": decision})
	if err != nil {
		return "Decision outcome unknown. Check Relay; it will not be repeated."
	}
	return "Decision submitted to the provider."
}
