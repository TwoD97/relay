// Relay has a native desktop controller and an independently owned PTY daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/TwoD97/relay/internal/controller"
	"github.com/TwoD97/relay/web"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Println("Relay is a desktop application. Open the Relay app to manage your fleet.\n\nrelay [session|fleet|project-context|daemon|ensure|bridge|version]\n\nsession  List, start, read, send input to, or stop a session\nfleet    List saved Relay machine names and resolved SSH endpoints\nproject-context  Prepare opt-in shared project instructions and notes\nensure   Start or check the persistent local runtime\ndaemon   Run the runtime in the foreground\nbridge   Bridge stdin/stdout to the private runtime socket\n\nUse <command> --help for options.")
		return nil
	case "ui":
		return serveUI(args)
	case "desktop":
		return desktopCommand(args)
	case "hook":
		if !localRuntimeSupported {
			return runtimeCommand(command, args)
		}
		return hookRuntime(args)
	case "notify":
		if !localRuntimeSupported {
			return runtimeCommand(command, args)
		}
		return notifyRuntime(args)
	case "session":
		return sessionCommand(args)
	case "fleet":
		return fleetCommand(args)
	case "project-context":
		return projectContextCommand(args)
	case "daemon", "ensure", "bridge", "harness-maintain":
		return runtimeCommand(command, args)
	}
	return fmt.Errorf("unknown command %q; run relay help", command)
}

func serveUI(args []string) error {
	opts, err := parseUIOptions("ui", args)
	if err != nil {
		return err
	}
	if !opts.NoLoginLink {
		return errors.New("Relay is a desktop application; open the Relay app to manage your fleet")
	}
	lock, err := lockControllerState(opts.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	listener, err := net.Listen("tcp", opts.Address)
	if err != nil {
		return err
	}
	defer listener.Close()
	localSocket := ""
	if opts.Local {
		cmd := exec.Command(opts.Executable, "ensure", "--state-dir", opts.RuntimeDir)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("start local runtime: %s", strings.TrimSpace(stderr.String()))
		}
		localSocket = socketPath(opts.RuntimeDir)
	}
	assets, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return err
	}
	app, err := controller.New(controller.Config{StateDir: opts.StateDir, BinaryDir: opts.BinaryDir, Version: version, Address: listener.Addr().String(), LocalSocket: localSocket, Assets: assets})
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	controlListener, cleanupControl, err := listenControllerControl(opts.StateDir)
	if err != nil {
		return err
	}
	defer controlListener.Close()
	defer cleanupControl()
	info := controllerInfo{Address: "http://" + listener.Addr().String(), Version: version, Protocol: controllerProtocol, PID: os.Getpid()}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	control := &http.Server{Handler: controllerControlHandler(app, info, opts, stop), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- server.Serve(listener) }()
	go func() { serveErrors <- control.Serve(controlListener) }()
	fmt.Printf("Relay %s controller ready at %s\n", version, info.Address)
	select {
	case <-ctx.Done():
	case err = <-serveErrors:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = control.Shutdown(shutdown)
	_ = server.Shutdown(shutdown)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
