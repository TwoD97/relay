package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/TwoD97/relay/internal/skillmanager"
	"io"
	"os"
)

// Private controller deployment helper. Paths come from this host's own user
// profile, never from the network request. Skill files are never executed.
func skillWorker(args []string) error {
	if len(args) != 0 {
		return errors.New("skill-worker reads one JSON request on stdin")
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, (6<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 6<<20 {
		return errors.New("Skill worker request exceeds its limit")
	}
	var request skillmanager.WorkerRequest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&request); err != nil {
		return errors.New("Invalid skill worker request")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("Expected one skill worker request")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	base, err := skillmanager.DefaultHostStore()
	if err != nil {
		return err
	}
	result, err := skillmanager.Worker(base, home, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
