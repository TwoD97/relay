// Package fleetregistry stores discovery metadata, never SSH credentials or
// authorization. Every session operation still uses the caller's SSH policy.
package fleetregistry

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBytes   = 128 << 10
	MaxSources = 32
	Lifetime   = 10 * time.Minute
)

type Host struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ConfiguredTarget string `json:"configuredTarget"`
	ConfiguredPort   int    `json:"configuredPort"`
	Target           string `json:"target,omitempty"`
	Port             int    `json:"port,omitempty"`
	Unavailable      string `json:"unavailable,omitempty"`
}

type Snapshot struct {
	Format       int       `json:"format"`
	ControllerID string    `json:"controllerId"`
	Revision     uint64    `json:"revision"`
	IssuedAt     time.Time `json:"issuedAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	SelfHostID   string    `json:"selfHostId,omitempty"`
	Hosts        []Host    `json:"hosts"`
}

type Source struct {
	Snapshot
	Stale bool `json:"stale"`
}

type Listing struct {
	Sources []Source `json:"sources"`
}

type Resolution struct {
	Host
	Self          bool     `json:"self"`
	ControllerIDs []string `json:"controllerIds"`
}

func ValidControllerID(id string) bool {
	data, err := hex.DecodeString(id)
	return err == nil && len(data) == 24 && strings.ToLower(id) == id
}

func validText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func Validate(s Snapshot) error {
	if s.Format != 1 || !ValidControllerID(s.ControllerID) || s.Revision == 0 || s.IssuedAt.IsZero() || !s.ExpiresAt.After(s.IssuedAt) || s.ExpiresAt.Sub(s.IssuedAt) > Lifetime || len(s.Hosts) > 64 {
		return errors.New("invalid fleet source identity, revision, expiry or host limit")
	}
	seen := map[string]bool{}
	for _, h := range s.Hosts {
		if !validText(h.ID, 80) || strings.ContainsAny(h.ID, "/\\") || seen[h.ID] || !validText(h.Name, 80) || !validText(h.ConfiguredTarget, 320) || h.ConfiguredPort < 0 || h.ConfiguredPort > 65535 {
			return errors.New("invalid or duplicate fleet host metadata")
		}
		seen[h.ID] = true
		if h.Target == "" {
			if !validText(h.Unavailable, 512) || h.Port != 0 {
				return errors.New("unresolved fleet host needs a diagnostic")
			}
		} else if !validText(h.Target, 320) || strings.ContainsAny(h.Target, " \t\r\n") || !strings.Contains(h.Target, "@") || h.Port < 1 || h.Port > 65535 || h.Unavailable != "" {
			return errors.New("invalid resolved SSH endpoint")
		}
	}
	if s.SelfHostID != "" && !seen[s.SelfHostID] {
		return errors.New("fleet source host is missing from its snapshot")
	}
	return nil
}

func Decode(reader io.Reader) (Snapshot, error) {
	var s Snapshot
	data, err := io.ReadAll(io.LimitReader(reader, MaxBytes+1))
	if err != nil {
		return s, err
	}
	if len(data) > MaxBytes {
		return s, errors.New("fleet snapshot exceeds 128 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, fmt.Errorf("invalid fleet JSON: %w", err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return s, errors.New("expected one fleet snapshot")
	}
	return s, Validate(s)
}

func DirectoryForSocket(socket string) (string, error) {
	if !filepath.IsAbs(socket) || filepath.Base(socket) != "daemon.sock" || filepath.Base(filepath.Dir(socket)) != "run" {
		return "", errors.New("fleet discovery requires an absolute Relay <state>/run/daemon.sock path")
	}
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Clean(socket))), "fleet"), nil
}

func (l Listing) Resolve(name, sourceID string, now time.Time) (Resolution, error) {
	if !validText(name, 80) {
		return Resolution{}, errors.New("--host needs an exact saved Relay name or host ID")
	}
	if sourceID != "" && !ValidControllerID(sourceID) {
		return Resolution{}, errors.New("--fleet needs an exact controller ID from relay fleet list")
	}
	var matches []Resolution
	staleMatch := false
	for _, source := range l.Sources {
		if sourceID != "" && source.ControllerID != sourceID {
			continue
		}
		for _, host := range source.Hosts {
			if host.ID != name && host.Name != name {
				continue
			}
			if source.Stale || !source.ExpiresAt.After(now) || source.IssuedAt.After(now.Add(2*time.Minute)) {
				staleMatch = true
				continue
			}
			matches = append(matches, Resolution{Host: host, Self: source.SelfHostID == host.ID, ControllerIDs: []string{source.ControllerID}})
		}
	}
	if len(matches) == 0 {
		if staleMatch {
			return Resolution{}, errors.New("fleet mapping has expired; reconnect the source machine in Relay to refresh it")
		}
		return Resolution{}, fmt.Errorf("no current Relay host matches %q; run relay fleet list (names and IDs are exact)", name)
	}
	result := matches[0]
	seenControllers := map[string]bool{result.ControllerIDs[0]: true}
	for _, match := range matches[1:] {
		// A name colliding with another host's ID is ambiguous even if the names
		// happen to point at an equal endpoint. Identical multi-controller copies
		// of a named machine may be shared without selecting a controller.
		if seenControllers[match.ControllerIDs[0]] || match.Self != result.Self || match.Target != result.Target || match.Port != result.Port || match.Target == "" || (match.ID != result.ID && (match.ID == name || result.ID == name)) {
			return Resolution{}, fmt.Errorf("Relay host %q is ambiguous; use an exact host ID and --fleet from relay fleet list", name)
		}
		seenControllers[match.ControllerIDs[0]] = true
		result.ControllerIDs = append(result.ControllerIDs, match.ControllerIDs...)
	}
	if !result.Self && result.Target == "" {
		return Resolution{}, fmt.Errorf("Relay host %q has no portable SSH endpoint: %s", name, result.Unavailable)
	}
	sort.Strings(result.ControllerIDs)
	return result, nil
}
