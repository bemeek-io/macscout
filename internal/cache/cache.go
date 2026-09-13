// Package cache provides a small TTL cache that also collapses concurrent
// misses for the same key into a single computation.
package cache

import (
	"context"
	"sync"
	"time"
)

type entry[T any] struct {
	val     T
	expires time.Time
}

type call[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// TTL is a concurrency-safe cache keyed by string. The zero value is not
// usable; construct one with New.
type TTL[T any] struct {
	mu       sync.Mutex
	items    map[string]entry[T]
	inFlight map[string]*call[T]
	ttl      time.Duration
}

func New[T any](ttl time.Duration) *TTL[T] {
	return &TTL[T]{
		items:    make(map[string]entry[T]),
		inFlight: make(map[string]*call[T]),
		ttl:      ttl,
	}
}

// Get returns a cached value and whether it was present and unexpired.
func (c *TTL[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.expires) {
		var zero T
		return zero, false
	}
	return e.val, true
}

// Forget drops a cached entry, for results that turn out not to be worth
// keeping until their TTL expires.
func (c *TTL[T]) Forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

func (c *TTL[T]) Set(key string, val T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[T]{val: val, expires: time.Now().Add(c.ttl)}
}

// Do returns the cached value for key, or runs fn to produce it. Concurrent
// callers for the same key wait on one execution rather than each doing the
// work — which matters here because a miss means a few hundred HTTP requests.
func (c *TTL[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}

	c.mu.Lock()
	if v, ok := c.items[key]; ok && time.Now().Before(v.expires) {
		c.mu.Unlock()
		return v.val, nil
	}
	if inFlight, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-inFlight.done:
			return inFlight.val, inFlight.err
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
	cl := &call[T]{done: make(chan struct{})}
	c.inFlight[key] = cl
	c.mu.Unlock()

	// The shared work runs detached from the initiating request's context so
	// one client disconnecting does not cancel the scan everyone is waiting on.
	go func() {
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		cl.val, cl.err = fn(workCtx)

		c.mu.Lock()
		if cl.err == nil {
			c.items[key] = entry[T]{val: cl.val, expires: time.Now().Add(c.ttl)}
		}
		delete(c.inFlight, key)
		c.mu.Unlock()
		close(cl.done)
	}()

	select {
	case <-cl.done:
		return cl.val, cl.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
