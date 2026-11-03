package machinestats

import (
	"context"
	"sync"
	"time"
)

type flight struct {
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	result  Stats
	err     error
}

// Cache belongs to one connection identity. Shared reads have bounded lifetime;
// the final departing caller cancels the read. A blocked native filesystem call
// remains the sole flight rather than spawning another unbounded goroutine.
type Cache struct {
	mu       sync.Mutex
	active   *flight
	previous Sample
	result   Stats
	err      error
	at       time.Time
}

func (c *Cache) Read(ctx, owner context.Context, ttl, timeout time.Duration, collect func(context.Context) (Sample, error)) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	c.mu.Lock()
	if !c.at.IsZero() && time.Since(c.at) < ttl {
		result, err := c.result, c.err
		c.mu.Unlock()
		return result, err
	}
	f := c.active
	if f == nil {
		work, cancel := context.WithTimeout(owner, timeout)
		f = &flight{done: make(chan struct{}), ctx: work, cancel: cancel}
		c.active = f
		go func() {
			sample, err := collect(work)
			if work.Err() != nil {
				err = work.Err()
			}
			c.mu.Lock()
			f.err = err
			if err == nil {
				f.result = Calculate(sample, c.previous)
				c.previous = sample
			}
			c.result, c.err = f.result, err
			// A cancelled observer should not poison a following read's cache.
			if err != context.Canceled {
				c.at = time.Now()
			}
			c.active = nil
			close(f.done)
			c.mu.Unlock()
			cancel()
		}()
	}
	f.waiters++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		f.waiters--
		if f.waiters == 0 && c.active == f {
			f.cancel()
		}
		c.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return Stats{}, ctx.Err()
	case <-f.done:
		return f.result, f.err
	case <-f.ctx.Done():
		select {
		case <-f.done:
			return f.result, f.err
		default:
			return Stats{}, f.ctx.Err()
		}
	}
}
