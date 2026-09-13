package apple

import (
	"errors"
	"testing"
	"time"
)

func TestBreakerOpensAfterRepeatedThrottles(t *testing.T) {
	b := newBreaker()
	b.threshold = 3
	b.cooldown = time.Minute

	for i := 0; i < 2; i++ {
		b.recordThrottle()
		if wait := b.check(); wait != 0 {
			t.Fatalf("breaker opened after %d throttles, want it closed until %d", i+1, b.threshold)
		}
	}

	b.recordThrottle()
	wait := b.check()
	if wait <= 0 {
		t.Fatal("breaker should be open after hitting the threshold")
	}
	if wait > time.Minute {
		t.Errorf("cooldown = %v, want at most 1m", wait)
	}
}

func TestBreakerSuccessResetsRun(t *testing.T) {
	b := newBreaker()
	b.threshold = 3

	b.recordThrottle()
	b.recordThrottle()
	b.recordSuccess()
	b.recordThrottle()
	b.recordThrottle()

	if wait := b.check(); wait != 0 {
		t.Errorf("breaker opened at %v; a success should have reset the run", wait)
	}
}

func TestBreakerClosesAfterCooldown(t *testing.T) {
	b := newBreaker()
	b.threshold = 1
	b.cooldown = 20 * time.Millisecond

	b.recordThrottle()
	if b.check() == 0 {
		t.Fatal("breaker should be open")
	}
	time.Sleep(30 * time.Millisecond)
	if wait := b.check(); wait != 0 {
		t.Errorf("breaker still open after cooldown (%v)", wait)
	}
}

func TestThrottleErrorUnwraps(t *testing.T) {
	err := error(&ThrottleError{RetryAfter: 30 * time.Second})
	if !errors.Is(err, ErrThrottled) {
		t.Error("ThrottleError should unwrap to ErrThrottled")
	}
}

func TestIsThrottleStatus(t *testing.T) {
	for _, code := range []int{429, 503, 541} {
		if !isThrottleStatus(code) {
			t.Errorf("status %d should count as throttling", code)
		}
	}
	for _, code := range []int{200, 404, 500, 502} {
		if isThrottleStatus(code) {
			t.Errorf("status %d should not count as throttling", code)
		}
	}
}
