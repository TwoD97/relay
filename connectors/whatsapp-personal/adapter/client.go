// SPDX-License-Identifier: GPL-3.0-only
// Package adapter connects one personal WhatsApp linked device. It
// exposes only self-chat text commands, never contact/history or credential data.
package adapter

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type Message struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}
type State struct {
	Linked    bool   `json:"linked"`
	Connected bool   `json:"connected"`
	Problem   string `json:"problem,omitempty"`
}
type Client interface {
	State() State
	Connect(context.Context) error
	Pair(context.Context, string) (string, error)
	Send(context.Context, string, string) error
	Logout(context.Context) error
	Close() error
}
type Factory func(context.Context, string, func(Message)) (Client, error)

// PrepareDirectory protects connector bookkeeping before any credentials exist.
func PrepareDirectory(directory string) error { return privateDirectory(directory) }

// Forget removes only this adapter's known database files after it is closed.
// Unknown entries are preserved for inspection rather than recursively removed.
func Forget(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || linked(info) {
		return errors.New("Personal WhatsApp private storage could not be safely removed")
	}
	for _, name := range []string{"device.sqlite", "device.sqlite-wal", "device.sqlite-shm", "device.sqlite-journal"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || linked(info) {
			return errors.New("Personal WhatsApp private storage contains an unexpected file; it was preserved")
		}
		if err = os.Remove(path); err != nil {
			return errors.New("Personal WhatsApp private storage could not be completely removed")
		}
	}
	if err = os.Remove(directory); err != nil {
		return errors.New("Personal WhatsApp private storage contains other files; they were preserved")
	}
	return nil
}

type client struct {
	op               sync.Mutex
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	connectionCancel context.CancelFunc
	wa               *whatsmeow.Client
	store            *sqlstore.Container
	state            State
	ready            chan struct{}
	closed           bool
}

// Open touches only the specified private directory and performs no network
// operation. Controllers invoke Connect/Pair only after explicit configuration.
func Open(parent context.Context, directory string, onMessage func(Message)) (Client, error) {
	path, err := prepareDatabase(directory)
	if err != nil {
		return nil, errors.New("Personal WhatsApp storage is unavailable; preserve its private directory")
	}
	filePath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		filePath = "/" + filePath
	}
	location := url.URL{Scheme: "file", Path: filePath}
	query := location.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	location.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, errors.New("Personal WhatsApp database could not be opened")
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(parent)
	setup, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err = container.Upgrade(setup); err == nil {
		_, err = db.ExecContext(setup, "PRAGMA max_page_count=32768")
	}
	if err != nil {
		cancel()
		container.Close()
		return nil, errors.New("Personal WhatsApp database could not be initialized; existing data was preserved")
	}
	devices, err := container.GetAllDevices(setup)
	if err != nil || len(devices) > 1 {
		cancel()
		container.Close()
		return nil, errors.New("Personal WhatsApp must contain exactly one linked account at most")
	}
	device := container.NewDevice()
	if len(devices) == 1 {
		device = devices[0]
	}
	wa := whatsmeow.NewClient(device, waLog.Noop)
	wa.BackgroundEventCtx = ctx
	wa.EnableAutoReconnect = false // the controller reconnects between sends, never during an uncertain send
	wa.InitialAutoReconnect = false
	wa.AutoTrustIdentity = false
	wa.ManualHistorySyncDownload = true
	wa.DisableManualHistorySyncReceipt = true
	wa.AutomaticMessageRerequestFromPhone = false
	wa.UseRetryMessageStore = false
	wa.PreRetryCallback = func(*events.Receipt, types.MessageID, int, *waE2E.Message) bool { return false }
	c := &client{ctx: ctx, cancel: cancel, wa: wa, store: container, state: State{Linked: !device.GetJID().IsEmpty()}, ready: make(chan struct{}, 1)}
	wa.AddEventHandler(func(event any) {
		switch event := event.(type) {
		case *events.QR:
			select {
			case c.ready <- struct{}{}:
			default:
			}
		case *events.Connected:
			c.mu.Lock()
			c.state = State{Linked: true, Connected: true}
			c.mu.Unlock()
		case *events.Disconnected:
			c.mu.Lock()
			c.state.Connected = false
			c.mu.Unlock()
		case *events.PairSuccess:
			c.mu.Lock()
			c.state.Linked = true
			c.state.Problem = ""
			c.mu.Unlock()
		case *events.LoggedOut:
			c.mu.Lock()
			c.state = State{Problem: "The personal WhatsApp link was removed. Disconnect it in Relay, then pair again."}
			c.mu.Unlock()
		case events.PermanentDisconnect:
			c.mu.Lock()
			c.state.Connected = false
			c.state.Problem = "WhatsApp refused this linked-device connection. Review Linked devices on your phone before reconnecting."
			c.mu.Unlock()
		case *events.Message:
			if message, ok := selfCommand(event, device.GetJID(), device.GetLID()); ok && onMessage != nil {
				onMessage(message)
			}
		}
	})
	return c, nil
}
func (c *client) State() State { c.mu.Lock(); defer c.mu.Unlock(); return c.state }
func (c *client) Connect(ctx context.Context) error {
	c.op.Lock()
	defer c.op.Unlock()
	return c.connectLocked(ctx)
}
func (c *client) connectLocked(ctx context.Context) error {
	if c.closed {
		return errors.New("Personal WhatsApp is closed")
	}
	if c.wa.IsConnected() {
		return nil
	}
	if c.connectionCancel != nil {
		c.connectionCancel()
	}
	connection, cancel := context.WithCancel(c.ctx)
	c.connectionCancel = cancel
	// The provider keeps this connection context for its socket/read loop. Stop
	// forwarding the request deadline after the handshake, rather than giving
	// a persistent socket the short HTTP request lifetime or leaking a dial task.
	stopDeadline := context.AfterFunc(ctx, cancel)
	err := c.wa.ConnectContext(connection)
	stopped := stopDeadline()
	if err != nil || !stopped || ctx.Err() != nil || c.ctx.Err() != nil {
		cancel()
		c.wa.Disconnect()
		return errors.New("Personal WhatsApp connection failed or timed out; no command was sent")
	}
	return nil
}
func (c *client) Pair(ctx context.Context, phone string) (string, error) {
	c.op.Lock()
	defer c.op.Unlock()
	if c.State().Linked {
		return "", errors.New("Personal WhatsApp is already linked")
	}
	if err := c.connectLocked(ctx); err != nil {
		return "", err
	}
	select {
	case <-c.ready:
	case <-ctx.Done():
		return "", errors.New("Personal WhatsApp pairing connection timed out")
	case <-c.ctx.Done():
		return "", errors.New("Personal WhatsApp is stopping")
	}
	code, err := c.wa.PairPhone(ctx, phone, false, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return "", errors.New("WhatsApp could not create a phone-link code. Check the international number and try a new pairing from Relay.")
	}
	return code, nil
}
func (c *client) Send(ctx context.Context, id, text string) error {
	c.op.Lock()
	defer c.op.Unlock()
	if c.closed || !c.wa.IsLoggedIn() {
		return errors.New("Personal WhatsApp is not connected")
	}
	if !ValidMessageID(id) || !utf8.ValidString(text) || len([]rune(text)) > 3800 {
		return errors.New("Invalid personal WhatsApp output")
	}
	_, err := c.wa.SendMessage(ctx, c.wa.Store.GetJID().ToNonAD(), &waE2E.Message{Conversation: proto.String("Relay\n" + text)}, whatsmeow.SendRequestExtra{ID: id, Timeout: 15 * time.Second})
	if err != nil {
		return errors.New("Personal WhatsApp delivery is unknown; it will not be repeated automatically")
	}
	return nil
}
func (c *client) Logout(ctx context.Context) error {
	c.op.Lock()
	defer c.op.Unlock()
	if c.closed {
		return nil
	}
	if !c.State().Linked {
		return nil
	}
	if err := c.wa.Logout(ctx); err != nil {
		return errors.New("Relay stopped this connector, but WhatsApp unlinking could not be confirmed. Remove Relay from Linked devices on your phone.")
	}
	return nil
}
func (c *client) Close() error {
	c.op.Lock()
	defer c.op.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.cancel()
	if c.connectionCancel != nil {
		c.connectionCancel()
	}
	c.wa.RemoveEventHandlers()
	c.wa.Disconnect()
	return c.store.Close()
}
func ValidMessageID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && strings.IndexFunc(id, func(r rune) bool {
		return r > 127 || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_')
	}) < 0
}
func selfCommand(event *events.Message, own, ownLID types.JID) (Message, bool) {
	if event == nil || event.Message == nil || !event.Info.IsFromMe || event.Info.IsGroup || event.IsEdit || event.IsViewOnce || event.IsBotInvoke || event.SourceWebMsg != nil || !ValidMessageID(event.Info.ID) {
		return Message{}, false
	}
	chat := event.Info.Chat.ToNonAD()
	if (own.IsEmpty() || chat != own.ToNonAD()) && (ownLID.IsEmpty() || chat != ownLID.ToNonAD()) {
		return Message{}, false
	}
	text := event.Message.GetConversation()
	if text == "" {
		text = event.Message.GetExtendedTextMessage().GetText()
	}
	if !strings.HasPrefix(text, "!relay ") || !utf8.ValidString(text) || len([]rune(text)) > 8010 || strings.ContainsRune(text, 0) {
		return Message{}, false
	}
	return Message{ID: event.Info.ID, Text: strings.TrimSpace(strings.TrimPrefix(text, "!relay ")), At: event.Info.Timestamp.UTC()}, true
}
func prepareDatabase(directory string) (string, error) {
	if err := privateDirectory(directory); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "device.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || linked(info) {
			return "", errors.New("Personal WhatsApp storage contains a linked or non-regular file")
		}
		if err = os.Chmod(path+suffix, 0600); err != nil {
			return "", err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	return path, err
}
