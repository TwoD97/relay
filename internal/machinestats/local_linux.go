package machinestats

import (
	"context"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func readBounded(path string) string {
	f, e := os.Open(path)
	if e != nil {
		return ""
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1048577))
	if e != nil || len(b) > 1048576 {
		return ""
	}
	return string(b)
}

func Collect(ctx context.Context) (Sample, error) {
	s := NewSample("linux")
	if e := ctx.Err(); e != nil {
		return s, e
	}
	stat := readBounded("/proc/stat")
	cores := 0
	for _, line := range strings.Split(stat, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], "cpu") && f[0] != "cpu" {
			if _, e := strconv.Atoi(strings.TrimPrefix(f[0], "cpu")); e == nil {
				cores++
			}
		}
	}
	var e error
	s.Counter, e = ParseCPU(stat + "\ncores " + strconv.Itoa(cores))
	if e != nil {
		s.Stats.CPU.Error = "CPU counters are unavailable."
		s.Stats.Warnings = append(s.Stats.Warnings, s.Stats.CPU.Error)
	}
	s.Stats.Memory, e = ParseMemory(readBounded("/proc/meminfo"))
	if e != nil {
		s.Stats.Warnings = append(s.Stats.Warnings, "Memory availability is unavailable.")
	}
	home, _ := os.UserHomeDir()
	for _, item := range []struct{ scope, path string }{{"root", "/"}, {"home", home}} {
		if e := ctx.Err(); e != nil {
			return s, e
		}
		var fs unix.Statfs_t
		if item.path == "" || unix.Statfs(item.path, &fs) != nil || fs.Bsize <= 0 || fs.Blocks == 0 || fs.Bfree > fs.Blocks || fs.Bavail > fs.Bfree || fs.Blocks > math.MaxUint64/uint64(fs.Bsize) {
			s.Stats.Warnings = append(s.Stats.Warnings, "Storage information is unavailable for the "+item.scope+" filesystem.")
			continue
		}
		unit := uint64(fs.Bsize)
		s.Stats.Storage = append(s.Stats.Storage, Storage{Scope: item.scope, Path: item.path, TotalBytes: fs.Blocks * unit, UsedBytes: (fs.Blocks - fs.Bfree) * unit, AvailableBytes: fs.Bavail * unit})
	}
	return s, ctx.Err()
}
