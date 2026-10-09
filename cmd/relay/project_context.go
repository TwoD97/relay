package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/TwoD97/relay/internal/projectcontext"
)

func projectContextCommand(args []string) error {
	f := flag.NewFlagSet("project-context", flag.ContinueOnError)
	path := f.String("path", "", "Existing Linux project folder (writes shared instruction and note files)")
	stdin := f.Bool("json-stdin", false, "Read one JSON request from stdin instead of --path")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || (*stdin && *path != "") {
		return errors.New("use project-context --path FOLDER or --json-stdin, without positional arguments")
	}
	request := projectcontext.Request{Path: *path}
	if *stdin {
		var err error
		request, err = readProjectContextRequest(os.Stdin)
		if err != nil {
			return err
		}
	}
	if err := projectcontext.ValidatePath(request.Path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := projectcontext.Prepare(ctx, request.Path)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func readProjectContextRequest(reader io.Reader) (projectcontext.Request, error) {
	var request projectcontext.Request
	data, err := io.ReadAll(io.LimitReader(reader, (32<<10)+1))
	if err != nil {
		return request, err
	}
	if len(data) > 32<<10 {
		return request, errors.New("project context request exceeds 32 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("invalid project context JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return request, errors.New("expected one project context JSON object")
	}
	return request, projectcontext.ValidatePath(request.Path)
}
