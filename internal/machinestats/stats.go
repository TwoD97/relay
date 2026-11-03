// Package machinestats collects read-only operating-system resource counters.
package machinestats

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type CPU struct {
	UsagePercent    *float64 `json:"usagePercent"`
	LogicalCPUs     *int     `json:"logicalCpus"`
	IntervalSeconds *float64 `json:"intervalSeconds"`
	Error           string   `json:"error,omitempty"`
}
type Memory struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
	UsedBytes      uint64 `json:"usedBytes"`
}
type Storage struct {
	Scope          string `json:"scope"`
	Path           string `json:"path"`
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
	UsedBytes      uint64 `json:"usedBytes"`
}
type Stats struct {
	SampledAt time.Time `json:"sampledAt"`
	Platform  string    `json:"platform"`
	CPU       CPU       `json:"cpu"`
	Memory    *Memory   `json:"memory"`
	Storage   []Storage `json:"storage"`
	Warnings  []string  `json:"warnings"`
}
type Counter struct {
	Total, Idle uint64
	Cores       int
}
type Sample struct {
	Stats   Stats
	Counter *Counter
}

func NewSample(platform string) Sample {
	return Sample{Stats: Stats{SampledAt: time.Now().UTC(), Platform: platform, Storage: []Storage{}, Warnings: []string{}}}
}

// Calculate uses cumulative ticks, never a process-load estimate. Counter
// reset, changed processor count, or zero elapsed ticks requires a new sample.
func Calculate(current, previous Sample) Stats {
	out := current.Stats
	out.CPU.UsagePercent, out.CPU.IntervalSeconds = nil, nil
	if current.Counter != nil && current.Counter.Cores > 0 {
		n := current.Counter.Cores
		out.CPU.LogicalCPUs = &n
	}
	a, b := previous.Counter, current.Counter
	if a == nil || b == nil || a.Cores != b.Cores || b.Total <= a.Total || b.Idle < a.Idle || !current.Stats.SampledAt.After(previous.Stats.SampledAt) || current.Stats.SampledAt.Sub(previous.Stats.SampledAt) > 30*time.Second {
		return out
	}
	total, idle := b.Total-a.Total, b.Idle-a.Idle
	if idle > total {
		return out
	}
	percent := 100 * float64(total-idle) / float64(total)
	seconds := current.Stats.SampledAt.Sub(previous.Stats.SampledAt).Seconds()
	out.CPU.UsagePercent, out.CPU.IntervalSeconds = &percent, &seconds
	return out
}

func bytesKB(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n > math.MaxUint64/1024 {
		return 0, errors.New("invalid byte count")
	}
	return n * 1024, nil
}

func ParseCPU(text string) (*Counter, error) {
	var result Counter
	found := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cores" && len(fields) == 2 {
			n, e := strconv.Atoi(fields[1])
			if e == nil && n > 0 && n <= 1048576 {
				result.Cores = n
			}
			continue
		}
		if fields[0] != "cpu" {
			continue
		}
		if found || len(fields) < 5 {
			return nil, errors.New("invalid CPU counters")
		}
		found = true
		// Guest ticks are already included in user/nice and must not be added.
		for index, value := range fields[1:min(len(fields), 9)] {
			n, e := strconv.ParseUint(value, 10, 64)
			if e != nil || n > math.MaxUint64-result.Total {
				return nil, errors.New("invalid CPU counters")
			}
			result.Total += n
			if index == 3 || index == 4 {
				if n > math.MaxUint64-result.Idle {
					return nil, errors.New("invalid idle counters")
				}
				result.Idle += n
			}
		}
	}
	if !found || result.Total == 0 || result.Cores == 0 {
		return nil, errors.New("CPU counters unavailable")
	}
	return &result, nil
}

func ParseMemory(text string) (*Memory, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || (f[0] != "MemTotal:" && f[0] != "MemAvailable:") {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return nil, errors.New("invalid memory counters")
		}
		if _, ok := values[f[0]]; ok {
			return nil, errors.New("duplicate memory counter")
		}
		n, e := bytesKB(f[1])
		if e != nil {
			return nil, e
		}
		values[f[0]] = n
	}
	total, ok := values["MemTotal:"]
	available, availableOK := values["MemAvailable:"]
	if !ok || !availableOK || total == 0 || available > total {
		return nil, errors.New("memory availability unavailable")
	}
	return &Memory{TotalBytes: total, AvailableBytes: available, UsedBytes: total - available}, nil
}

func ParseStorage(scope, path, text string) (Storage, error) {
	f := strings.Fields(text)
	s := Storage{Scope: scope, Path: path}
	if len(f) != 3 || path == "" || len(path) > 4096 || !utf8.ValidString(path) {
		return s, errors.New("storage counters unavailable")
	}
	var err error
	s.TotalBytes, err = bytesKB(f[0])
	if err != nil {
		return s, err
	}
	s.UsedBytes, err = bytesKB(f[1])
	if err != nil {
		return s, err
	}
	s.AvailableBytes, err = bytesKB(f[2])
	if err != nil {
		return s, err
	}
	if s.TotalBytes == 0 || s.UsedBytes > s.TotalBytes || s.AvailableBytes > s.TotalBytes-s.UsedBytes {
		return s, errors.New("inconsistent storage counters")
	}
	return s, nil
}

// LinuxScript has no client-supplied commands or paths. NUL-separated fields
// preserve literal HOME names, including whitespace/newlines. df --output
// requests numbers only so mount/source names cannot alter parsing.
const LinuxScript = `LC_ALL=C
export LC_ALL
printf 'relay-stats-v1\000'
awk '$1 == "cpu" {print} $1 ~ /^cpu[0-9]+$/ {n++} END {print "cores", n+0}' /proc/stat 2>/dev/null
printf '\000'
awk '$1 == "MemTotal:" || $1 == "MemAvailable:" {print}' /proc/meminfo 2>/dev/null
printf '\000'
df -k --output=size,used,avail -- / 2>/dev/null | awk 'NR == 2 {print $1,$2,$3}'
printf '\000%s\000' "$HOME"
if [ -n "$HOME" ]; then df -k --output=size,used,avail -- "$HOME" 2>/dev/null | awk 'NR == 2 {print $1,$2,$3}'; fi
printf '\000'
`

func ParseLinux(data []byte) (Sample, error) {
	s := NewSample("linux")
	if len(data) > 65536 {
		return s, errors.New("oversized machine statistics")
	}
	parts := strings.Split(string(data), "\x00")
	if len(parts) != 7 || parts[0] != "relay-stats-v1" || parts[6] != "" {
		return s, errors.New("invalid machine statistics response")
	}
	var err error
	s.Counter, err = ParseCPU(parts[1])
	if err != nil {
		s.Stats.CPU.Error = "CPU counters are unavailable."
		s.Stats.Warnings = append(s.Stats.Warnings, s.Stats.CPU.Error)
	}
	s.Stats.Memory, err = ParseMemory(parts[2])
	if err != nil {
		s.Stats.Warnings = append(s.Stats.Warnings, "Memory availability is unavailable.")
	}
	for _, row := range []struct{ scope, path, text string }{{"root", "/", parts[3]}, {"home", parts[4], parts[5]}} {
		disk, e := ParseStorage(row.scope, row.path, row.text)
		if e != nil {
			s.Stats.Warnings = append(s.Stats.Warnings, "Storage information is unavailable for the "+row.scope+" filesystem.")
			continue
		}
		s.Stats.Storage = append(s.Stats.Storage, disk)
	}
	return s, nil
}
