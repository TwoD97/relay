package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/TwoD97/relay/internal/conversations"
	runtimeServer "github.com/TwoD97/relay/internal/runtime"
)

func conversationsCommand(args []string) error {
	f := flag.NewFlagSet("conversations", flag.ContinueOnError)
	dir := f.String("state-dir", defaultDir("relay"), "Private Relay runtime directory")
	stdin := f.Bool("json-stdin", false, "Read a bounded metadata operation from stdin")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*stdin || f.NArg() != 0 {
		return errors.New("conversations requires --json-stdin")
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, (16<<10)+1))
	if err != nil || len(data) > 16<<10 {
		return errors.New("conversation metadata request exceeds 16 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var request conversations.Request
	if dec.Decode(&request) != nil || dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid conversation metadata request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := runtimeServer.ConversationOperation(ctx, *dir, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
