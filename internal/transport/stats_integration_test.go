//go:build !windows && !relay_ssh_native

package transport

import (
	"context"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/machinestats"
)

// Run only on the disposable container owned by TestRealSSH. In particular the
// first call occurs before any Relay runtime has been installed on that host.
func exerciseSSHMachineStats(t *testing.T, ctx context.Context, connection *Connection) {
	t.Helper()
	read := func() machinestats.Sample {
		data, err := connection.Run(ctx, machinestats.LinuxScript, nil)
		if err != nil {
			t.Fatal("read-only SSH statistics", err)
		}
		sample, err := machinestats.ParseLinux(data)
		if err != nil {
			t.Fatal(err)
		}
		if sample.Counter == nil || sample.Stats.Memory == nil || len(sample.Stats.Storage) != 2 || len(sample.Stats.Warnings) != 0 {
			t.Fatal("disposable Linux collector returned incomplete metrics")
		}
		return sample
	}
	first := read()
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := read()
	result := machinestats.Calculate(second, first)
	if result.CPU.UsagePercent == nil || *result.CPU.UsagePercent < 0 || *result.CPU.UsagePercent > 100 || result.CPU.IntervalSeconds == nil {
		t.Fatal("real SSH counter delta unavailable")
	}
	t.Log("read-only statistics over existing SSH succeeded without a runtime API")
}
