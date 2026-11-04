package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"
)

func controlCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: relay control show|wait|claim|reply|learn; use --help for action options")
	}
	action := args[0]
	f := flag.NewFlagSet("control "+action, flag.ContinueOnError)
	id := f.String("id", "", "Exact instruction or new lesson ID")
	text := f.String("text", "", "Result to send to the user")
	seconds := f.Int("seconds", 300, "Wait locally for an instruction (1–3600 seconds; no model calls)")
	problem := f.String("problem", "", "Observed failure or reusable discovery")
	step := f.String("action", "", "Action that was actually tested")
	outcome := f.String("outcome", "", "success or failed")
	evidence := f.String("evidence", "", "Concrete verification and exact target")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("Unexpected arguments; use named options")
	}
	var data []byte
	var err error
	switch action {
	case "show":
		data, err = agentRequest("GET", "/agent/control", nil)
	case "wait":
		if *seconds < 1 || *seconds > 3600 {
			return errors.New("--seconds must be between 1 and 3600")
		}
		deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
		for {
			data, err = agentRequest("GET", "/agent/control/wait", nil)
			if err != nil {
				break
			}
			var response struct {
				Messages []json.RawMessage `json:"messages"`
			}
			if json.Unmarshal(data, &response) != nil {
				return errors.New("Invalid inbox response")
			}
			if len(response.Messages) > 0 || time.Now().After(deadline) {
				break
			}
		}
	case "claim", "reply":
		if !agentID(*id) {
			return errors.New("Read control show/wait and supply the exact --id")
		}
		var body any
		if action == "reply" {
			body = map[string]string{"text": *text}
		}
		data, err = agentRequest("POST", "/agent/control/messages/"+*id+"/"+action, body)
	case "learn":
		if !agentID(*id) {
			return errors.New("Supply a new 32-character hexadecimal --id; inspect it before repeating an uncertain write")
		}
		data, err = agentRequest("POST", "/agent/control/lessons", map[string]string{"id": *id, "problem": *problem, "action": *step, "outcome": *outcome, "evidence": *evidence})
	default:
		return errors.New("Use show, wait, claim, reply, or learn")
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}
