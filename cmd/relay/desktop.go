package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/TwoD97/relay/internal/controller"
)

const controllerProtocol = 1

var errControllerRunning = errors.New("a controller already owns this data directory")

type uiOptions struct {
	Address, StateDir, RuntimeDir, BinaryDir, Executable string
	Local, NoLoginLink                                   bool
}

type controllerInfo struct {
	Address  string `json:"address"`
	Version  string `json:"version"`
	PID      int    `json:"pid"`
	Protocol int    `json:"protocol"`
}

type desktopLaunch struct {
	controllerInfo
	URL string `json:"url"`
}

type controllerHealth struct {
	controllerInfo
	Local      bool   `json:"local"`
	RuntimeDir string `json:"runtimeDir"`
	StateDir   string `json:"stateDir,omitempty"`
	Executable string `json:"executable,omitempty"`
	Handover   int    `json:"handover,omitempty"`
	Instance   string `json:"instance,omitempty"`
}

func parseUIOptions(command string, args []string) (uiOptions, error) {
	var opts uiOptions
	exe, err := os.Executable()
	if err != nil {
		return opts, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	opts.Executable = exe
	defaultAddress := "127.0.0.1:7340"
	if command == "desktop" {
		defaultAddress = "127.0.0.1:0"
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.StringVar(&opts.Address, "listen", defaultAddress, "Loopback address when starting a new browser controller")
	f.StringVar(&opts.StateDir, "state-dir", defaultDir("relay-client"), "Private saved host directory")
	f.StringVar(&opts.RuntimeDir, "runtime-dir", defaultDir("relay"), "Local runtime data directory")
	f.StringVar(&opts.BinaryDir, "binaries", filepath.Dir(exe), "Directory with Linux runtime binaries and SHA256SUMS")
	f.BoolVar(&opts.Local, "local", localRuntimeSupported, "Include this computer's persistent runtime")
	if command == "ui" {
		f.BoolVar(&opts.NoLoginLink, "no-login-link", false, "Do not print a login secret; local launchers issue their own links")
	}
	if err = f.Parse(args); err != nil {
		return opts, err
	}
	if f.NArg() != 0 {
		return opts, errors.New("unexpected positional arguments")
	}
	host, port, err := net.SplitHostPort(opts.Address)
	if err != nil {
		return opts, err
	}
	ip := net.ParseIP(host)
	portNumber, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || portNumber < 0 || portNumber > 65535 {
		return opts, errors.New("listen must use a loopback IP and a valid port; remote access is through SSH tunnels")
	}
	for _, path := range []*string{&opts.StateDir, &opts.RuntimeDir, &opts.BinaryDir} {
		*path, err = filepath.Abs(*path)
		if err != nil {
			return opts, err
		}
	}
	if err := validateControllerOptions(opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func controllerControlHandler(app *controller.Server, info controllerInfo, opts uiOptions, shutdown ...func()) http.Handler {
	mux := http.NewServeMux()
	health := controllerHealth{controllerInfo: info, Local: opts.Local, RuntimeDir: opts.RuntimeDir, StateDir: opts.StateDir, Executable: opts.Executable}
	var stopping atomic.Bool
	if len(shutdown) > 0 {
		health.Handover, health.Instance = 1, rand.Text()
		mux.HandleFunc("POST /handover", func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 2048)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var request controllerHandoverRequest
			if err := decoder.Decode(&request); err != nil {
				http.Error(w, "invalid handover", http.StatusBadRequest)
				return
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				http.Error(w, "invalid handover", http.StatusBadRequest)
				return
			}
			if request.PID != info.PID || request.Version != info.Version || request.Instance != health.Instance || !stopping.CompareAndSwap(false, true) {
				http.Error(w, "controller handover identity changed", http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
			shutdown[0]()
		})
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(health)
	})
	mux.HandleFunc("POST /launch", func(w http.ResponseWriter, r *http.Request) {
		if stopping.Load() {
			http.Error(w, "controller is handing over", http.StatusServiceUnavailable)
			return
		}
		loginURL, err := app.IssueLoginURL()
		if err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		app.ReconnectSaved()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(desktopLaunch{controllerInfo: info, URL: loginURL})
	})
	return mux
}

func controlRequest(ctx context.Context, dir, method, path string, output any) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialControllerControl(ctx, dir)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay-control"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller %s returned HTTP %d", path, resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<10))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("invalid local controller response: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("invalid extra data in local controller response")
	}
	return nil
}

func validateControllerInfo(info controllerInfo) error {
	if err := validateControllerIdentity(info); err != nil {
		return err
	}
	if info.Version != version {
		return fmt.Errorf("running controller is %s (protocol %d), this client is %s (protocol %d); use its matching client or close the existing controller before upgrading", info.Version, info.Protocol, version, controllerProtocol)
	}
	return nil
}

func validateControllerIdentity(info controllerInfo) error {
	if info.Protocol != controllerProtocol || info.Version == "" || len(info.Version) > 101 {
		return errors.New("invalid controller protocol or version")
	}
	for i, c := range []byte(info.Version) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (i > 0 && (c == '.' || c == '_' || c == '+' || c == '-')) {
			continue
		}
		return errors.New("invalid controller version")
	}
	address, err := url.Parse(info.Address)
	if err != nil || address.Scheme != "http" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || address.Opaque != "" || info.PID <= 1 {
		return errors.New("invalid local controller identity")
	}
	host, port, err := net.SplitHostPort(address.Host)
	if err != nil {
		return errors.New("invalid local controller address")
	}
	ip := net.ParseIP(host)
	p, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || p < 1 || p > 65535 {
		return errors.New("local controller must use a loopback IP and port")
	}
	return nil
}

func validateDesktopLaunch(launch desktopLaunch) error {
	if err := validateControllerInfo(launch.controllerInfo); err != nil {
		return err
	}
	u, err := url.Parse(launch.URL)
	if err != nil || u.Scheme+"://"+u.Host != launch.Address || u.User != nil || u.Path != "/auth" || u.RawPath != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("invalid local controller login URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 1 || len(query["token"]) != 1 {
		return errors.New("invalid local controller login token")
	}
	token, err := hex.DecodeString(query.Get("token"))
	if err != nil || len(token) != 24 {
		return errors.New("invalid local controller login token")
	}
	return nil
}

func readControllerInfo(ctx context.Context, opts uiOptions) (controllerInfo, error) {
	var health controllerHealth
	if err := controlRequest(ctx, opts.StateDir, http.MethodGet, "/health", &health); err != nil {
		return controllerInfo{}, err
	}
	info := health.controllerInfo
	if err := validateControllerInfo(info); err != nil {
		return controllerInfo{}, err
	}
	if err := validateControllerConfiguration(health, opts); err != nil {
		return controllerInfo{}, err
	}
	return info, nil
}

func issueControllerLaunch(ctx context.Context, opts uiOptions, info controllerInfo) (desktopLaunch, error) {
	var launch desktopLaunch
	if err := controlRequest(ctx, opts.StateDir, http.MethodPost, "/launch", &launch); err != nil {
		return desktopLaunch{}, err
	}
	if err := validateDesktopLaunch(launch); err != nil {
		return desktopLaunch{}, err
	}
	if launch.controllerInfo != info {
		return desktopLaunch{}, errors.New("controller identity changed during launch; retry")
	}
	return launch, nil
}

func launchExistingController(ctx context.Context, opts uiOptions) (desktopLaunch, error) {
	info, err := readControllerInfo(ctx, opts)
	if err != nil {
		return desktopLaunch{}, err
	}
	return issueControllerLaunch(ctx, opts, info)
}

func ensureController(ctx context.Context, opts uiOptions) (desktopLaunch, error) {
	if err := prepareControllerDirectory(opts.StateDir); err != nil {
		return desktopLaunch{}, err
	}
	probe, err := probeController(ctx, opts)
	if err == nil {
		defer probe.Close()
		if probe.health.Version == version {
			return issueControllerLaunch(ctx, opts, probe.health.controllerInfo)
		}
		return upgradeController(ctx, opts, probe)
	}
	if !controllerUnavailable(err) {
		return desktopLaunch{}, err
	}
	opts, err = stageController(opts)
	if err != nil {
		return desktopLaunch{}, err
	}
	// A launcher arriving between old shutdown and replacement readiness must
	// not steal the lifetime lock and bind a different browser origin.
	lock, err := acquireHandoverLock(ctx, opts.StateDir)
	if err != nil {
		return desktopLaunch{}, err
	}
	defer lock.Close()
	probe, err = probeController(ctx, opts)
	if err == nil {
		defer probe.Close()
		if probe.health.Version != version {
			return desktopLaunch{}, errors.New("another controller version started while waiting; reopen its matching desktop")
		}
		return issueControllerLaunch(ctx, opts, probe.health.controllerInfo)
	}
	if !controllerUnavailable(err) {
		return desktopLaunch{}, err
	}
	return startController(ctx, opts)
}

func startController(ctx context.Context, opts uiOptions) (desktopLaunch, error) {
	logPath := filepath.Join(opts.StateDir, "controller.log")
	logFile, err := privateControllerFile(logPath)
	if err != nil {
		return desktopLaunch{}, err
	}
	defer logFile.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return desktopLaunch{}, err
	}
	defer null.Close()
	args := []string{"ui", "--no-login-link", "--listen", opts.Address, "--state-dir", opts.StateDir, "--runtime-dir", opts.RuntimeDir, "--binaries", opts.BinaryDir, "--local=" + strconv.FormatBool(opts.Local)}
	cmd := exec.Command(opts.Executable, args...)
	cmd.Dir = opts.StateDir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, logFile, logFile
	detachController(cmd)
	if err := cmd.Start(); err != nil {
		return desktopLaunch{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	preserveChild := false
	defer func() {
		if preserveChild || done == nil {
			return
		}
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Deferred cleanup targets only our own still-running child. An
			// existing/racing controller is never stopped by this launcher.
			return desktopLaunch{}, fmt.Errorf("controller did not become ready; see %s", logPath)
		case <-done:
			done = nil // Another launcher may have won the lifetime lock.
		case <-ticker.C:
			info, err := readControllerInfo(ctx, opts)
			if err == nil {
				// A healthy controller may already serve other clients. Once
				// readiness is established, a rejected login (for example an
				// exhausted token pool) must not tear that shared process down.
				if info.PID == cmd.Process.Pid {
					preserveChild = true
				}
				launch, launchErr := issueControllerLaunch(ctx, opts, info)
				if launchErr != nil {
					return desktopLaunch{}, launchErr
				}
				preserveChild = true
				return launch, nil
			}
			if !controllerUnavailable(err) {
				return desktopLaunch{}, err
			}
		}
	}
}

func desktopCommand(args []string) error {
	opts, err := parseUIOptions("desktop", args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	launch, err := ensureController(ctx, opts)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(launch)
}
