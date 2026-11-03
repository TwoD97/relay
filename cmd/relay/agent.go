package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func agentBridgeConfigured() bool {
	return os.Getenv("RELAY_CONTROLLER_STATE") != "" || os.Getenv("RELAY_AGENT_CAPABILITY_FILE") != ""
}

func agentCredential() (stateDir, token string, err error) {
	stateDir = os.Getenv("RELAY_CONTROLLER_STATE")
	path := os.Getenv("RELAY_AGENT_CAPABILITY_FILE")
	if !filepath.IsAbs(stateDir) || !filepath.IsAbs(path) || filepath.Clean(filepath.Dir(path)) != filepath.Join(filepath.Clean(stateDir), "agent-capabilities") {
		return "", "", errors.New("This command requires the private controller connection supplied to Relay Mastermind")
	}
	f, err := openAgentRead(path, true)
	if err != nil {
		return "", "", errors.New("The private Mastermind capability is unavailable; reconnect in Relay")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 4096 {
		return "", "", errors.New("The Mastermind capability is invalid")
	}
	var capability struct {
		Format   int    `json:"format"`
		LaunchID string `json:"launchId"`
		Token    string `json:"token"`
	}
	d := json.NewDecoder(io.LimitReader(f, 4097))
	d.DisallowUnknownFields()
	if d.Decode(&capability) != nil || capability.Format != 1 || !agentID(capability.LaunchID) || filepath.Base(path) != capability.LaunchID+".json" {
		return "", "", errors.New("The Mastermind capability is invalid")
	}
	var extra any
	secret, decodeErr := hex.DecodeString(capability.Token)
	if d.Decode(&extra) != io.EOF || decodeErr != nil || len(secret) != 32 || hex.EncodeToString(secret) != capability.Token {
		return "", "", errors.New("The Mastermind capability is invalid")
	}
	return stateDir, capability.Token, nil
}

func agentID(id string) bool {
	value, err := hex.DecodeString(id)
	return err == nil && len(value) == 16 && hex.EncodeToString(value) == id
}

func agentRequest(method, path string, body any) ([]byte, error) {
	stateDir, token, err := agentCredential()
	if err != nil {
		return nil, err
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialControllerControl(ctx, stateDir) }}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://relay-controller"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Controller request failed and was not retried; inspect the current state before repeating a change")
	}
	defer response.Body.Close()
	output, err := io.ReadAll(io.LimitReader(response.Body, (5<<20)+1))
	if err != nil || len(output) > 5<<20 {
		return nil, errors.New("Controller response was incomplete or too large; changes were not retried")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(output, &failure) == nil && failure.Error != "" {
			return nil, errors.New(failure.Error)
		}
		return nil, fmt.Errorf("Controller returned HTTP %d", response.StatusCode)
	}
	return output, nil
}

func agentHost(name string) (string, error) {
	data, err := agentRequest("GET", "/agent/fleet", nil)
	if err != nil {
		return "", err
	}
	var fleet struct {
		Hosts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"hosts"`
	}
	if json.Unmarshal(data, &fleet) != nil {
		return "", errors.New("Could not read the connected fleet")
	}
	var matches []string
	for _, host := range fleet.Hosts {
		if host.ID == name || host.Name == name {
			matches = append(matches, host.ID)
		}
	}
	if len(matches) != 1 {
		return "", errors.New("Use one exact, unambiguous machine name or ID from relay fleet list")
	}
	if strings.ContainsAny(matches[0], "/?#%\\") {
		return "", errors.New("The controller returned an invalid machine identity")
	}
	return matches[0], nil
}
