package api

import (
	"context"
	"sync"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
)

// sweepState accumulates a nationwide sweep across attempts.
type sweepState struct {
	// byPart is store availability per configuration. A sweep asks about a
	// whole batch at once, so accumulating only the primary part's results
	// meant a resumed sweep saved one configuration and silently dropped the
	// other fifteen it had already paid for.
	byPart  map[string]map[string]apple.StoreAvailability
	covers  map[string][]string
	done    map[string]bool
	updated time.Time
}

func newSweepState() *sweepState {
	return &sweepState{
		byPart:  map[string]map[string]apple.StoreAvailability{},
		covers:  map[string][]string{},
		done:    map[string]bool{},
		updated: time.Now(),
	}
}

func (s *sweepState) record(part string, stores []apple.StoreAvailability) {
	byStore, ok := s.byPart[part]
	if !ok {
		byStore = map[string]apple.StoreAvailability{}
		s.byPart[part] = byStore
	}
	for _, store := range stores {
		byStore[store.StoreNumber] = store
	}
}

func (s *sweepState) count(part string) int { return len(s.byPart[part]) }

func (s *sweepState) merge(report apple.ScanReport) {
	for part, stores := range report.ByPart {
		s.record(part, stores)
	}
	for zip, reached := range report.Covers {
		s.covers[zip] = reached
		s.done[zip] = true
	}
	s.updated = time.Now()
}

func (s *sweepState) remaining(anchors []string) []string {
	out := make([]string, 0, len(anchors))
	for _, zip := range anchors {
		if !s.done[zip] {
			out = append(out, zip)
		}
	}
	return out
}

func (s *sweepState) collect(part string) []apple.StoreAvailability {
	byStore := s.byPart[part]
	out := make([]apple.StoreAvailability, 0, len(byStore))
	for _, store := range byStore {
		out = append(out, store)
	}
	apple.SortStores(out)
	return out
}

// collectAll returns every configuration the sweep has gathered.
func (s *sweepState) collectAll() map[string][]apple.StoreAvailability {
	out := make(map[string][]apple.StoreAvailability, len(s.byPart))
	for part := range s.byPart {
		out[part] = s.collect(part)
	}
	return out
}

// sweepEvent is one thing a watcher needs to hear about.
type sweepEvent struct {
	Kind       string           `json:"kind"` // batch | waiting | done | failed
	Batch      *apple.ScanEvent `json:"batch,omitempty"`
	RetryAfter int              `json:"retryAfter,omitempty"`
	Remaining  int              `json:"remaining,omitempty"`
	Result     *ScanResult      `json:"result,omitempty"`
	Error      string           `json:"error,omitempty"`
}

const (
	// persistEvery bounds how often a running sweep writes its progress out.
	persistEvery = 4 * time.Second
	// sweepDeadline caps how long one sweep may keep retrying.
	sweepDeadline = 25 * time.Minute
	// maxStalledRounds stops a sweep that is no longer covering new anchors.
	maxStalledRounds = 6
	// earlyGiveUp applies when a sweep has covered nothing at all, where
	// persistence buys nothing.
	earlyGiveUp = 2
)

// sweepRunner drives one configuration's sweep to completion, in the
// background, across as many attempts as apple.com's rate limiting requires.
//
// The loop lives here rather than in the browser because a sweep can take
// several minutes of mostly waiting, and a tab that is reloaded, backgrounded
// or closed must not lose the anchors already covered. Watchers attach and
// detach freely; the sweep carries on either way.
type sweepRunner struct {
	// part is what a watching UI is drawing. batch is every configuration this
	// sweep asks about — Apple answers for all of them in the same requests,
	// so covering the rest is free.
	part    string
	batch   []string
	scope   string
	anchors []string

	// onRound persists what the sweep has after every round, so a long sweep
	// makes its progress readable immediately instead of only at the end. It
	// receives every configuration in the batch, since apple.com answered for
	// all of them in the same requests.
	onRound func(byPart map[string][]apple.StoreAvailability, anchors, covered int, complete bool)

	// Timings are fields rather than constants so tests can drive the same
	// loop without waiting out real apple.com cooldowns.
	throttledWait time.Duration
	transientWait time.Duration
	maxStalled    int
	deadline      time.Duration

	mu           sync.Mutex
	expectStores int
	state        *sweepState
	watchers     map[chan sweepEvent]struct{}
	running      bool
	finished     bool
	waitUntil    time.Time
	lastErr      string
	cancel       context.CancelFunc
}

func newSweepRunner(part, scope string, anchors []string) *sweepRunner {
	return &sweepRunner{
		part:          part,
		batch:         []string{part},
		scope:         scope,
		anchors:       anchors,
		state:         newSweepState(),
		watchers:      map[chan sweepEvent]struct{}{},
		throttledWait: 100 * time.Second,
		transientWait: 30 * time.Second,
		maxStalled:    maxStalledRounds,
		deadline:      sweepDeadline,
	}
}

// setBatch widens the sweep to cover other configurations at no extra cost.
// results returns everything the sweep has gathered, for every configuration
// in the batch.
func (r *sweepRunner) results() map[string][]apple.StoreAvailability {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.collectAll()
}

func (r *sweepRunner) setBatch(parts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(parts) > 0 {
		r.batch = parts
	}
}

// expected is how many stores a finished sweep should have found, or zero if
// that is not yet known.
func (r *sweepRunner) expected() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expectStores
}

// satisfied reports that the sweep has found every store its anchor set is
// known to reach.
//
// Anchors overlap heavily by design, so the last few contribute nothing but
// can still fail. Demanding all of them made "complete" unreachable, which put
// every part permanently overdue and had the rotation re-sweeping finished data
// forever. The honest test is whether the stores are in, not whether every
// redundant anchor replied.
func (r *sweepRunner) satisfied() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expectStores > 0 && r.state.count(r.part) >= r.expectStores
}

// snapshot describes the sweep as it stands, for a watcher just attaching.
func (r *sweepRunner) snapshot() (*ScanResult, time.Duration, bool) {
	r.mu.Lock()
	remaining := len(r.state.remaining(r.anchors))
	complete := remaining == 0 || (r.expectStores > 0 && r.state.count(r.part) >= r.expectStores)
	res := newScanResult(r.part, r.scope, r.state.collect(r.part), len(r.anchors), remaining, complete)
	finished, wait := r.finished, time.Until(r.waitUntil)
	r.mu.Unlock()
	return res, wait, finished
}

// await blocks until the sweep finishes or the window expires, returning the
// best result available either way. It lets a cheap one-anchor lookup answer
// directly while a long nationwide sweep still returns promptly.
func (r *sweepRunner) await(ctx context.Context, window time.Duration) *ScanResult {
	// Subscribe before snapshotting, so a sweep that finishes in between is
	// not missed.
	events := r.watch()
	defer r.unwatch(events)

	if res, _, finished := r.snapshot(); finished {
		return res
	}

	timer := time.NewTimer(window)
	defer timer.Stop()

	for {
		select {
		case ev := <-events:
			if ev.Kind == "done" {
				return ev.Result
			}
		case <-timer.C:
			res, _, _ := r.snapshot()
			return res
		case <-ctx.Done():
			res, _, _ := r.snapshot()
			return res
		}
	}
}

func (r *sweepRunner) watch() chan sweepEvent {
	ch := make(chan sweepEvent, 64)
	r.mu.Lock()
	r.watchers[ch] = struct{}{}
	r.mu.Unlock()
	return ch
}

func (r *sweepRunner) unwatch(ch chan sweepEvent) {
	r.mu.Lock()
	delete(r.watchers, ch)
	r.mu.Unlock()
}

// publish fans an event out without blocking: a watcher that cannot keep up
// misses intermediate batches, never the sweep itself.
func (r *sweepRunner) publish(ev sweepEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.watchers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// start launches the background loop if it is not already running.
func (r *sweepRunner) start(client *apple.Client, coverage *apple.CoverageIndex, national bool) {
	r.mu.Lock()
	if r.running || r.finished {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.running, r.cancel = true, cancel
	r.expectStores = coverage.ExpectedStores()
	r.mu.Unlock()

	go r.loop(ctx, client, coverage, national)
}

func (r *sweepRunner) stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *sweepRunner) loop(ctx context.Context, client *apple.Client, coverage *apple.CoverageIndex, national bool) {
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	deadline := time.Now().Add(r.deadline)
	stalled := 0

	for {
		r.mu.Lock()
		todo := r.state.remaining(r.anchors)
		covered := len(r.state.done)
		r.mu.Unlock()

		if len(todo) == 0 || r.satisfied() {
			r.finish(client, coverage, national)
			return
		}
		// A sweep that has not reached a single anchor is being refused, not
		// slowed. Grinding through the full stalled-round budget would hold
		// the rotation for the better part of an hour on one dead part; giving
		// up quickly lets the next part have a turn, and the rotation's own
		// per-part backoff decides when to come back to this one.
		giveUp := r.maxStalled
		// Nothing reached at all, or nearly everything reached already: in
		// both cases the remaining anchors are not worth grinding for.
		nearlyDone := covered > 0 && covered*100 >= len(r.anchors)*80
		if (covered == 0 || nearlyDone) && giveUp > earlyGiveUp {
			giveUp = earlyGiveUp
		}
		if ctx.Err() != nil || time.Now().After(deadline) || stalled >= giveUp {
			r.pause(stalled >= giveUp)
			return
		}

		var lastSave time.Time
		report := client.Scan(ctx, r.batch, todo, func(ev apple.ScanEvent) {
			// Fold each anchor in as it answers, rather than waiting for the
			// whole pass: a pass over 88 anchors takes minutes, and holding
			// its results back would leave the map empty for all of it.
			r.mu.Lock()
			if ev.Err == "" {
				// Every configuration in the batch, not just the primary:
				// apple.com answered for all of them in this one request.
				for part, stores := range ev.ByPart {
					r.state.record(part, stores)
				}
				r.state.covers[ev.Zip] = ev.Reached
				r.state.done[ev.Zip] = true
				r.state.updated = time.Now()
			}
			ev.Done = len(r.state.done)
			found := r.state.collectAll()
			covered := len(r.state.done)
			due := time.Since(lastSave) > persistEvery
			r.mu.Unlock()

			ev.Total = len(r.anchors)
			r.publish(sweepEvent{Kind: "batch", Batch: &ev})

			if due && r.onRound != nil && covered > 0 {
				lastSave = time.Now()
				r.onRound(found, len(r.anchors), covered, false)
			}
		})

		r.mu.Lock()
		r.state.merge(report)
		progressed := len(r.state.done) > covered
		remaining := len(r.state.remaining(r.anchors))
		found := r.state.collectAll()
		nowCovered := len(r.state.done)
		r.mu.Unlock()

		if r.onRound != nil && nowCovered > 0 {
			r.onRound(found, len(r.anchors), nowCovered, remaining == 0)
		}

		if progressed {
			stalled = 0
		} else {
			stalled++
		}
		// Stop as soon as every known store is in, rather than spending
		// requests on anchors that can only report stores already seen.
		if remaining == 0 || r.satisfied() {
			r.finish(client, coverage, national)
			return
		}

		// Wait out apple.com's cooldown, then carry on where we stopped.
		wait := r.waitFor(report, stalled)
		r.mu.Lock()
		r.waitUntil = time.Now().Add(wait)
		r.mu.Unlock()
		r.publish(sweepEvent{
			Kind:       "waiting",
			RetryAfter: int(wait.Seconds()),
			Remaining:  remaining,
		})

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			r.pause(false)
			return
		}
	}
}

// waitFor backs off further each time a round covers no new anchors.
func (r *sweepRunner) waitFor(report apple.ScanReport, stalled int) time.Duration {
	base := r.transientWait
	if report.Throttled {
		base = r.throttledWait
	}
	ceiling := 5 * time.Minute
	for i := 0; i < stalled && base < ceiling; i++ {
		base = time.Duration(float64(base) * 1.5)
	}
	return base
}

func (r *sweepRunner) finish(client *apple.Client, coverage *apple.CoverageIndex, national bool) {
	r.mu.Lock()
	r.finished = true
	r.waitUntil = time.Time{}
	covers := r.state.covers
	remaining := len(r.state.remaining(r.anchors))
	full := remaining == 0
	result := newScanResult(r.part, r.scope, r.state.collect(r.part), len(r.anchors), remaining, true)
	r.mu.Unlock()

	// Only a sweep that actually queried every anchor can recompute the cover;
	// one that stopped early has no evidence about the anchors it skipped.
	if national && full {
		coverage.Observe(covers, true)
	}
	if r.onRound != nil {
		r.mu.Lock()
		all := r.state.collectAll()
		r.mu.Unlock()
		r.onRound(all, result.Anchors, result.Covered, true)
	}
	// The sweep came through in full, so the current pace is sustainable.
	client.ResetPace()
	r.publish(sweepEvent{Kind: "done", Result: result})
}

// pause ends the loop with the sweep still incomplete, leaving its progress in
// place so pressing Scan again picks up from here.
func (r *sweepRunner) pause(stalled bool) {
	r.mu.Lock()
	r.waitUntil = time.Time{}
	remaining := len(r.state.remaining(r.anchors))
	complete := r.expectStores > 0 && r.state.count(r.part) >= r.expectStores
	result := newScanResult(r.part, r.scope, r.state.collect(r.part), len(r.anchors), remaining, complete)
	if stalled {
		r.lastErr = "apple.com is refusing more requests for now"
	}
	r.mu.Unlock()

	r.publish(sweepEvent{Kind: "done", Result: result, Error: r.lastErr})
}

// sweeps is the registry of running and recently finished sweeps.
type sweeps struct {
	mu    sync.Mutex
	byKey map[string]*sweepRunner
	ttl   time.Duration
}

func newSweeps(ttl time.Duration) *sweeps {
	return &sweeps{byKey: map[string]*sweepRunner{}, ttl: ttl}
}

// ensure returns the runner for a key, replacing one whose data has gone stale.
func (s *sweeps) ensure(key, part, scope string, anchors []string) *sweepRunner {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.byKey[key]
	if ok {
		runner.mu.Lock()
		stale := time.Since(runner.state.updated) > s.ttl
		runner.mu.Unlock()
		if !stale {
			return runner
		}
		runner.stop()
	}
	runner = newSweepRunner(part, scope, anchors)
	s.byKey[key] = runner
	return runner
}

func (s *sweeps) get(key string) (*sweepRunner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runner, ok := s.byKey[key]
	return runner, ok
}
