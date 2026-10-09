package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	runtimeServer "github.com/TwoD97/relay/internal/runtime"
)

// A standalone worker lets a compatible older daemon own the setup PTY while
// the current client installs new harnesses without replacing that daemon.
func maintainHarnessCommand(args []string) error {
	f := flag.NewFlagSet("harness-maintain", flag.ContinueOnError)
	dir := f.String("state-dir", defaultDir("relay"), "Private runtime data directory")
	id := f.String("id", "", "claude or codex")
	action := f.String("action", "", "update or repair")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || (*id != "claude" && *id != "codex") || (*action != "update" && *action != "repair") {
		return errors.New("harness-maintain requires --id claude|codex --action update|repair")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	return runtimeServer.MaintainHarness(ctx, *dir, *id, *action, os.Stdout)
}
