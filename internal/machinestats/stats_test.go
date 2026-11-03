package machinestats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture() []byte {
	return []byte("relay-stats-v1\x00cpu 10 20 30 40 5 6 7 8 900 900\ncores 4\n\x00MemTotal: 1024 kB\nMemAvailable: 256 kB\n\x001000 600 300\n\x00/home/a ' $(touch nope)\nspace\x002000 700 1200\n\x00")
}

func TestLinuxResourceParsingAndPartialFailures(t *testing.T) {
	s, err := ParseLinux(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if s.Counter.Total != 126 || s.Counter.Idle != 45 || s.Counter.Cores != 4 {
		t.Fatal("guest counted twice or CPU counters wrong", s.Counter)
	}
	if s.Stats.Memory.UsedBytes != 768*1024 || s.Stats.Memory.AvailableBytes != 256*1024 || len(s.Stats.Storage) != 2 || s.Stats.Storage[1].Path != "/home/a ' $(touch nope)\nspace" {
		t.Fatal("lost literal path or memory semantics", s.Stats)
	}
	if len(s.Stats.Warnings) != 0 {
		t.Fatal(s.Stats.Warnings)
	}
	data := strings.Replace(string(fixture()), "MemAvailable: 256 kB", "MemFree: 256 kB", 1)
	data = strings.Replace(data, "1000 600 300", "1000 800 300", 1)
	partial, err := ParseLinux([]byte(data))
	if err != nil || partial.Counter == nil || partial.Stats.Memory != nil || len(partial.Stats.Storage) != 1 || len(partial.Stats.Warnings) != 2 {
		t.Fatal("partial failure erased valid readings", partial, err)
	}
	partial, err = ParseLinux([]byte(strings.Replace(string(fixture()), "cpu 10 20 30 40 5 6 7 8 900 900", "cpu invalid", 1)))
	if err != nil || partial.Counter != nil || partial.Stats.CPU.Error == "" || partial.Stats.Memory == nil {
		t.Fatal("CPU failure missing explicit unavailable state")
	}
	for _, data := range [][]byte{[]byte("garbage"), append(fixture(), 1), []byte(strings.Repeat("x", 65537))} {
		if _, err := ParseLinux(data); err == nil {
			t.Fatal("invalid framing accepted")
		}
	}
}

func TestResourceCountersRejectOverflowAndUnderflow(t *testing.T) {
	for _, cpu := range []string{"cpu 1 2 3 4\ncores 0", "cpu -1 2 3 4\ncores 1", fmt.Sprintf("cpu %d 2 3 4\ncores 1", uint64(math.MaxUint64)), "cpu 1 2 3 4\ncpu 1 2 3 4\ncores 1"} {
		if _, e := ParseCPU(cpu); e == nil {
			t.Fatal("invalid CPU accepted", cpu)
		}
	}
	for _, memory := range []string{"MemTotal: 1 kB\nMemAvailable: 2 kB", "MemTotal: 0 kB\nMemAvailable: 0 kB", "MemTotal: 18446744073709551615 kB\nMemAvailable: 0 kB", "MemTotal: 1 kB\nMemTotal: 1 kB\nMemAvailable: 0 kB"} {
		if _, e := ParseMemory(memory); e == nil {
			t.Fatal("invalid memory accepted")
		}
	}
	for _, disk := range []string{"0 0 0", "100 110 0", "100 90 20", "18446744073709551615 1 0", "100 -1 0"} {
		if _, e := ParseStorage("root", "/", disk); e == nil {
			t.Fatal("invalid storage accepted", disk)
		}
	}
}

func TestCPUPercentageNeedsTwoValidConsistentSamples(t *testing.T) {
	a := NewSample("linux")
	a.Counter = &Counter{100, 20, 4}
	b := a
	b.Stats.SampledAt = a.Stats.SampledAt.Add(5 * time.Second)
	b.Counter = &Counter{300, 70, 4}
	if got := Calculate(a, Sample{}); got.CPU.UsagePercent != nil || got.CPU.LogicalCPUs == nil || *got.CPU.LogicalCPUs != 4 {
		t.Fatal("firstsample invented load")
	}
	got := Calculate(b, a)
	if got.CPU.UsagePercent == nil || *got.CPU.UsagePercent != 75 || *got.CPU.IntervalSeconds != 5 {
		t.Fatal(got)
	}
	stale := b
	stale.Stats.SampledAt = a.Stats.SampledAt.Add(time.Hour)
	if Calculate(stale, a).CPU.UsagePercent != nil {
		t.Fatal("returning after an idle overview displayed a long-term average as current load")
	}
	for _, counter := range []*Counter{{50, 10, 4}, {100, 20, 4}, {110, 40, 4}, {300, 10, 4}, {300, 70, 8}, nil} {
		b.Counter = counter
		if got := Calculate(b, a); got.CPU.UsagePercent != nil {
			t.Fatal("reset/inconsistent counter invented load", counter)
		}
	}
}

func TestCacheCoalescesReadersAndPreservesOtherWaiters(t *testing.T) {
	var c Cache
	var calls atomic.Int32
	started, finish := make(chan struct{}), make(chan struct{})
	collect := func(ctx context.Context) (Sample, error) {
		calls.Add(1)
		close(started)
		select {
		case <-finish:
			return ParseLinux(fixture())
		case <-ctx.Done():
			return Sample{}, ctx.Err()
		}
	}
	first, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { _, e := c.Read(first, context.Background(), time.Second, time.Second, collect); done <- e }()
	<-started
	go func() {
		_, e := c.Read(context.Background(), context.Background(), time.Second, time.Second, collect)
		done <- e
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		n := c.active.waiters
		c.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second waiter absent")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	close(finish)
	if e := <-done; e != nil {
		t.Fatal("one cancelling client poisoned another", e)
	}
	for range 20 {
		if _, e := c.Read(context.Background(), context.Background(), time.Second, time.Second, collect); e != nil {
			t.Fatal(e)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("cache launched duplicate probes", calls.Load())
	}
}

func TestCacheCancelsFinalReaderAndBoundsStuckCollectors(t *testing.T) {
	var c Cache
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	collect := func(context.Context) (Sample, error) {
		calls.Add(1)
		close(started)
		<-release
		return ParseLinux(fixture())
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, e := c.Read(context.Background(), context.Background(), time.Second, 15*time.Millisecond, collect)
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Error(e)
		}
	}()
	<-started
	wg.Wait()
	for range 10 {
		_, e := c.Read(context.Background(), context.Background(), time.Second, 15*time.Millisecond, collect)
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Fatal(e)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("blocked OS call spawned unbounded collectors")
	}
	close(release)
	var other Cache
	started2, stopped := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = other.Read(ctx, context.Background(), time.Second, time.Second, func(ctx context.Context) (Sample, error) {
			close(started2)
			<-ctx.Done()
			close(stopped)
			return Sample{}, ctx.Err()
		})
	}()
	<-started2
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("last cancellation did not stop probe")
	}
}
