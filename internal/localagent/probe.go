package localagent

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

func runProbe(ctx context.Context, path string, args []string, env []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cwd, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	p, err := startProcess(command{Path: path, Args: args, Dir: cwd, Env: env}, 100, 30)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	var mu sync.Mutex
	var output []byte
	overflow := false
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, e := p.Read(buf)
			if n > 0 {
				mu.Lock()
				if len(output)+n > 256<<10 {
					overflow = true
				} else {
					output = append(output, buf[:n]...)
				}
				over := overflow
				mu.Unlock()
				if over {
					_ = p.Stop()
				}
			}
			if e != nil {
				return
			}
		}
	}()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() { code, e := p.Wait(); done <- result{code, e} }()
	var resultValue result
	select {
	case resultValue = <-done:
	case <-ctx.Done():
		_ = p.Stop()
		resultValue = <-done
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
	}
	_ = p.Close()
	<-readDone
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if overflow {
		return nil, errors.New("provider probe output exceeded 256 KiB")
	}
	if resultValue.err != nil {
		return nil, resultValue.err
	}
	if resultValue.code != 0 {
		return output, errors.New("provider command failed")
	}
	return output, nil
}
