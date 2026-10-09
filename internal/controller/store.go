package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Host struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Target           string            `json:"target"`
	Port             int               `json:"port"`
	Status           string            `json:"status"`
	Stage            string            `json:"stage"`
	Error            string            `json:"error,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	RuntimeOperation *RuntimeOperation `json:"runtimeOperation,omitempty"`
	SetupWarning     string            `json:"setupWarning,omitempty"`
}

type RuntimeOperation struct {
	Status           string `json:"status"`
	Stage            string `json:"stage"`
	Error            string `json:"error,omitempty"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	RunningVersion   string `json:"runningVersion,omitempty"`
	RestartRequired  bool   `json:"restartRequired,omitempty"`
}

type hostStore struct {
	mu    sync.RWMutex
	path  string
	hosts []Host
}

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure random source unavailable")
	}
	return hex.EncodeToString(b[:])
}

func openStore(dir string) (*hostStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	s := &hostStore{path: filepath.Join(dir, "hosts.json"), hosts: []Host{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &s.hosts); err != nil {
		return nil, fmt.Errorf("read host state: %w", err)
	}
	if len(s.hosts) > 64 {
		return nil, errors.New("host state exceeds the 64 saved host limit")
	}
	seen := map[string]bool{}
	for i := range s.hosts {
		h := &s.hosts[i]
		if h.ID == "" || h.ID == "local" || strings.ContainsAny(h.ID, "/\\") || seen[h.ID] {
			return nil, errors.New("invalid host state; restore hosts.json from backup")
		}
		seen[h.ID] = true
		h.Status = "disconnected"
		h.Stage = "Ready to connect"
		h.Error = ""
		h.RuntimeOperation = nil
		h.SetupWarning = ""
	}
	return s, nil
}

func (s *hostStore) list() []Host {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Host{}, s.hosts...)
}
func (s *hostStore) get(id string) (Host, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.hosts {
		if h.ID == id {
			return h, true
		}
	}
	return Host{}, false
}

func (s *hostStore) saveLocked() error {
	b, err := json.MarshalIndent(s.hosts, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".hosts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	return syncStoreDirectory(filepath.Dir(s.path))
}

func (s *hostStore) add(h Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hosts) >= 64 {
		return errors.New("64 saved hosts reached; forget an unused host first")
	}
	for _, old := range s.hosts {
		if old.Target == h.Target && old.Port == h.Port {
			return errors.New("this SSH target is already saved")
		}
	}
	s.hosts = append(s.hosts, h)
	if err := s.saveLocked(); err != nil {
		s.hosts = s.hosts[:len(s.hosts)-1]
		return err
	}
	return nil
}

func (s *hostStore) update(id, status, stage, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.hosts {
		if s.hosts[i].ID == id {
			s.hosts[i].Status = status
			s.hosts[i].Stage = stage
			s.hosts[i].Error = message
			if status == "disconnected" || status == "error" {
				s.hosts[i].RuntimeOperation = nil
			}
			return
		}
	}
}

func (s *hostStore) runtimeOperation(id string, operation *RuntimeOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.hosts {
		if s.hosts[i].ID == id {
			s.hosts[i].RuntimeOperation = operation
			return
		}
	}
}

func (s *hostStore) setupWarning(id, warning string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.hosts {
		if s.hosts[i].ID == id {
			s.hosts[i].SetupWarning = warning
			return
		}
	}
}

func (s *hostStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.hosts {
		if h.ID == id {
			old := s.hosts
			s.hosts = append(append([]Host{}, old[:i]...), old[i+1:]...)
			if err := s.saveLocked(); err != nil {
				s.hosts = old
				return err
			}
			return nil
		}
	}
	return os.ErrNotExist
}
