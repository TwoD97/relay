package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	runtimeServer "github.com/TwoD97/relay/internal/runtime"
)

// The current CLI can run inside a shell PTY owned by an older daemon. This
// command never opens the runtime database or replaces that daemon.
func mastermindWorkerCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("mastermind-worker requires probe, run, or status")
	}
	action := args[0]
	f := flag.NewFlagSet("mastermind-worker "+action, flag.ContinueOnError)
	dir := f.String("state-dir", defaultDir("relay"), "Private runtime data directory")
	harness := f.String("harness", "", "claude or codex")
	mode := f.String("mode", "", "manual or auto")
	id := f.String("launch-id", "", "Controller-owned launch identity")
	session := f.String("session-id", "", "Owning shell session identity")
	created := f.String("session-created-at", "", "Owning session creation time")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected mastermind-worker arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch action {
	case "probe":
		if *id != "" || *session != "" || *created != "" {
			return errors.New("probe does not accept a launch identity")
		}
		capability, err := runtimeServer.ProbeMastermindWorker(ctx, *dir, *harness, *mode)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(capability)
	case "status":
		if *harness != "" || *mode != "" || *session != "" || *created != "" {
			return errors.New("status accepts only a launch identity and runtime directory")
		}
		status, err := runtimeServer.ReadMastermindWorker(*dir, *id)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(status)
	case "run":
		started, err := time.Parse(time.RFC3339Nano, *created)
		if err != nil || started.IsZero() {
			return errors.New("run requires an exact session creation time")
		}
		return runtimeServer.RunMastermindWorker(ctx, *dir, runtimeServer.MastermindWorkerLaunch{
			LaunchID: *id, SessionID: *session, SessionCreatedAt: started, Harness: *harness, AgentMode: *mode,
		}, os.Stdout)
	}
	return errors.New("unknown mastermind-worker action")
}
