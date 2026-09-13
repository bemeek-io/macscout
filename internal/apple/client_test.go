package apple

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPaceStartsConservative(t *testing.T) {
	c := NewClient(0)
	if c.Pace() != startInterval {
		t.Errorf("initial pace = %v, want the timid %v starting point", c.Pace(), startInterval)
	}
	if startInterval <= minInterval {
		t.Error("the controller should start slower than its floor, so it has room to ease up")
	}
}

func TestPaceEasesUpAdditively(t *testing.T) {
	c := NewClient(1)
	c.interval = 2 * time.Second

	c.speedUp()
	if got, want := c.Pace(), 2*time.Second-speedUpStep; got != want {
		t.Errorf("pace = %v after one clean request, want %v", got, want)
	}

	// Recovery is additive, so it takes many successes to undo one backoff.
	for i := 0; i < 500; i++ {
		c.speedUp()
	}
	if c.Pace() != minInterval {
		t.Errorf("pace = %v, want it to settle at the %v floor", c.Pace(), minInterval)
	}
}

func TestPaceBacksOffMultiplicatively(t *testing.T) {
	c := NewClient(1)
	c.interval = time.Second

	c.slowDown()
	if got, want := c.Pace(), time.Duration(float64(time.Second)*slowdownFactor); got != want {
		t.Errorf("pace = %v after being shed, want %v", got, want)
	}

	for i := 0; i < 50; i++ {
		c.slowDown()
	}
	if c.Pace() > maxInterval {
		t.Errorf("pace = %v, want it capped at %v", c.Pace(), maxInterval)
	}
}

// Backing off must cost far more than one success recovers, or the controller
// would creep back into the rate that got it shed.
func TestBackoffOutweighsRecovery(t *testing.T) {
	c := NewClient(1)
	c.interval = time.Second
	before := c.Pace()

	c.slowDown()
	c.speedUp()

	if c.Pace() <= before {
		t.Errorf("pace = %v after one backoff and one recovery, want still slower than %v",
			c.Pace(), before)
	}
}

func TestBeingShedSlowsTheClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(541)
	}))
	defer srv.Close()

	c := NewClient(1, WithBaseURL(srv.URL), WithRateLimit(0))
	c.interval = 50 * time.Millisecond
	c.breaker.threshold = 2
	before := c.Pace()

	_, _ = c.Pickup(context.Background(), []string{"TEST1LL/A"}, "94301")

	if c.Pace() <= before {
		t.Errorf("pace = %v after apple.com shed the request, want slower than %v", c.Pace(), before)
	}
}

func TestSuccessSpeedsUpTheClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"body":{"stores":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(1, WithBaseURL(srv.URL), WithRateLimit(0))
	c.interval = 2 * time.Second
	before := c.Pace()

	if _, err := c.Pickup(context.Background(), []string{"TEST1LL/A"}, "94301"); err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	if c.Pace() >= before {
		t.Errorf("pace = %v after a clean request, want faster than %v", c.Pace(), before)
	}
}
