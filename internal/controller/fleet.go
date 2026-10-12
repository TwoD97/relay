package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/fleetregistry"
	"github.com/TwoD97/relay/internal/transport"
)

type fleetIdentity struct {
	Format    int                           `json:"format"`
	ID        string                        `json:"id"`
	Revision  uint64                        `json:"revision"`
	Endpoints map[string]fleetregistry.Host `json:"endpoints"`
}

type fleetPublisher struct {
	s        *Server
	mu       sync.Mutex
	identity fleetIdentity
	path     string
	trigger  chan struct{}
	done     chan struct{}
}

func newFleetPublisher(s *Server) (*fleetPublisher, error) {
	p := &fleetPublisher{s: s, path: filepath.Join(s.config.StateDir, "fleet-source.json"), trigger: make(chan struct{}, 1), done: make(chan struct{})}
	p.identity = fleetIdentity{Format: 1, ID: randomID(), Endpoints: map[string]fleetregistry.Host{}}
	info, err := os.Lstat(p.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := p.saveLocked(); err != nil {
			return nil, err
		}
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("fleet source state must be a regular file")
	}
	f, err := os.Open(p.path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, fleetregistry.MaxBytes+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > fleetregistry.MaxBytes || json.Unmarshal(data, &p.identity) != nil || p.identity.Format != 1 || !fleetregistry.ValidControllerID(p.identity.ID) || len(p.identity.Endpoints) > 64 {
		return nil, errors.New("invalid fleet source state; preserve fleet-source.json for inspection")
	}
	if p.identity.Endpoints == nil {
		p.identity.Endpoints = map[string]fleetregistry.Host{}
	}
	for id, endpoint := range p.identity.Endpoints {
		if endpoint.ID != id || endpoint.Target == "" || transport.ValidateTarget(endpoint.Target) != nil || transport.ValidatePort(endpoint.Port) != nil || endpoint.Port == 0 {
			return nil, errors.New("invalid saved fleet endpoint")
		}
	}
	return p, nil
}

func (p *fleetPublisher) saveLocked() error {
	data, err := json.Marshal(p.identity)
	if err != nil || len(data) > fleetregistry.MaxBytes {
		return errors.New("fleet source state exceeds its size limit")
	}
	f, err := os.CreateTemp(filepath.Dir(p.path), ".fleet-source-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), p.path); err != nil {
		return err
	}
	return syncStoreDirectory(filepath.Dir(p.path))
}

func (p *fleetPublisher) changed() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

func (p *fleetPublisher) remember(host Host, target string, port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity.Endpoints[host.ID] = fleetregistry.Host{ID: host.ID, Name: host.Name, ConfiguredTarget: host.Target, ConfiguredPort: host.Port, Target: target, Port: port}
}

func (p *fleetPublisher) invalidate(host Host) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// An older connection must not discard an endpoint resolved for a newer
	// saved configuration. Ordinary disconnects retain their known mapping.
	if known, ok := p.identity.Endpoints[host.ID]; ok && known.ConfiguredTarget == host.Target && known.ConfiguredPort == host.Port {
		delete(p.identity.Endpoints, host.ID)
	}
}

func portableFleetEndpoint(target string) bool {
	_, host, ok := strings.Cut(target, "@")
	if !ok {
		return false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	return true
}

func (p *fleetPublisher) snapshot(now time.Time) (fleetregistry.Snapshot, error) {
	hosts := p.s.store.list()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.identity.Revision == ^uint64(0) {
		return fleetregistry.Snapshot{}, errors.New("fleet revision limit reached")
	}
	p.identity.Revision++
	snapshot := fleetregistry.Snapshot{Format: 1, ControllerID: p.identity.ID, Revision: p.identity.Revision, IssuedAt: now, ExpiresAt: now.Add(fleetregistry.Lifetime), Hosts: []fleetregistry.Host{}}
	keep := map[string]fleetregistry.Host{}
	for _, host := range hosts {
		entry := fleetregistry.Host{ID: host.ID, Name: host.Name, ConfiguredTarget: host.Target, ConfiguredPort: host.Port, Unavailable: "Connect this machine in Relay to resolve its SSH endpoint"}
		if known, ok := p.identity.Endpoints[host.ID]; ok && known.ConfiguredTarget == host.Target && known.ConfiguredPort == host.Port {
			keep[host.ID] = known
			if portableFleetEndpoint(known.Target) {
				entry.Target, entry.Port, entry.Unavailable = known.Target, known.Port, ""
			} else {
				entry.Unavailable = "The controller uses a loopback SSH endpoint; it cannot be shared with another machine"
			}
		}
		snapshot.Hosts = append(snapshot.Hosts, entry)
	}
	p.identity.Endpoints = keep
	if err := fleetregistry.Validate(snapshot); err != nil {
		return snapshot, err
	}
	// Persist every revision before any delivery. Restart never publishes an
	// older snapshot or reuses a revision for different content.
	return snapshot, p.saveLocked()
}

func (p *fleetPublisher) loop() {
	defer close(p.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.s.ctx.Done():
			return
		case <-p.trigger:
		case <-ticker.C:
		}
		if err := p.publish(); err != nil && p.s.ctx.Err() == nil {
			log.Printf("fleet discovery refresh: %v", err)
		}
	}
}

func (p *fleetPublisher) publish() error {
	snapshot, err := p.snapshot(time.Now().UTC())
	if err != nil {
		return err
	}
	type delivery struct {
		id   string
		conn *transport.Connection
	}
	var deliveries []delivery
	p.s.mu.Lock()
	for _, host := range snapshot.Hosts {
		if l := p.s.links[host.ID]; l != nil && l.proxy != nil && l.conn != nil {
			deliveries = append(deliveries, delivery{host.ID, l.conn})
		}
	}
	p.s.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.s.ctx, 20*time.Second)
	defer cancel()
	var result error
	if p.s.config.LocalSocket != "" {
		dir, err := fleetregistry.DirectoryForSocket(p.s.config.LocalSocket)
		if err == nil {
			err = fleetregistry.Import(ctx, dir, snapshot, time.Now().UTC())
		}
		if err != nil {
			result = fmt.Errorf("local discovery: %w", err)
		}
	}
	var mu sync.Mutex
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, item := range deliveries {
		workers.Add(1)
		go func(item delivery) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			copy := snapshot
			copy.SelfHostID = item.id
			if err := item.conn.PublishFleet(ctx, copy); err != nil {
				mu.Lock()
				result = errors.Join(result, fmt.Errorf("host %s: %w", item.id, err))
				mu.Unlock()
			}
		}(item)
	}
	workers.Wait()
	return result
}

// The source registry is metadata only. Read failures never trigger a new SSH
// login, runtime restart, or session mutation.
func (s *Server) fleetChanged() {
	if s.fleet != nil {
		s.fleet.changed()
	}
}

// Retain only the three nonsecret endpoint fields. Authentication settings and
// SSH configuration output are never copied into discovery state.
func (s *Server) resolveFleetEndpoint(ctx context.Context, host Host, conn interface {
	FleetEndpoint(context.Context) (string, int, error)
}) {
	if s.fleet == nil {
		return
	}
	target, port, err := conn.FleetEndpoint(ctx)
	if err == nil {
		s.fleet.remember(host, target, port)
	} else {
		s.fleet.invalidate(host)
	}
	s.fleet.changed()
}
