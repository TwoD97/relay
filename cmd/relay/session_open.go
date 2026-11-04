package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/TwoD97/relay/internal/viewrequests"
)

func sessionOpen(id, host, socket, sshHost string, sshPort int, useController bool) error {
	if !viewrequests.ValidID(id) {
		return errors.New("A valid lowercase session --id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var client *http.Client
	var transport *http.Transport
	if !useController {
		transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if sshHost != "" {
				return dialSessionSSH(ctx, sshHost, sshPort)
			}
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}, DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	request := func(method, path string, body any) ([]byte, error) {
		if useController {
			return agentRequest(method, path, body)
		}
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, "http://runtime"+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.GetBody = nil
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Terminal window request failed and was not retried: %w", err)
		}
		defer res.Body.Close()
		out, err := io.ReadAll(io.LimitReader(res.Body, (512<<10)+1))
		if err != nil || len(out) > 512<<10 {
			return nil, errors.New("Could not confirm the terminal window request; do not repeat an unknown result")
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			var failure struct {
				Error string `json:"error"`
			}
			json.Unmarshal(out, &failure)
			if failure.Error != "" {
				return nil, errors.New(failure.Error)
			}
			return nil, fmt.Errorf("Terminal window requests require a supported runtime (HTTP %d)", res.StatusCode)
		}
		return out, nil
	}
	path := "/api/sessions"
	if useController {
		if host == "" {
			return errors.New("Inside Mastermind, supply --host from relay fleet list")
		}
		var err error
		host, err = agentHost(host)
		if err != nil {
			return err
		}
		path = "/agent/hosts/" + url.PathEscape(host) + "/sessions"
	}
	data, err := request("GET", path, nil)
	if err != nil {
		return err
	}
	var sessions []struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
		Purpose   string    `json:"purpose"`
	}
	if json.Unmarshal(data, &sessions) != nil {
		return errors.New("Invalid session list; no window was requested")
	}
	var ref viewrequests.Ref
	for _, session := range sessions {
		if session.ID == id && session.Purpose == "" {
			ref = viewrequests.Ref{SessionID: id, SessionCreatedAt: session.CreatedAt}
			break
		}
	}
	if !viewrequests.ValidRef(ref) {
		return errors.New("The session is missing or is a setup session; no window was requested")
	}
	path = "/api/views"
	var body any = ref
	if useController {
		path = "/agent/views"
		body = struct {
			HostID string `json:"hostId"`
			viewrequests.Ref
		}{host, ref}
	}
	data, err = request("POST", path, body)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}
