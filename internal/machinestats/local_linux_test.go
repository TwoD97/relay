package machinestats

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLinuxProbeUsesLiteralPathsAndNumericStorageOutput(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "args")
	program := "#!/bin/sh\nprintf '%s\\000' \"$@\" >> \"$RELAY_STATS_ARGS\"\nprintf '1K-blocks Used Available\\n1000 200 700\\n'\n"
	if err := os.WriteFile(filepath.Join(root, "df"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "quotes ' $(touch NEVER)\nnew line")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", LinuxScript)
	cmd.Env = []string{"PATH=" + root + ":/usr/bin:/bin", "HOME=" + home, "RELAY_STATS_ARGS=" + log}
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sample, err := ParseLinux(data)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Counter == nil || sample.Stats.Memory == nil || len(sample.Stats.Storage) != 2 || sample.Stats.Storage[1].Path != home {
		t.Fatal("bounded fixed script failed", sample.Stats)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"-k", "--output=size,used,avail", "--", "/", "-k", "--output=size,used,avail", "--", home, ""}, "\x00")
	if string(args) != want {
		t.Fatalf("df arguments changed: %q", args)
	}
	if _, err := os.Stat(filepath.Join(root, "NEVER")); !os.IsNotExist(err) {
		t.Fatal("home path executed shell syntax")
	}
}

func TestLocalStatsCancellationAndRealCounterSanity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Collect(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	sample, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Counter == nil || sample.Counter.Idle > sample.Counter.Total || sample.Stats.Memory == nil || sample.Stats.Memory.AvailableBytes > sample.Stats.Memory.TotalBytes {
		t.Fatal("local OS resource counters invalid")
	}
}
