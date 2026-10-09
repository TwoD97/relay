package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	runtimeServer "github.com/TwoD97/relay/internal/runtime"
)

var runtimeVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$`)

func runtimeCommand(command string, args []string) error {
	if command == "harness-maintain" {
		return maintainHarnessCommand(args)
	}

	f := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := f.String("state-dir", defaultDir("relay"), "Private runtime data directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() > 0 {
		return errors.New("unexpected positional arguments")
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if len(socketPath(absolute)) > 103 {
		return errors.New("runtime directory is too long for a Unix socket")
	}
	switch command {
	case "daemon":
		return serveDaemon(absolute)
	case "ensure":
		health, err := ensureDaemon(absolute)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(health)
	case "bridge":
		return bridge(socketPath(absolute))
	}
	return errors.New("unknown runtime command")
}

type health struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	Home     string `json:"home"`
	Hostname string `json:"hostname"`
}

func readHealth(ctx context.Context, dir string) (health, error) {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socketPath(dir))
	}}
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime/api/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return health{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return health{}, fmt.Errorf("runtime health returned HTTP %d", resp.StatusCode)
	}
	var h health
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil {
		return h, err
	}
	return h, nil
}
func compatible(h health) error {
	if h.Protocol != 1 || !runtimeVersionPattern.MatchString(h.Version) {
		return errors.New("running runtime has an unsupported protocol or invalid version; use a compatible client without replacing its live sessions")
	}
	return nil
}

func ensureDaemon(dir string) (health, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if h, err := readHealth(ctx, dir); err == nil {
		return h, compatible(h)
	}
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		return health{}, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return health{}, err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return health{}, err
	}
	defer logFile.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return health{}, err
	}
	defer null.Close()
	exe, err := os.Executable()
	if err != nil {
		return health{}, err
	}
	cmd := exec.Command(exe, "daemon", "--state-dir", dir)
	cmd.Stdin = null
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return health{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return health{}, fmt.Errorf("runtime did not become healthy; see %s", filepath.Join(dir, "daemon.log"))
		case <-done: // A simultaneous ensure may already own the daemon lock.
			done = nil
		case <-ticker.C:
			if h, err := readHealth(ctx, dir); err == nil {
				return h, compatible(h)
			}
		}
	}
}

func serveDaemon(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "run"), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "run", "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("runtime already owns this data directory")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	sock := socketPath(dir)
	if err := removeStaleSocket(sock); err != nil {
		return err
	}
	listener, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(sock)
	if err = os.Chmod(sock, 0600); err != nil {
		return err
	}
	app, err := runtimeServer.New(dir, version)
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Relay %s runtime ready", version)
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func bridge(socket string) error {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		_, _ = io.Copy(c, os.Stdin)
		if u, ok := c.(*net.UnixConn); ok {
			_ = u.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, c)
	return err
}
