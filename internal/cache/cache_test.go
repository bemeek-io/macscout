package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoCollapsesConcurrentMisses(t *testing.T) {
	c := New[int](time.Minute)

	var calls int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]int, 20)

	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			v, err := c.Do(context.Background(), "k", func(context.Context) (int, error) {
				atomic.AddInt64(&calls, 1)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Errorf("Do: %v", err)
			}
			results[idx] = v
		}(i)
	}

	// Give the goroutines time to pile onto the same key before it resolves.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Errorf("ran the work %d times, want 1: concurrent misses must collapse", n)
	}
	for i, v := range results {
		if v != 42 {
			t.Errorf("caller %d got %d, want 42", i, v)
		}
	}
}

func TestDoServesFromCache(t *testing.T) {
	c := New[string](time.Minute)
	var calls int
	fn := func(context.Context) (string, error) {
		calls++
		return "value", nil
	}

	for i := 0; i < 3; i++ {
		if v, err := c.Do(context.Background(), "k", fn); err != nil || v != "value" {
			t.Fatalf("Do = %q, %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("ran the work %d times, want 1", calls)
	}
}

func TestEntriesExpire(t *testing.T) {
	c := New[int](20 * time.Millisecond)
	c.Set("k", 7)

	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get = %d, %v immediately after Set", v, ok)
	}
	time.Sleep(35 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Error("entry should have expired")
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	c := New[int](time.Minute)
	boom := errors.New("boom")

	var calls int
	fn := func(context.Context) (int, error) {
		calls++
		if calls == 1 {
			return 0, boom
		}
		return 5, nil
	}

	if _, err := c.Do(context.Background(), "k", fn); !errors.Is(err, boom) {
		t.Fatalf("first Do err = %v, want boom", err)
	}
	v, err := c.Do(context.Background(), "k", fn)
	if err != nil || v != 5 {
		t.Errorf("second Do = %d, %v; a failure must not be cached", v, err)
	}
}

func TestDoDetachesWorkFromCallerCancellation(t *testing.T) {
	c := New[int](time.Minute)
	started := make(chan struct{})
	finished := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = c.Do(ctx, "k", func(workCtx context.Context) (int, error) {
			close(started)
			// The shared sweep must outlive the request that kicked it off,
			// because other callers may be waiting on the same key.
			time.Sleep(60 * time.Millisecond)
			if workCtx.Err() != nil {
				t.Errorf("work context was cancelled with the caller: %v", workCtx.Err())
			}
			close(finished)
			return 9, nil
		})
	}()

	<-started
	cancel()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("work did not complete after the caller cancelled")
	}

	if v, ok := c.Get("k"); !ok || v != 9 {
		t.Errorf("result not cached after the caller left: %d, %v", v, ok)
	}
}
