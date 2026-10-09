package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// notifyRuntime is invoked by provider hooks in a session, not by the browser.
// Provider event bodies may contain sensitive prompts/commands; intentionally
// send only the explicitly configured kind and source to the runtime.
func notifyRuntime(args []string) error {
	f := flag.NewFlagSet("notify", flag.ContinueOnError)
	source := f.String("source", "", "claude-hook or codex-notify")
	kind := f.String("kind", "", "working, completed, notification, or permission")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *source != "claude-hook" && *source != "codex-notify" {
		return errors.New("unknown event source")
	}
	switch *kind {
	case "working", "completed", "notification", "permission":
	default:
		return errors.New("unknown event kind")
	}
	socket := os.Getenv("RELAY_SOCKET")
	session := os.Getenv("RELAY_SESSION_ID")
	token := os.Getenv("RELAY_EVENT_TOKEN")
	id, err := hex.DecodeString(session)
	if err != nil || len(id) != 16 || socket == "" || token == "" {
		return errors.New("notify must run inside a Relay-managed agent session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer t.CloseIdleConnections()
	payload, _ := json.Marshal(map[string]string{"kind": *kind, "source": *source})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://runtime/api/sessions/"+session+"/events", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: t}).Do(req)
	if err != nil {
		return errors.New("runtime notification could not be delivered")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("runtime notification rejected (HTTP %d)", resp.StatusCode)
	}
	return nil
}
