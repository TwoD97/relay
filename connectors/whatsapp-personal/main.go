// SPDX-License-Identifier: GPL-3.0-only
// This optional, standalone connector owns its linked-device credentials. Its
// bounded JSON-lines interface contains no Relay controller or terminal API.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/TwoD97/relay/connectors/whatsapp-personal/adapter"
)

const maxFrame = 128 << 10

type request struct {
	ID        uint64 `json:"id"`
	Method    string `json:"method"`
	Phone     string `json:"phone,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"text,omitempty"`
}
type response struct {
	Kind    string           `json:"kind"`
	ID      uint64           `json:"id,omitempty"`
	State   *adapter.State   `json:"state,omitempty"`
	Code    string           `json:"code,omitempty"`
	Message *adapter.Message `json:"message,omitempty"`
	Error   string           `json:"error,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "WhatsApp connector stopped; inspect its local configuration.")
		os.Exit(1)
	}
}
func run() error {
	flags := flag.NewFlagSet("relay-whatsapp-personal", flag.ContinueOnError)
	stdio := flags.Bool("stdio", false, "Serve bounded JSON lines on stdin/stdout")
	dir := flags.String("state-dir", "", "Private directory for this linked device")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if !*stdio || *dir == "" || !filepath.IsAbs(*dir) || flags.NArg() != 0 {
		return errors.New("--stdio and an absolute --state-dir are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	var output sync.Mutex
	emit := func(value response) {
		output.Lock()
		defer output.Unlock()
		data, err := json.Marshal(value)
		if err != nil || len(data) > maxFrame {
			cancel()
			return
		}
		data = append(data, '\n')
		if _, err = os.Stdout.Write(data); err != nil {
			cancel()
		}
	}
	client, err := adapter.Open(ctx, *dir, func(message adapter.Message) { emit(response{Kind: "message", Message: &message}) })
	if err != nil {
		emit(response{Kind: "fatal", Error: "Private linked-device storage could not be opened"})
		return err
	}
	defer client.Close()
	var events sync.WaitGroup
	events.Add(1)
	go func() {
		defer events.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var previous adapter.State
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state := client.State()
				if state != previous {
					previous = state
					emit(response{Kind: "state", State: &state})
				}
			}
		}
	}()
	defer func() { cancel(); events.Wait() }()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxFrame)
	var previousID uint64
	for scanner.Scan() {
		var req request
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.ID == 0 || req.ID <= previousID || req.ID >= 1<<53 {
			return errors.New("invalid connector request")
		}
		previousID = req.ID
		command, stop := context.WithTimeout(ctx, 25*time.Second)
		result := response{Kind: "response", ID: req.ID}
		switch req.Method {
		case "state":
		case "connect":
			err = client.Connect(command)
		case "pair":
			if len(req.Phone) < 7 || len(req.Phone) > 15 {
				err = errors.New("Invalid international phone number")
			} else {
				result.Code, err = client.Pair(command, req.Phone)
			}
		case "send":
			err = client.Send(command, req.MessageID, req.Text)
		case "logout":
			err = client.Logout(command)
		case "close":
			stop()
			emit(result)
			return nil
		default:
			err = errors.New("Unknown connector operation")
		}
		stop()
		state := client.State()
		result.State = &state
		if err != nil {
			result.Error = err.Error()
			err = nil
		}
		emit(result)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return scanner.Err()
}
