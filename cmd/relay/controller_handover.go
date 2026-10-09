package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The health response and shutdown request use the same private connection.
// Its OS process handle stays pinned until the old controller has exited.
type controllerProbe struct {
	health  controllerHealth
	conn    net.Conn
	reader  *bufio.Reader
	process *controllerProcess
}

func (p *controllerProbe) Close() {
	_ = p.conn.Close()
	p.process.Close()
}

func (p *controllerProbe) request(ctx context.Context, method, path string, body, output any) error {
	deadline := time.Now().Add(2 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := p.conn.SetDeadline(deadline); err != nil {
		return err
	}
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay-control"+path, input)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := req.Write(p.conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(p.reader, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller %s returned HTTP %d", path, resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<10))
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("invalid extra controller response data")
	}
	return nil
}

func probeController(ctx context.Context, opts uiOptions) (*controllerProbe, error) {
	conn, err := dialControllerControl(ctx, opts.StateDir)
	if err != nil {
		return nil, err
	}
	process, err := pinControllerProcess(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	p := &controllerProbe{conn: conn, reader: bufio.NewReader(conn), process: process}
	if err = p.request(ctx, http.MethodGet, "/health", nil, &p.health); err == nil {
		err = validateControllerIdentity(p.health.controllerInfo)
	}
	if err == nil && p.health.PID != process.PID {
		err = errors.New("private controller peer does not match its reported PID")
	}
	if err == nil {
		err = validateControllerConfiguration(p.health, opts)
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func validateControllerConfiguration(health controllerHealth, opts uiOptions) error {
	if health.Local != opts.Local || (opts.Local && !sameControllerPath(health.RuntimeDir, opts.RuntimeDir)) {
		return errors.New("running controller uses different --local or --runtime-dir settings; use matching settings or a separate --state-dir")
	}
	if health.StateDir != "" && !sameControllerPath(health.StateDir, opts.StateDir) {
		return errors.New("running controller reports a different state directory")
	}
	return nil
}

func validateControllerArguments(args []string, health controllerHealth, opts uiOptions) error {
	if len(args) < 3 || args[1] != "ui" {
		return errors.New("private peer is not a Relay UI controller")
	}
	// A default state path is not sufficient evidence for a destructive action.
	explicitState := false
	for _, arg := range args[2:] {
		if arg == "--state-dir" || strings.HasPrefix(arg, "--state-dir=") {
			explicitState = true
		}
	}
	if !explicitState {
		return errors.New("controller command line has no explicit state directory")
	}
	actual, err := parseUIOptions("ui", args[2:])
	if err != nil {
		return errors.New("controller command line is not a recognized Relay launch")
	}
	if !sameControllerPath(actual.StateDir, opts.StateDir) || actual.Local != health.Local ||
		!sameControllerPath(actual.RuntimeDir, health.RuntimeDir) {
		return errors.New("controller command line does not match its private state and runtime configuration")
	}
	return nil
}

type versionOutput struct{ data []byte }

func (b *versionOutput) Write(data []byte) (int, error) {
	if len(b.data)+len(data) > 256 {
		return 0, errors.New("oversized controller version")
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func verifyControllerVersion(ctx context.Context, executable, expected string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "version")
	detachController(command)
	command.WaitDelay = 250 * time.Millisecond
	var output versionOutput
	command.Stdout = &output
	if err := command.Run(); err != nil || strings.TrimSpace(string(output.data)) != expected {
		return errors.New("controller executable version does not match its private identity")
	}
	return nil
}

// Verify the immutable release key, including all remote payloads, before a
// legacy process without the cooperative endpoint may be terminated.
func verifyControllerRelease(executable, stateDir string) error {
	release := filepath.Dir(executable)
	if filepath.Base(executable) != stagedControllerFilename || !sameControllerPath(filepath.Dir(release), filepath.Join(stateDir, "controller-releases")) {
		return errors.New("controller is not running from a verified private release")
	}
	if err := privateControllerDirectory(filepath.Dir(release)); err != nil {
		return err
	}
	if err := privateControllerDirectory(release); err != nil {
		return err
	}
	digest, err := stageHashFile(executable)
	if err != nil {
		return err
	}
	manifest, digests, err := stageManifest(release)
	if errors.Is(err, os.ErrNotExist) {
		manifest, digests, err = nil, nil, nil
	}
	if err != nil {
		return err
	}
	manifestHash := sha256.Sum256(manifest)
	key := sha256.Sum256([]byte(digest + ":" + hex.EncodeToString(manifestHash[:])))
	if filepath.Base(release) != hex.EncodeToString(key[:]) {
		return errors.New("controller private release hash mismatch")
	}
	for name, expected := range digests {
		actual, err := stageHashFile(filepath.Join(release, name))
		if err != nil {
			return err
		}
		if actual != expected {
			return errors.New("controller private runtime checksum mismatch")
		}
	}
	return nil
}

func acquireHandoverLock(ctx context.Context, dir string) (*os.File, error) {
	for {
		lock, err := lockControllerFile(dir, "handover.lock")
		if !errors.Is(err, errControllerRunning) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func upgradeController(ctx context.Context, opts uiOptions, observed *controllerProbe) (desktopLaunch, error) {
	// Prepare and validate the candidate before disrupting any connected view.
	staged, err := stageController(opts)
	if err != nil {
		return desktopLaunch{}, err
	}
	if err := verifyControllerVersion(ctx, staged.Executable, version); err != nil {
		return desktopLaunch{}, err
	}
	lock, err := acquireHandoverLock(ctx, opts.StateDir)
	if err != nil {
		return desktopLaunch{}, err
	}
	defer lock.Close()
	current, err := probeController(ctx, opts)
	if err != nil {
		if controllerUnavailable(err) {
			staged.Address = strings.TrimPrefix(observed.health.Address, "http://")
			return startController(ctx, staged)
		}
		return desktopLaunch{}, err
	}
	defer current.Close()
	if current.health.Version == version {
		return issueControllerLaunch(ctx, opts, current.health.controllerInfo)
	}
	if current.health.controllerInfo != observed.health.controllerInfo || current.health.Instance != observed.health.Instance {
		return desktopLaunch{}, errors.New("another controller version won the upgrade; reopen its matching desktop")
	}
	if err := current.process.Verify(ctx, current.health, opts); err != nil {
		return desktopLaunch{}, err
	}
	if current.health.Handover == 1 {
		if current.health.Instance == "" || current.health.StateDir == "" {
			return desktopLaunch{}, errors.New("controller handover identity is incomplete")
		}
		var ack struct {
			Accepted bool `json:"accepted"`
		}
		err = current.request(ctx, http.MethodPost, "/handover", controllerHandoverRequest{
			PID: current.health.PID, Version: current.health.Version, Instance: current.health.Instance,
		}, &ack)
		// Even on a lost response the stop is never repeated or escalated.
		if err == nil && !ack.Accepted {
			return desktopLaunch{}, errors.New("controller did not accept handover")
		}
	} else if current.health.Handover == 0 && current.health.Instance == "" {
		if err := current.process.StopLegacy(opts.StateDir); err != nil {
			return desktopLaunch{}, err
		}
	} else {
		return desktopLaunch{}, errors.New("controller handover protocol is unsupported")
	}
	if waitErr := current.process.Wait(ctx); waitErr != nil {
		if err != nil {
			return desktopLaunch{}, fmt.Errorf("controller handover was not confirmed; no stop retry was attempted: %w", err)
		}
		return desktopLaunch{}, fmt.Errorf("controller is still shutting down; reconnect to retry: %w", waitErr)
	}
	// Retain the old origin so existing tabs can authenticate again. Runtime
	// sockets and remote daemon processes are independent of this controller.
	staged.Address = strings.TrimPrefix(current.health.Address, "http://")
	return startController(ctx, staged)
}

type controllerHandoverRequest struct {
	PID      int    `json:"pid"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
}
