package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var coordinatorIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// These fixed, separate directories keep a migration target independent from
// the ordinary desktop controller. Caller input can only choose a random ID.
func coordinatorOptions(id, binaries string) (uiOptions, error) {
	if !localRuntimeSupported {
		return uiOptions{}, errors.New("A persistent migration coordinator must run on a Linux host")
	}
	if !coordinatorIDPattern.MatchString(id) {
		return uiOptions{}, errors.New("Coordinator identity must be 32 lowercase hexadecimal characters")
	}
	args := []string{"--listen", "127.0.0.1:0", "--state-dir", filepath.Join(defaultDir("relay"), "coordinators", id), "--runtime-dir", defaultDir("relay"), "--local=true", "--local-mastermind=false"}
	if binaries != "" {
		args = append(args, "--binaries", binaries)
	}
	opts, err := parseUIOptions("desktop", args)
	opts.NoLaunch = true
	return opts, err
}

func coordinatorCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("Use relay coordinator prepare|bridge --id ID")
	}
	action := args[0]
	f := flag.NewFlagSet("coordinator "+action, flag.ContinueOnError)
	id := f.String("id", "", "Dedicated coordinator identity")
	binaries := f.String("binaries", "", "Verified Linux runtime bundle (prepare only)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("Unexpected coordinator arguments")
	}
	if action != "prepare" && action != "bridge" {
		return errors.New("Use coordinator prepare or bridge")
	}
	if action == "bridge" && *binaries != "" {
		return errors.New("The coordinator bridge does not accept a bundle path")
	}
	opts, err := coordinatorOptions(*id, *binaries)
	if err != nil {
		return err
	}
	if action == "prepare" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		info, err := ensureController(ctx, opts)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Protocol int    `json:"protocol"`
			ID       string `json:"id"`
			Version  string `json:"version"`
		}{1, *id, info.Version})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := dialControllerControl(ctx, opts.StateDir)
	if err != nil {
		return err
	}
	defer connection.Close()
	go func() {
		_, _ = io.Copy(connection, os.Stdin)
		if unix, ok := connection.(*net.UnixConn); ok {
			_ = unix.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, connection)
	return err
}
