package controller

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type mastermindWorkerCapture struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *mastermindWorkerCapture) Write(data []byte) (int, error) {
	n := len(data)
	remaining := b.limit - b.buffer.Len()
	if n > remaining {
		b.overflow = true
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(data[:min(remaining, n)])
	}
	return n, nil
}

func runLocalMastermindWorker(ctx context.Context, executable string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	stdout := &mastermindWorkerCapture{limit: mastermindWorkerOutputLimit}
	stderr := &mastermindWorkerCapture{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// The trusted probe handles TERM and reaps its isolated provider probes.
	// Never send numeric process-group signals from the controller.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Run(); err != nil || stdout.overflow || stderr.overflow {
		return nil, errors.New("Mastermind worker command failed or exceeded its output limit")
	}
	return stdout.buffer.Bytes(), nil
}
