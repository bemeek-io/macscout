package apple

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrThrottled reports that apple.com is shedding this caller's traffic and the
// client is deliberately not sending more until the cooldown passes.
var ErrThrottled = errors.New("apple: rate limited, backing off")

// ThrottleError carries how long the caller should wait.
type ThrottleError struct{ RetryAfter time.Duration }

func (e *ThrottleError) Error() string {
	return fmt.Sprintf("apple is rate limiting; retry in %s", e.RetryAfter.Round(time.Second))
}

func (e *ThrottleError) Unwrap() error { return ErrThrottled }

// breaker stops a sweep the moment apple.com starts shedding load. Without it,
// a few hundred queued requests each burn their retries against a server that
// is already refusing them, which extends the block instead of riding it out.
type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time

	threshold int
	cooldown  time.Duration
}

func newBreaker() *breaker {
	return &breaker{threshold: 4, cooldown: 90 * time.Second}
}

// check reports the remaining cooldown, or zero when requests may proceed.
func (b *breaker) check() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := time.Until(b.openUntil); remaining > 0 {
		return remaining
	}
	return 0
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

// recordThrottle counts a shed request and opens the breaker once apple.com has
// refused enough of them in a row. It reports whether this call opened it.
func (b *breaker) recordThrottle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures < b.threshold {
		return false
	}
	b.openUntil = time.Now().Add(b.cooldown)
	b.failures = 0
	return true
}

// isThrottleStatus reports whether a status means "you are sending too much".
// 541 is Apple's own non-standard shed response.
func isThrottleStatus(code int) bool {
	return code == 429 || code == 503 || code == 541
}
