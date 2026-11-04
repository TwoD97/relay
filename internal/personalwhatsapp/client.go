// Package personalwhatsapp speaks a bounded, implementation-independent stdio
// protocol to an optional, separately licensed linked-device connector.
package personalwhatsapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxFrame = 128 << 10

type Message struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}
type State struct {
	Linked    bool   `json:"linked"`
	Connected bool   `json:"connected"`
	Problem   string `json:"problem,omitempty"`
	Stopped   bool   `json:"-"`
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
type request struct {
	ID        uint64 `json:"id"`
	Method    string `json:"method"`
	Phone     string `json:"phone,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"text,omitempty"`
}
type frame struct {
	Kind    string   `json:"kind"`
	ID      uint64   `json:"id,omitempty"`
	State   *State   `json:"state,omitempty"`
	Code    string   `json:"code,omitempty"`
	Message *Message `json:"message,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func Open(ctx context.Context, directory string, onMessage func(Message)) (Client, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("Optional WhatsApp connector location is unavailable")
	}
	return OpenWithBundle(ctx, directory, filepath.Dir(executable), onMessage)
}

// OpenWithBundle verifies and privately stages the optional executable before
// starting it. It does not connect to WhatsApp or read a personal account.
func OpenWithBundle(ctx context.Context, directory, bundle string, onMessage func(Message)) (Client, error) {
	if err := privateDirectory(directory); err != nil {
		return nil, errors.New("Personal WhatsApp private storage is unavailable")
	}
	path, err := stageHelper(ctx, bundle, filepath.Join(filepath.Dir(directory), "helper-releases"))
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, "--stdio", "--state-dir", absolute)
	cmd.Env = connectorEnvironment(os.Environ())
	configureProcess(cmd)
	return startClient(ctx, cmd, onMessage)
}

// Pass operating-system and networking settings, not inherited agent tokens,
// provider API keys, injected runtimes or controller capability paths.
func connectorEnvironment(entries []string) []string {
	allowed := map[string]bool{"PATH": true, "SYSTEMROOT": true, "WINDIR": true, "TEMP": true, "TMP": true, "TMPDIR": true, "HOME": true, "USERPROFILE": true, "LOCALAPPDATA": true, "APPDATA": true, "LANG": true, "LC_ALL": true, "TZ": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	result := make([]string, 0, len(allowed))
	for _, entry := range entries {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allowed[strings.ToUpper(key)] {
			result = append(result, entry)
		}
	}
	return result
}

type client struct {
	op        chan struct{}
	mu        sync.Mutex
	state     State
	cmd       *exec.Cmd
	input     io.WriteCloser
	output    io.ReadCloser
	responses chan frame
	done      chan struct{}
	killOnce  sync.Once
	next      uint64
}

func startClient(ctx context.Context, cmd *exec.Cmd, onMessage func(Message)) (Client, error) {
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, errors.New("Optional WhatsApp connector could not start")
	}
	c := &client{op: make(chan struct{}, 1), cmd: cmd, input: input, output: output, responses: make(chan frame, 1), done: make(chan struct{})}
	go c.read(onMessage)
	go func() {
		select {
		case <-ctx.Done():
			c.stop()
		case <-c.done:
		}
	}()
	setup, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if _, err = c.call(setup, request{Method: "state"}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *client) stop() {
	c.killOnce.Do(func() { c.input.Close(); _ = c.cmd.Process.Kill(); c.output.Close() })
}
func (c *client) read(onMessage func(Message)) {
	defer func() {
		c.stop()
		_ = c.cmd.Wait()
		c.mu.Lock()
		c.state.Connected = false
		c.state.Stopped = true
		c.mu.Unlock()
		close(c.done)
	}()
	scanner := bufio.NewScanner(c.output)
	scanner.Buffer(make([]byte, 4096), maxFrame)
	for scanner.Scan() {
		if !utf8.Valid(scanner.Bytes()) {
			return
		}
		var value frame
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
			return
		}
		if value.State != nil {
			if len(value.State.Problem) > 1024 || !utf8.ValidString(value.State.Problem) {
				return
			}
			c.mu.Lock()
			c.state = *value.State
			c.mu.Unlock()
		}
		switch value.Kind {
		case "response":
			if value.ID == 0 || len(value.Error) > 2048 || len(value.Code) > 32 {
				return
			}
			select {
			case c.responses <- value:
			default:
				return
			}
		case "state":
			if value.State == nil || value.ID != 0 {
				return
			}
		case "message":
			m := value.Message
			if m == nil || !ValidMessageID(m.ID) || len(m.Text) > 40000 || !utf8.ValidString(m.Text) || strings.ContainsRune(m.Text, 0) || m.At.IsZero() {
				return
			}
			if onMessage != nil {
				onMessage(*m)
			}
		default:
			return
		}
	}
}
func (c *client) call(parent context.Context, req request) (frame, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	select {
	case c.op <- struct{}{}:
	case <-ctx.Done():
		return frame{}, errors.New("WhatsApp connector operation was not started")
	case <-c.done:
		return frame{}, errors.New("WhatsApp connector stopped")
	}
	defer func() { <-c.op }()
	select {
	case <-c.done:
		return frame{}, errors.New("WhatsApp connector stopped")
	default:
	}
	c.next++
	req.ID = c.next
	data, err := json.Marshal(req)
	if err != nil || len(data) >= maxFrame {
		return frame{}, errors.New("WhatsApp connector request exceeds its limit")
	}
	data = append(data, '\n')
	written := make(chan error, 1)
	go func() { _, err := c.input.Write(data); written <- err }()
	unknown := func() (frame, error) {
		c.stop()
		return frame{}, errors.New("WhatsApp connector outcome could not be confirmed; this operation will not be repeated automatically")
	}
	select {
	case err = <-written:
		if err != nil {
			return unknown()
		}
	case <-ctx.Done():
		return unknown()
	case <-c.done:
		return unknown()
	}
	select {
	case response := <-c.responses:
		if response.ID != req.ID {
			return unknown()
		}
		if response.Error != "" {
			return frame{}, errors.New("WhatsApp connector rejected this operation; check the link in Relay")
		}
		return response, nil
	case <-ctx.Done():
		return unknown()
	case <-c.done:
		return unknown()
	}
}
func (c *client) State() State { c.mu.Lock(); defer c.mu.Unlock(); return c.state }
func (c *client) Connect(ctx context.Context) error {
	_, err := c.call(ctx, request{Method: "connect"})
	return err
}
func (c *client) Pair(ctx context.Context, phone string) (string, error) {
	response, err := c.call(ctx, request{Method: "pair", Phone: phone})
	return response.Code, err
}
func (c *client) Send(ctx context.Context, id, text string) error {
	if !ValidMessageID(id) || utf8.RuneCountInString(text) > 3800 || !utf8.ValidString(text) {
		return errors.New("Invalid WhatsApp message")
	}
	_, err := c.call(ctx, request{Method: "send", MessageID: id, Text: text})
	return err
}
func (c *client) Logout(ctx context.Context) error {
	_, err := c.call(ctx, request{Method: "logout"})
	return err
}
func (c *client) Close() error {
	c.stop()
	select {
	case <-c.done:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("WhatsApp connector shutdown could not be confirmed")
	}
}
func ValidMessageID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func PrepareDirectory(directory string) error { return privateDirectory(directory) }
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
			return errors.New("Personal WhatsApp storage contains an unexpected file; it was preserved")
		}
		if err = os.Remove(path); err != nil {
			return errors.New("Personal WhatsApp storage could not be completely removed")
		}
	}
	if err = os.Remove(directory); err != nil {
		return errors.New("Personal WhatsApp storage contains other files; they were preserved")
	}
	return nil
}
