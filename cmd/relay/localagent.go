package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/TwoD97/relay/internal/localagent"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"
)

var localAgentVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$`)

func localAgentCommand(command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := f.String("state-dir", defaultDir("relay-localagent"), "Private local coordinator data directory")
	harness := f.String("harness", "", "Provider identifier")
	action := f.String("action", "install", "Provider maintenance action")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected local agent arguments")
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	switch command {
	case "local-agent-ensure":
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		health, err := ensureLocalAgent(ctx, absolute)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(health)
	case "local-agent-daemon":
		return serveLocalAgent(absolute)
	case "local-agent-install":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return localagent.Install(ctx, absolute, *harness, *action, os.Stdout)
	default:
		return errors.New("unknown local agent command")
	}
}
func readLocalAgentHealth(ctx context.Context, dir string) (localagent.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return localagent.DialContext(ctx, dir) }}
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localagent/api/health", nil)
	response, err := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return localagent.Health{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return localagent.Health{}, errors.New("existing local agent endpoint is not healthy")
	}
	var health localagent.Health
	data, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(data) > 8192 {
		return health, errors.New("invalid local agent health response")
	}
	if err = json.Unmarshal(data, &health); err != nil {
		return health, err
	}
	if health.Protocol != localagent.Protocol || !localAgentVersionPattern.MatchString(health.Version) || health.Platform != runtime.GOOS || !filepath.IsAbs(health.Home) || health.PID < 1 || !health.Capabilities["localMastermindV1"] {
		return health, errors.New("existing local agent is incompatible; preserve its sessions and close them before upgrading the service")
	}
	return health, nil
}
func ensureLocalAgent(ctx context.Context, dir string) (localagent.Health, error) {
	if err := prepareControllerDirectory(dir); err != nil {
		return localagent.Health{}, err
	}
	health, err := readLocalAgentHealth(ctx, dir)
	if err == nil {
		return health, nil
	}
	if !controllerUnavailable(err) {
		return health, err
	}
	exe, err := os.Executable()
	if err != nil {
		return health, err
	}
	// Only the executable is needed locally. Keep its release out of the
	// installer directory so replacing/uninstalling the UI cannot lock this PE.
	emptyBundle := filepath.Join(dir, "local-payload")
	if err = privateControllerDirectory(emptyBundle); err != nil {
		return health, err
	}
	staged, err := stageController(uiOptions{StateDir: dir, Executable: exe, BinaryDir: emptyBundle})
	if err != nil {
		return health, err
	}
	log, err := privateControllerFile(filepath.Join(dir, "daemon.log"))
	if err != nil {
		return health, err
	}
	defer log.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return health, err
	}
	defer null.Close()
	cmd := exec.Command(staged.Executable, "local-agent-daemon", "--state-dir", dir)
	cmd.Dir = dir
	cmd.Stdin = null
	cmd.Stdout = log
	cmd.Stderr = log
	detachController(cmd)
	if err = cmd.Start(); err != nil {
		return health, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	preserve := false
	defer func() {
		if preserve || done == nil {
			return
		}
		// Cancellation can race another launcher observing our ready child.
		// Recheck private health independently before stopping only our child.
		verifyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		verified, verifyErr := readLocalAgentHealth(verifyCtx, dir)
		cancel()
		if verifyErr == nil && verified.PID == cmd.Process.Pid {
			return
		}
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return health, fmt.Errorf("local agent did not become ready; see %s", filepath.Join(dir, "daemon.log"))
		case <-done:
			done = nil
		case <-tick.C:
			health, err = readLocalAgentHealth(ctx, dir)
			if err == nil {
				preserve = true
				return health, nil
			}
			if !controllerUnavailable(err) {
				return health, err
			}
		}
	}
}
func serveLocalAgent(dir string) error {
	lock, err := lockControllerFile(dir, "local-agent.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	listener, cleanup, err := listenLocalAgent(dir)
	if err != nil {
		return err
	}
	defer cleanup()
	defer listener.Close()
	app, err := localagent.New(dir, version)
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err = <-done:
	case <-ctx.Done():
	}
	shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
