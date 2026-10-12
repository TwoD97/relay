package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/TwoD97/relay/internal/fleetregistry"
)

type resolvedFleetHost struct {
	Target        string
	Port          int
	Self          bool
	HostID        string
	Name          string
	ControllerIDs []string
}

func resolveFleetHost(name, fleetID, socket string) (resolvedFleetHost, error) {
	dir, err := fleetregistry.DirectoryForSocket(socket)
	if err != nil {
		return resolvedFleetHost{}, err
	}
	now := time.Now().UTC()
	listing, err := fleetregistry.List(dir, now)
	if err != nil {
		return resolvedFleetHost{}, err
	}
	host, err := listing.Resolve(name, fleetID, now)
	if err != nil {
		return resolvedFleetHost{}, err
	}
	return resolvedFleetHost{Target: host.Target, Port: host.Port, Self: host.Self, HostID: host.ID, Name: host.Name, ControllerIDs: host.ControllerIDs}, nil
}

func fleetCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: relay fleet list [--socket PATH] [--fleet CONTROLLER_ID]")
	}
	action := args[0]
	f := flag.NewFlagSet("fleet "+action, flag.ContinueOnError)
	defaultSocket := os.Getenv("RELAY_SOCKET")
	if defaultSocket == "" {
		defaultSocket = socketPath(defaultDir("relay"))
	}
	socket := f.String("socket", defaultSocket, "Owning runtime socket; fleet metadata is stored beside it")
	sourceID := f.String("fleet", "", "Exact controller ID from fleet list (optional)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected fleet arguments")
	}
	dir, err := fleetregistry.DirectoryForSocket(*socket)
	if err != nil {
		return err
	}
	switch action {
	case "list":
		if *sourceID != "" && !fleetregistry.ValidControllerID(*sourceID) {
			return errors.New("--fleet needs an exact controller ID")
		}
		listing, err := fleetregistry.List(dir, time.Now().UTC())
		if err != nil {
			return err
		}
		if *sourceID != "" {
			filtered := listing.Sources[:0]
			for _, source := range listing.Sources {
				if source.ControllerID == *sourceID {
					filtered = append(filtered, source)
				}
			}
			listing.Sources = filtered
		}
		return json.NewEncoder(os.Stdout).Encode(listing)
	case "import":
		if *sourceID != "" {
			return errors.New("fleet import reads its source identity from bounded JSON stdin")
		}
		snapshot, err := fleetregistry.Decode(os.Stdin)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return fleetregistry.Import(ctx, dir, snapshot, time.Now().UTC())
	default:
		return errors.New("unknown fleet action; use relay fleet list")
	}
}
