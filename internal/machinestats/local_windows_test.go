package machinestats

import (
	"context"
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsMemoryABIAndFiletime(t *testing.T) {
	// MEMORYSTATUSEX is two DWORDs followed by seven DWORDLONGs. Its
	// dwLength is part of the Windows API contract, not a Go serialization.
	if unsafe.Sizeof(memoryStatus{}) != 64 || unsafe.Offsetof(memoryStatus{}.TotalPhysical) != 8 {
		t.Fatal("Windows memory status layout does not match MEMORYSTATUSEX")
	}
	if filetimeValue(windows.Filetime{HighDateTime: 1, LowDateTime: 3}) != 1<<32|3 {
		t.Fatal("Windows CPU counter was truncated")
	}
}

func TestWindowsCollectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sample, err := Collect(ctx)
	if !errors.Is(err, context.Canceled) || sample.Counter != nil || sample.Stats.Memory != nil || len(sample.Stats.Storage) != 0 {
		t.Fatalf("canceled collection returned data: %v", err)
	}
}

func TestWindowsReadOnlySystemCounters(t *testing.T) {
	// Only OS resource APIs are read. No personal profile files, providers,
	// remote hosts, desktop instances, or clipboard APIs are accessed.
	t.Setenv("USERPROFILE", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := Collect(ctx)
	if err != nil || first.Counter == nil || first.Stats.Memory == nil {
		t.Fatalf("native system counters unavailable: %v", err)
	}
	if first.Counter.Cores <= 0 || first.Counter.Idle > first.Counter.Total || first.Stats.Memory.TotalBytes == 0 || first.Stats.Memory.UsedBytes+first.Stats.Memory.AvailableBytes != first.Stats.Memory.TotalBytes {
		t.Fatal("native resource counters were inconsistent")
	}
	if Calculate(first, Sample{}).CPU.UsagePercent != nil {
		t.Fatal("first CPU sample claimed to measure usage")
	}
	if len(first.Stats.Storage) == 0 {
		t.Fatal("fixed system drive was not measured")
	}
	for _, disk := range first.Stats.Storage {
		if disk.TotalBytes == 0 || disk.UsedBytes > disk.TotalBytes || disk.AvailableBytes != disk.TotalBytes-disk.UsedBytes {
			t.Fatal("native drive counters were inconsistent")
		}
	}
	time.Sleep(50 * time.Millisecond)
	second, err := Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stats := Calculate(second, first)
	if stats.CPU.UsagePercent == nil || *stats.CPU.UsagePercent < 0 || *stats.CPU.UsagePercent > 100 {
		t.Fatal("native CPU sampling did not yield a bounded percentage")
	}
}
