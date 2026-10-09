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

// sessionCommand gives agents and scripts the same local runtime API as the UI.
// It does not bypass provider permissions or replay an uncertain mutation.
func sessionCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: relay session list|start|read|send|stop [options]")
	}
	action := args[0]
	f := flag.NewFlagSet("session "+action, flag.ContinueOnError)
	defaultSocket := os.Getenv("RELAY_SOCKET")
	if defaultSocket == "" {
		defaultSocket = socketPath(defaultDir("relay"))
	}
	socket := f.String("socket", defaultSocket, "Runtime Unix socket (defaults to the owning session's runtime)")
	sshHost := f.String("ssh", "", "Use an already trusted SSH alias or user@host with existing key authentication")
	sshPort := f.Int("ssh-port", 0, "SSH port (zero respects SSH configuration)")
	id := f.String("id", "", "Session ID for read, send, or stop")
	harness := f.String("harness", "shell", "shell, claude, or codex")
	cwd := f.String("cwd", "~", "Existing project directory")
	workspace := f.String("workspace", "Default", "Workspace name")
	title := f.String("title", "Terminal", "Session title")
	text := f.String("text", "", "Text to send (sent literally)")
	enter := f.Bool("enter", false, "Append carriage return after text")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments; use --id and --text")
	}
	if err := validateSessionSSH(*sshHost, *sshPort); err != nil {
		return err
	}
	if *sshHost == "" && !localRuntimeSupported {
		return errors.New("local sessions require Linux; use --ssh to control a Linux host from Windows")
	}
	if *sshHost != "" {
		var explicitSocket bool
		f.Visit(func(option *flag.Flag) { explicitSocket = explicitSocket || option.Name == "socket" })
		if explicitSocket {
			return errors.New("--ssh uses the remote user's Relay runtime; do not combine it with --socket")
		}
	}
	method, path := "GET", "/api/sessions"
	var body any
	switch action {
	case "list":
	case "start":
		method = "POST"
		body = map[string]string{"harness": *harness, "cwd": *cwd, "workspace": *workspace, "title": *title}
	case "read", "send", "stop":
		decoded, err := hex.DecodeString(*id)
		if err != nil || len(decoded) != 16 {
			return errors.New("a valid 32-character session --id is required")
		}
		path += "/" + *id
		switch action {
		case "read":
			path += "/history"
		case "stop":
			method = "DELETE"
		case "send":
			method = "POST"
			path += "/input"
			data := *text
			if *enter {
				data += "\r"
			}
			if data == "" {
				return errors.New("send requires --text or --enter")
			}
			if len(data) > 64<<10 {
				return errors.New("terminal input must not exceed 64 KiB")
			}
			body = map[string]string{"data": data}
		}
	default:
		return fmt.Errorf("unknown session action %q", action)
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	timeout := 15 * time.Second
	if *sshHost != "" {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if *sshHost != "" {
			return dialSessionSSH(ctx, *sshHost, *sshPort)
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
	}, DisableKeepAlives: true}
	defer t.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://runtime"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.GetBody = nil
	resp, err := (&http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return fmt.Errorf("runtime request failed (not retried); run relay ensure if it is stopped: %w", err)
	}
	defer resp.Body.Close()
	output, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(output) > 2<<20 {
		return errors.New("runtime response exceeded 2 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var response struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(output, &response)
		if response.Error != "" {
			return errors.New(response.Error)
		}
		return fmt.Errorf("runtime returned HTTP %d", resp.StatusCode)
	}
	_, err = os.Stdout.Write(output)
	return err
}
