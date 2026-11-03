package machinestats

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel             = windows.NewLazySystemDLL("kernel32.dll")
	getSystemTimes     = kernel.NewProc("GetSystemTimes")
	globalMemoryStatus = kernel.NewProc("GlobalMemoryStatusEx")
)

type memoryStatus struct {
	Length, Load                                                                                                          uint32
	TotalPhysical, AvailablePhysical, TotalPageFile, AvailablePageFile, TotalVirtual, AvailableVirtual, AvailableExtended uint64
}

func filetimeValue(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}

func Collect(ctx context.Context) (Sample, error) {
	s := NewSample("windows")
	if e := ctx.Err(); e != nil {
		return s, e
	}
	var idle, kern, user windows.Filetime
	ok, _, _ := getSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user)))
	i, k, u := filetimeValue(idle), filetimeValue(kern), filetimeValue(user)
	// GetSystemTimes includes idle time in the kernel counter.
	if ok != 0 && k <= math.MaxUint64-u && i <= k {
		s.Counter = &Counter{Total: k + u, Idle: i, Cores: runtime.NumCPU()}
	} else {
		s.Stats.CPU.Error = "CPU counters are unavailable."
		s.Stats.Warnings = append(s.Stats.Warnings, s.Stats.CPU.Error)
	}
	if runtime.NumCPU() > 64 {
		s.Stats.Warnings = append(s.Stats.Warnings, "CPU usage covers the controller's primary Windows processor group.")
	}
	memory := memoryStatus{Length: uint32(unsafe.Sizeof(memoryStatus{}))}
	ok, _, _ = globalMemoryStatus.Call(uintptr(unsafe.Pointer(&memory)))
	if ok != 0 && memory.TotalPhysical > 0 && memory.AvailablePhysical <= memory.TotalPhysical {
		s.Stats.Memory = &Memory{TotalBytes: memory.TotalPhysical, AvailableBytes: memory.AvailablePhysical, UsedBytes: memory.TotalPhysical - memory.AvailablePhysical}
	} else {
		s.Stats.Warnings = append(s.Stats.Warnings, "Memory availability is unavailable.")
	}
	windowsDir, _ := windows.GetWindowsDirectory()
	home, _ := os.UserHomeDir()
	for _, item := range []struct{ scope, path string }{{"system", windowsDir}, {"home", home}} {
		if e := ctx.Err(); e != nil {
			return s, e
		}
		volume := filepath.VolumeName(item.path)
		// Never contact redirected UNC homes/network drives during a local poll.
		if len(volume) != 2 || volume[1] != ':' || strings.HasPrefix(volume, `\\`) {
			s.Stats.Warnings = append(s.Stats.Warnings, "Storage information is unavailable for the "+item.scope+" drive.")
			continue
		}
		path := volume + `\`
		name, e := windows.UTF16PtrFromString(path)
		if e != nil || windows.GetDriveType(name) != windows.DRIVE_FIXED {
			s.Stats.Warnings = append(s.Stats.Warnings, "Storage information is unavailable for the "+item.scope+" drive.")
			continue
		}
		var available, total, free uint64
		if windows.GetDiskFreeSpaceEx(name, &available, &total, &free) != nil || total == 0 || available > total {
			s.Stats.Warnings = append(s.Stats.Warnings, "Storage information is unavailable for the "+item.scope+" drive.")
			continue
		}
		s.Stats.Storage = append(s.Stats.Storage, Storage{Scope: item.scope, Path: path, TotalBytes: total, AvailableBytes: available, UsedBytes: total - available})
		// GetDiskFreeSpaceEx reports quota-aware totals and availability to the
		// caller. A different physical free-space count must not be subtracted
		// from that quota-aware total (it can be larger than the total).
		if available != free {
			s.Stats.Warnings = append(s.Stats.Warnings, "The "+item.scope+" drive capacity reflects space available to this Windows account.")
		}
	}
	return s, ctx.Err()
}
