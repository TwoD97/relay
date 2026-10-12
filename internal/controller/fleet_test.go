package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type failedFleetEndpoint struct{}

func (failedFleetEndpoint) FleetEndpoint(context.Context) (string, int, error) {
	return "", 0, errors.New("fixture configuration could not be resolved")
}

func TestFleetPublisherPersistsIdentityAndRemovesForgottenHosts(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{ID: "host-a", Name: "Mynet Docker Live", Target: "desktop-alias", Port: 0}
	if err := store.add(host); err != nil {
		t.Fatal(err)
	}
	server := &Server{config: Config{StateDir: dir}, store: store}
	p, err := newFleetPublisher(server)
	if err != nil {
		t.Fatal(err)
	}
	p.remember(host, "operator@10.10.1.20", 2222)
	first, err := p.snapshot(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if first.Hosts[0].Target != "operator@10.10.1.20" || first.Hosts[0].ConfiguredTarget != "desktop-alias" || first.Hosts[0].Port != 2222 {
		t.Fatal("resolved endpoint missing", first)
	}
	restarted, err := newFleetPublisher(server)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.snapshot(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if second.ControllerID != first.ControllerID || second.Revision <= first.Revision || second.Hosts[0].Target != first.Hosts[0].Target {
		t.Fatal("restart lost identity or resolved metadata", second)
	}
	if err := store.remove(host.ID); err != nil {
		t.Fatal(err)
	}
	third, err := restarted.snapshot(time.Now().UTC())
	if err != nil || len(third.Hosts) != 0 || third.Revision <= second.Revision {
		t.Fatal("forgotten host resurrected", third, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fleet-source.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "operator@") {
		t.Fatal("forgotten endpoint retained in persistent cache")
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(data, &keys) != nil || len(keys) != 4 {
		t.Fatal("unexpected discovery state fields")
	}
}

func TestFleetPublisherDoesNotAdvertiseLoopbackOrGuessUnresolvedAliases(t *testing.T) {
	for _, target := range []string{"u@127.0.0.1", "u@localhost", "u@::1", "u@0.0.0.0", "u@::", "u@alias.localhost"} {
		if portableFleetEndpoint(target) {
			t.Fatal("loopback advertised", target)
		}
	}
	if !portableFleetEndpoint("u@10.1.2.3") || !portableFleetEndpoint("u@server.example") {
		t.Fatal("portable hostname/address rejected")
	}
	dir := t.TempDir()
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{ID: "host-a", Name: "Mynet Docker Live", Target: "desktop-only"}
	if err := store.add(host); err != nil {
		t.Fatal(err)
	}
	p, err := newFleetPublisher(&Server{config: Config{StateDir: dir}, store: store})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.snapshot(time.Now().UTC())
	if err != nil || snapshot.Hosts[0].Target != "" || snapshot.Hosts[0].Unavailable == "" {
		t.Fatal("unresolved alias guessed", snapshot, err)
	}
	p.remember(host, "u@127.0.0.1", 12345)
	snapshot, err = p.snapshot(time.Now().UTC())
	if err != nil || snapshot.Hosts[0].Target != "" || !strings.Contains(snapshot.Hosts[0].Unavailable, "loopback") {
		t.Fatal("loopback alias advertised to peers", snapshot, err)
	}
}

func TestFleetPublisherInvalidatesOnlyMatchingFailedResolution(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{ID: "host-a", Name: "Fixture", Target: "desktop-alias", Port: 2222}
	if err := store.add(host); err != nil {
		t.Fatal(err)
	}
	server := &Server{config: Config{StateDir: dir}, store: store}
	p, err := newFleetPublisher(server)
	if err != nil {
		t.Fatal(err)
	}
	server.fleet = p
	p.remember(host, "operator@192.0.2.1", 2222)
	oldConfig := host
	oldConfig.Port = 2200
	server.resolveFleetEndpoint(context.Background(), oldConfig, failedFleetEndpoint{})
	before, err := p.snapshot(time.Now().UTC())
	if err != nil || before.Hosts[0].Target != "operator@192.0.2.1" {
		t.Fatal("failed old configuration discarded a newer endpoint", before, err)
	}
	server.resolveFleetEndpoint(context.Background(), host, failedFleetEndpoint{})
	after, err := p.snapshot(time.Now().UTC())
	if err != nil || after.Hosts[0].Target != "" || after.Hosts[0].Unavailable == "" {
		t.Fatal("failed resolution renewed an obsolete endpoint", after, err)
	}
	restarted, err := newFleetPublisher(server)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.snapshot(time.Now().UTC())
	if err != nil || loaded.Hosts[0].Target != "" || loaded.Revision <= after.Revision {
		t.Fatal("restart resurrected an invalidated endpoint", loaded, err)
	}
}
