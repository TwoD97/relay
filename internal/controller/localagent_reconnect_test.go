package controller

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalAgentReconnectSingleflightAndRecovery(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})
	started := make(chan struct{}, 2)
	s, err := New(Config{StateDir: t.TempDir(), Address: "127.0.0.1:7000", LocalAgentDir: filepath.Join(t.TempDir(), "local"), LocalAgentError: "initial failure", EnsureLocalAgent: func(ctx context.Context) error {
		n := calls.Add(1)
		started <- struct{}{}
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
		if n == 1 {
			return errors.New("fixture unavailable")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	<-started
	for range 12 {
		if n := s.ReconnectSaved(); n != 0 {
			t.Fatalf("concurrent ensure was queued: %d", n)
		}
	}
	close(block)
	until := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		busy := s.localAgentConnecting
		failure := s.localAgentError
		s.mu.Unlock()
		if !busy {
			if failure == "" {
				t.Fatal("failure was hidden")
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("ensure never finished")
		}
		time.Sleep(time.Millisecond)
	}
	if n := s.ReconnectSaved(); n != 1 {
		t.Fatalf("retry not queued: %d", n)
	}
	<-started
	until = time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		busy := s.localAgentConnecting
		failure := s.localAgentError
		s.mu.Unlock()
		if !busy {
			if failure != "" {
				t.Fatalf("stale error retained %q", failure)
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("recovery never finished")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected replay")
	}
	if state := s.mastermind.current(); state.Session != nil {
		t.Fatal("reconnect created a coordinator")
	}
}
