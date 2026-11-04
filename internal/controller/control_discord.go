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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type discordRecord struct {
	Format      int                     `json:"format"`
	Epoch       string                  `json:"epoch"`
	Token       string                  `json:"token"`
	Bot         string                  `json:"bot"`
	Owner       string                  `json:"owner"`
	Channel     string                  `json:"channel"`
	Paired      bool                    `json:"paired"`
	PairHash    string                  `json:"pairHash"`
	PairExpires time.Time               `json:"pairExpires"`
	Cursor      string                  `json:"cursor"`
	Sent        []string                `json:"sent"`
	Tickets     []controlApprovalTicket `json:"tickets"`
}
type discordStore struct {
	mu           sync.Mutex
	path         string
	record       discordRecord
	err          error
	status       string
	base         string
	client       *http.Client
	limitedUntil time.Time
}

var discordIDPattern = regexp.MustCompile(`^[1-9][0-9]{5,19}$`)

func validDiscordID(id string) bool {
	_, err := strconv.ParseUint(id, 10, 64)
	return discordIDPattern.MatchString(id) && err == nil
}

func openDiscordStore(dir string) *discordStore {
	s := &discordStore{path: filepath.Join(dir, "discord-private.json"), record: discordRecord{Format: 1}, base: "https://discord.com/api/v10", client: &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	data, err := readWorkspaceFile(s.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		err = strictWorkspaceJSON(data, &s.record)
	}
	if err == nil && (s.record.Format != 1 || len(s.record.Sent) > 512 || len(s.record.Tickets) > 100 || (s.record.Token != "" && (!validDiscordID(s.record.Owner) || !validDiscordID(s.record.Channel)))) {
		err = errors.New("invalid Discord state")
	}
	if err != nil {
		s.err = errors.New("Discord settings could not be read; preserve discord-private.json before recovery")
	}
	return s
}
func (s *discordStore) snapshot() discordRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.record
	r.Sent = append([]string{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	return r
}
func (s *discordStore) update(epoch string, change func(*discordRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if epoch != "" && s.record.Epoch != epoch {
		return errors.New("Discord connection changed")
	}
	r := s.record
	r.Sent = append([]string{}, r.Sent...)
	r.Tickets = append([]controlApprovalTicket{}, r.Tickets...)
	if err := change(&r); err != nil {
		return err
	}
	if err := saveControlFile(s.path, r); err != nil {
		s.err = errors.New("Discord save could not be confirmed. Reconnect Relay before changing it again.")
		return s.err
	}
	s.record = r
	return nil
}
func (s *discordStore) view() map[string]any {
	if s == nil {
		return map[string]any{"configured": false, "paired": false, "bot": "", "status": ""}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if s.err != nil {
		status = s.err.Error()
	}
	return map[string]any{"configured": s.record.Token != "", "paired": s.record.Paired, "bot": s.record.Bot, "status": status}
}
func (s *discordStore) setStatus(text string) { s.mu.Lock(); s.status = text; s.mu.Unlock() }
func (s *discordStore) call(ctx context.Context, token, method, path string, body, result any) error {
	s.mu.Lock()
	limited := time.Now().Before(s.limitedUntil)
	s.mu.Unlock()
	if limited {
		return errors.New("Discord requested a pause; Relay is respecting its rate limit")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("Could not encode Discord request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("Could not create Discord request")
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/TwoD97/relay, 0.4.0)")
	response, err := s.client.Do(req)
	if err != nil {
		return errors.New("Discord connection failed; delivery may be unknown")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(payload) > 1<<20 {
		return errors.New("Discord returned an incomplete response")
	}
	if response.StatusCode == 429 {
		var limit struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(payload, &limit)
		if limit.RetryAfter < 1 || limit.RetryAfter > 3600 {
			limit.RetryAfter = 30
		}
		s.mu.Lock()
		s.limitedUntil = time.Now().Add(time.Duration(limit.RetryAfter * float64(time.Second)))
		s.mu.Unlock()
		return errors.New("Discord rate limit reached; Relay will wait before polling again")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Discord rejected the request; check the bot and direct-message access")
	}
	if result != nil && json.Unmarshal(payload, result) != nil {
		return errors.New("Discord returned invalid data")
	}
	return nil
}
func (s *Server) configureDiscord(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  string `json:"token"`
		UserID string `json:"userId"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	body.UserID = strings.TrimSpace(body.UserID)
	if len(body.Token) < 30 || len(body.Token) > 256 || strings.ContainsAny(body.Token, "\r\n \t") || !validDiscordID(body.UserID) {
		writeError(w, 400, "Enter a bot token and your numeric Discord user ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var bot struct {
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	}
	if err := s.discord.call(ctx, body.Token, "GET", "/users/@me", nil, &bot); err != nil || !bot.Bot {
		writeError(w, 502, "Could not verify this Discord bot. Personal user tokens are not supported.")
		return
	}
	var channel struct {
		ID            string `json:"id"`
		Type          int    `json:"type"`
		LastMessageID string `json:"last_message_id"`
	}
	if err := s.discord.call(ctx, body.Token, "POST", "/users/@me/channels", map[string]string{"recipient_id": body.UserID}, &channel); err != nil || channel.Type != 1 || !validDiscordID(channel.ID) {
		writeError(w, 502, "Could not open your private bot conversation. Add the bot to a server you use and allow its direct messages.")
		return
	}
	if channel.LastMessageID != "" && !validDiscordID(channel.LastMessageID) {
		writeError(w, 502, "Discord returned an invalid direct-message identity")
		return
	}
	code := randomID()
	digest := sha256.Sum256([]byte(code))
	expires := time.Now().UTC().Add(10 * time.Minute)
	err := s.discord.update("", func(next *discordRecord) error {
		*next = discordRecord{Format: 1, Epoch: randomID(), Token: body.Token, Bot: bot.Username, Owner: body.UserID, Channel: channel.ID, PairHash: hex.EncodeToString(digest[:]), PairExpires: expires, Cursor: channel.LastMessageID}
		return nil
	})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.discord.setStatus("Waiting for private-message pairing")
	writeJSON(w, 200, map[string]any{"bot": bot.Username, "pairCommand": "!pair " + code, "expiresAt": expires})
}
func (s *Server) disconnectDiscord(w http.ResponseWriter, r *http.Request) {
	if err := s.discord.update("", func(next *discordRecord) error { *next = discordRecord{Format: 1, Epoch: randomID()}; return nil }); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.discord.setStatus("")
	s.controlState(w, r)
}

type discordMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	WebhookID string `json:"webhook_id"`
	Content   string `json:"content"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
}

func discordNumber(id string) uint64 { number, _ := strconv.ParseUint(id, 10, 64); return number }
func (s *Server) monitorDiscord() {
	defer s.controlWG.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.pollDiscord()
		}
	}
}
func (s *Server) pollDiscord() {
	if s.coordinatorDelegated() {
		return
	}
	record := s.discord.snapshot()
	if record.Token == "" {
		return
	}
	path := "/channels/" + record.Channel + "/messages?limit=100"
	if record.Cursor != "" {
		path += "&after=" + record.Cursor
	}
	var messages []discordMessage
	if err := s.discord.call(s.ctx, record.Token, "GET", path, nil, &messages); err != nil {
		s.discord.setStatus(err.Error())
		return
	}
	if len(messages) >= 100 {
		s.discord.setStatus("There are too many unread messages to replay safely. Review the chat, then disconnect and pair again to start from its latest message.")
		return
	}
	sort.Slice(messages, func(i, j int) bool { return discordNumber(messages[i].ID) < discordNumber(messages[j].ID) })
	for _, message := range messages {
		if !validDiscordID(message.ID) || discordNumber(message.ID) <= discordNumber(record.Cursor) {
			continue
		}
		current := s.discord.snapshot()
		if current.Epoch != record.Epoch || s.coordinatorDelegated() {
			return
		}
		if err := s.handleDiscord(current, message); err != nil {
			s.discord.setStatus(err.Error())
			return
		}
		if err := s.discord.update(record.Epoch, func(next *discordRecord) error { next.Cursor = message.ID; return nil }); err != nil {
			return
		}
	}
	current := s.discord.snapshot()
	if current.Epoch == record.Epoch && current.Paired {
		s.deliverDiscord(current)
	}
}
func (s *Server) discordSend(record discordRecord, text string) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	if s.discord.snapshot().Epoch != record.Epoch {
		return errors.New("Discord configuration changed")
	}
	if len([]rune(text)) > 1900 {
		text = string([]rune(text)[:1900]) + "\n… Open Relay for the full result."
	}
	return s.discord.call(s.ctx, record.Token, "POST", "/channels/"+record.Channel+"/messages", map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}, nil)
}
func (s *Server) sendDiscordOnce(record discordRecord, key, text string) error {
	err := s.discord.update(record.Epoch, func(next *discordRecord) error {
		for _, previous := range next.Sent {
			if previous == key {
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
	if err = s.discordSend(record, text); err != nil {
		s.discord.setStatus("A Discord delivery could not be confirmed. Review Relay; it will not be repeated automatically.")
	}
	return nil
}
func (s *Server) handleDiscord(record discordRecord, message discordMessage) error {
	if s.coordinatorDelegated() {
		return errors.New("Mastermind moved; configure messaging on its current controller")
	}
	if message.GuildID != "" || message.WebhookID != "" || message.Author.Bot || message.Author.ID != record.Owner || message.ChannelID != record.Channel {
		return nil
	}
	text := strings.TrimSpace(message.Content)
	if !record.Paired {
		if time.Now().After(record.PairExpires) || !strings.HasPrefix(text, "!pair ") {
			return nil
		}
		digest := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(text, "!pair "))))
		expected, err := hex.DecodeString(record.PairHash)
		if err != nil || subtle.ConstantTimeCompare(digest[:], expected) != 1 {
			return nil
		}
		if err := s.discord.update(record.Epoch, func(next *discordRecord) error {
			next.Paired = true
			next.PairHash = ""
			next.PairExpires = time.Time{}
			return nil
		}); err != nil {
			return err
		}
		s.discord.setStatus("Connected to your private bot chat")
		return s.sendDiscordOnce(s.discord.snapshot(), "pair:"+message.ID, "Relay is paired. Send instructions here. !status shows the inbox; !approvals lists pending permissions. Approval replies use !allow ID or !deny ID. Keep Relay running.")
	}
	if text == "" {
		return nil
	}
	key := "message:" + message.ID
	if text == "!status" {
		return s.sendDiscordOnce(record, key, s.remoteControlStatus())
	}
	if text == "!approvals" {
		s.sendDiscordApprovals(record)
		return s.sendDiscordOnce(record, key, "Current supported approval requests are shown above. Terminal-only prompts must be reviewed in Relay.")
	}
	if strings.HasPrefix(text, "!allow ") || strings.HasPrefix(text, "!deny ") {
		parts := strings.Fields(text)
		if len(parts) != 2 || !validMastermindID(parts[1]) {
			return s.sendDiscordOnce(record, key, "Use !allow ID or !deny ID from a current permission request.")
		}
		decision := "deny"
		if parts[0] == "!allow" {
			decision = "allow"
		}
		return s.sendDiscordOnce(record, key, s.discordDecision(record, parts[1], decision))
	}
	if strings.HasPrefix(text, "!") {
		return s.sendDiscordOnce(record, key, "Use plain text for instructions, !status, !approvals, !allow ID, or !deny ID.")
	}
	if !boundedControlText(text, 8000) {
		return s.sendDiscordOnce(record, key, "Keep an instruction under 8000 characters.")
	}
	digest := sha256.Sum256([]byte(record.Epoch + ":" + message.ID))
	s.discord.mu.Lock()
	if s.discord.record.Epoch != record.Epoch || !s.discord.record.Paired {
		s.discord.mu.Unlock()
		return errors.New("Discord connection changed; instruction was not queued")
	}
	_, enqueueErr := s.control.enqueue(hex.EncodeToString(digest[:16]), "discord", text, "discord:"+record.Epoch+":"+message.ID)
	s.discord.mu.Unlock()
	if err := enqueueErr; err != nil {
		return err
	}
	return s.sendDiscordOnce(record, key, "Queued for Mastermind. Use !status to check whether the coordinator is listening.")
}
func (s *controlStore) claimDelivery(id, source, prefix string) error {
	return s.update(func(next *controlRecord) error {
		for i := range next.Messages {
			item := &next.Messages[i]
			if item.ID == id {
				if item.Source != source || !strings.HasPrefix(item.Key, prefix) || item.Status != "completed" || item.Delivery != "" {
					return errTelegramSent
				}
				item.Delivery = "attempted"
				return nil
			}
		}
		return errors.New("Instruction no longer exists")
	})
}
func (s *controlStore) finishDelivery(id, outcome string) error {
	return s.update(func(next *controlRecord) error {
		for i := range next.Messages {
			if next.Messages[i].ID == id {
				next.Messages[i].Delivery = outcome
				return nil
			}
		}
		return nil
	})
}
func (s *Server) deliverDiscord(record discordRecord) {
	if s.coordinatorDelegated() {
		return
	}
	state, _, err := s.control.snapshot()
	if err != nil {
		return
	}
	prefix := "discord:" + record.Epoch + ":"
	for _, item := range state.Messages {
		if item.Source != "discord" || item.Status != "completed" || item.Delivery != "" || !strings.HasPrefix(item.Key, prefix) {
			continue
		}
		if s.control.claimDelivery(item.ID, "discord", prefix) != nil {
			continue
		}
		outcome := "sent"
		if s.discordSend(record, "Mastermind\n"+item.Reply) != nil {
			outcome = "unknown"
			s.discord.setStatus("A result delivery is unknown. Read it in Relay; it will not be repeated automatically.")
		}
		_ = s.control.finishDelivery(item.ID, outcome)
	}
	s.sendDiscordApprovals(record)
}
func (s *Server) sendDiscordApprovals(record discordRecord) {
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
			ticket := controlApprovalTicket{ID: randomID()[:32], Host: host.ID, Request: request.ID, Session: request.SessionID, CreatedAt: request.SessionCreatedAt, ExpiresAt: request.ExpiresAt, CanAllow: approvalFits(request, host.Name, 1800) && len([]rune(string(request.Input))) <= 1100}
			err = s.discord.update(record.Epoch, func(next *discordRecord) error {
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
					return errors.New("Too many pending Discord approvals")
				}
				next.Tickets = append(alive, ticket)
				return nil
			})
			if err != nil {
				continue
			}
			input := string(request.Input)
			full := len([]rune(input)) <= 1100
			if !full {
				input = string([]rune(input)[:1100]) + "\n[Truncated; inspect the full request in Relay]"
			}
			text := fmt.Sprintf("Permission needed · %s\nTool: %s\nFolder: %s\nSession: %s\n%s\nExpires: %s\nDeny: !deny %s", host.Name, request.ToolName, request.Cwd, request.SessionID, input, request.ExpiresAt.Format(time.RFC3339), ticket.ID)
			if full && ticket.CanAllow {
				text += "\nAllow once: !allow " + ticket.ID
			}
			_ = s.sendDiscordOnce(record, "approval:"+ticket.ID, text)
		}
	}
}
func (s *Server) discordDecision(record discordRecord, id, decision string) string {
	var ticket controlApprovalTicket
	found := false
	for _, candidate := range s.discord.snapshot().Tickets {
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
		return s.discord.update(record.Epoch, func(next *discordRecord) error {
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
