// Package syncer keeps the local dataset fresh in the background, so the UI
// never has to wait on apple.com.
package syncer

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
	"github.com/benmeeker/macscout/internal/store"
)

// Options tune the rotation. The defaults are deliberately unhurried: nothing
// here is urgent, and the whole point is to stay well inside apple.com's
// tolerance rather than to finish quickly.
type Options struct {
	// KeepFresh is how old a part's data may get before it is swept again.
	KeepFresh time.Duration
	// ForgetAfter drops parts nobody has looked at for this long.
	ForgetAfter time.Duration
	// BetweenParts is the pause between one part's sweep and the next.
	BetweenParts time.Duration
	// IdlePoll is how often to look for work when there is none.
	IdlePoll time.Duration
	// Batch is how many parts to pull into one rotation.
	Batch int
	// CatalogMaxAge is how old stored product options may get.
	CatalogMaxAge time.Duration
}

func (o Options) withDefaults() Options {
	if o.KeepFresh == 0 {
		o.KeepFresh = 30 * time.Minute
	}
	if o.ForgetAfter == 0 {
		o.ForgetAfter = 7 * 24 * time.Hour
	}
	if o.BetweenParts == 0 {
		o.BetweenParts = 20 * time.Second
	}
	if o.IdlePoll == 0 {
		o.IdlePoll = time.Minute
	}
	if o.Batch == 0 {
		o.Batch = 12
	}
	if o.CatalogMaxAge == 0 {
		o.CatalogMaxAge = 12 * time.Hour
	}
	return o
}

// Sweeper runs one batch's sweep and returns each part's result. The API layer
// supplies this so foreground and background sweeps share one implementation,
// one client, and one pacer.
type Sweeper func(ctx context.Context, parts []string) (byPart map[string][]apple.StoreAvailability, anchors, covered int, complete bool)

// CatalogFetcher loads and stores one family's configurator options.
type CatalogFetcher func(ctx context.Context, family string) error

// LineupRefresher rediscovers which products Apple currently sells.
type LineupRefresher func(ctx context.Context) error

// Prober makes one cheap request to test whether apple.com is answering.
type Prober func(ctx context.Context) error

// Syncer cycles through the tracked parts, refreshing the stalest first.
//
// It yields to anything a person is waiting on: user-initiated sweeps go
// through the same rate-limited client, and a background rotation that kept
// running would spend the budget the person in front of the screen needs.
type Syncer struct {
	db              *store.Store
	sweep           Sweeper
	catalog         CatalogFetcher
	lineup          LineupRefresher
	probe           Prober
	familyIDs       func() []string
	opts            Options
	trigger         chan struct{}
	lineupCheckedAt time.Time

	mu          sync.Mutex
	busyWith    string
	pausedAt    time.Time
	active      int
	cancelSweep context.CancelFunc
	// outage counts consecutive sweeps that reached nothing, which means
	// apple.com is refusing this address rather than any one part being odd.
	outage      int
	stepStarted time.Time
	stepEnded   time.Time
	lastTook    time.Duration
}

func New(db *store.Store, sweep Sweeper, opts Options) *Syncer {
	return &Syncer{
		db:      db,
		sweep:   sweep,
		opts:    opts.withDefaults(),
		trigger: make(chan struct{}, 1),
	}
}

// WithCatalogs has the rotation keep every family's options stored, so a
// product can still be browsed while apple.com is refusing. Options change on
// Apple's release cadence, so this is a handful of requests a day, and without
// it a family nobody happened to open while apple.com was healthy cannot be
// opened at all while it is not.
func (s *Syncer) WithCatalogs(familyIDs func() []string, fetch CatalogFetcher, lineup LineupRefresher) *Syncer {
	s.familyIDs = familyIDs
	s.catalog = fetch
	s.lineup = lineup
	return s
}

// WithProbe supplies the cheap liveness check used while blocked.
func (s *Syncer) WithProbe(p Prober) *Syncer {
	s.probe = p
	return s
}

// Hold marks a foreground request as in flight. The returned function releases
// it. While anything is held, the rotation stands down.
func (s *Syncer) Hold() func() {
	s.mu.Lock()
	s.active++
	cancel := s.cancelSweep
	s.mu.Unlock()

	// Stand down immediately rather than at the next decision point. A sweep
	// in flight is spending the same rate limit the waiting request needs, and
	// its progress is preserved, so stopping costs nothing but the wait.
	if cancel != nil {
		cancel()
	}

	return func() {
		s.mu.Lock()
		s.active--
		if s.active < 0 {
			s.active = 0
		}
		s.pausedAt = time.Now()
		s.mu.Unlock()
	}
}

func (s *Syncer) inOutage() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outage > 0
}

func (s *Syncer) noteFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outage++
}

func (s *Syncer) noteSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outage = 0
}

// outageBackoff slows the whole rotation while nothing is getting through.
// Continuing to sweep at full cadence during a block keeps the address in the
// penalty box, which is how it stayed blocked for hours.
func (s *Syncer) outageBackoff() time.Duration {
	s.mu.Lock()
	outage := s.outage
	s.mu.Unlock()
	if outage == 0 {
		return 0
	}
	wait := 2 * time.Minute
	for i := 1; i < outage && wait < 30*time.Minute; i++ {
		wait *= 2
	}
	if wait > 30*time.Minute {
		wait = 30 * time.Minute
	}
	return wait
}

func (s *Syncer) yielding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Stay out of the way for a moment after the last foreground request too,
	// so a burst of clicks is not interleaved with background traffic.
	return s.active > 0 || time.Since(s.pausedAt) < yieldWindow
}

// yieldWindow is how long the rotation keeps out of the way after a foreground
// request finishes.
const yieldWindow = 15 * time.Second

// State describes why the rotation is or is not working, which is otherwise
// invisible: a rotation that is yielding looks exactly like one that has
// nothing to do.
type State struct {
	Busy   string `json:"busy,omitempty"`
	Holds  int    `json:"holds"`
	Yield  bool   `json:"yielding"`
	Outage int    `json:"outage"`
	// Running distinguishes a step in progress from one that has finished. A
	// sweep can take minutes, and without this an in-flight step is
	// indistinguishable from a rotation that has stopped.
	Running bool `json:"running"`
	// RunningFor is how long the current step has been going.
	RunningFor string `json:"runningFor,omitempty"`
	// IdleFor is how long since the last step finished.
	IdleFor string `json:"idleFor,omitempty"`
	// LastTook is how long the previous step ran.
	LastTook string `json:"lastTook,omitempty"`
}

func (s *Syncer) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := State{
		Busy:    s.busyWith,
		Holds:   s.active,
		Outage:  s.outage,
		Running: !s.stepStarted.IsZero(),
	}
	st.Yield = s.active > 0 || time.Since(s.pausedAt) < yieldWindow
	if st.Running {
		st.RunningFor = time.Since(s.stepStarted).Round(time.Second).String()
	} else if !s.stepEnded.IsZero() {
		st.IdleFor = time.Since(s.stepEnded).Round(time.Second).String()
	}
	if s.lastTook > 0 {
		st.LastTook = s.lastTook.Round(time.Second).String()
	}
	return st
}

// Syncing reports which part the rotation is working on, if any.
func (s *Syncer) Syncing() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyWith, s.busyWith != ""
}

// Nudge asks the rotation to look for work now rather than at the next poll.
func (s *Syncer) Nudge() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// stepCatalog stores one family's options if any are missing or stale.
func (s *Syncer) stepCatalog(ctx context.Context) (worked, outage bool) {
	if s.catalog == nil || s.familyIDs == nil {
		return false, false
	}

	// Rediscover the lineup occasionally, so products Apple adds show up and
	// ones it discontinues stop being asked about.
	if s.lineup != nil && time.Since(s.lineupCheckedAt) > s.opts.CatalogMaxAge {
		s.lineupCheckedAt = time.Now()
		if err := s.lineup(ctx); err != nil {
			if ctx.Err() != nil {
				return false, false
			}
			log.Printf("syncer: refresh lineup: %v", err)
			return true, errors.Is(err, apple.ErrThrottled)
		}
		log.Printf("syncer: lineup refreshed, %d product lines", len(s.familyIDs()))
		return true, false
	}

	families := s.familyIDs()
	if len(families) == 0 {
		return false, false
	}
	due, err := s.db.StaleCatalogs(families, s.opts.CatalogMaxAge)
	if err != nil {
		log.Printf("syncer: find stale catalogs: %v", err)
		return false, false
	}
	if len(due) == 0 {
		return false, false
	}

	family := due[0]
	err = s.catalog(ctx, family)
	if ctx.Err() != nil {
		return false, false
	}
	permanent := err != nil && !errors.Is(err, apple.ErrThrottled)
	if markErr := s.db.MarkCatalogAttempt(family, err == nil, permanent); markErr != nil {
		log.Printf("syncer: mark catalog attempt %s: %v", family, markErr)
	}
	if err != nil {
		// A page that will not parse is a problem with that one family, not
		// evidence that apple.com is refusing us, so it must not slow the
		// whole rotation the way a real outage should.
		if !permanent {
			s.noteFailure()
		}
		log.Printf("syncer: catalog %s: %v", family, err)
		return true, !permanent
	}
	s.noteSuccess()
	log.Printf("syncer: catalog %s stored (%d families still due)", family, len(due)-1)
	return true, false
}

// Run drives the rotation until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	log.Printf("syncer: refreshing tracked parts every %s, %s between parts",
		s.opts.KeepFresh, s.opts.BetweenParts)

	for {
		if ctx.Err() != nil {
			return
		}
		wait := s.opts.IdlePoll

		if !s.yielding() {
			started := time.Now()
			s.mu.Lock()
			s.stepStarted = started
			s.mu.Unlock()

			worked, outage := s.step(ctx)

			s.mu.Lock()
			s.stepStarted = time.Time{}
			s.stepEnded = time.Now()
			s.lastTook = time.Since(started)
			s.mu.Unlock()
			if worked {
				wait = s.opts.BetweenParts
				// Only an outage slows the rotation. A family whose page will
				// not parse is not evidence that apple.com is refusing us, and
				// treating it as such stalled the whole rotation for minutes
				// at a time on three dead product pages.
				if outage {
					if backoff := s.outageBackoff(); backoff > wait {
						log.Printf("syncer: nothing is getting through, pausing %s", backoff)
						wait = backoff
					}
				}
			}
		} else {
			wait = 5 * time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
		case <-time.After(wait):
		}
	}
}

// step does at most one unit of work. It reports whether it did any, and
// whether what it did suggests apple.com is refusing traffic.
func (s *Syncer) step(ctx context.Context) (worked, outage bool) {
	if err := s.db.Prune(); err != nil {
		log.Printf("syncer: prune: %v", err)
	}

	// While apple.com is refusing, check with one request rather than a whole
	// sweep. Repeatedly spending a dozen requests to rediscover a block is
	// what keeps the address blocked.
	if s.probe != nil && s.inOutage() {
		err := s.probe(ctx)
		if ctx.Err() != nil {
			return false, false
		}
		if err != nil {
			log.Printf("syncer: apple.com still refusing, holding off")
			return true, true
		}
		s.noteSuccess()
		log.Printf("syncer: apple.com answering again, resuming")
		return true, false
	}

	// A missing set of options blocks a product outright, where stale
	// availability only makes it less current, so options come first.
	if did, hitOutage := s.stepCatalog(ctx); did {
		return true, hitOutage
	}

	batch := s.nextBatch()
	if len(batch) == 0 {
		return false, false
	}

	parts := make([]string, len(batch))
	for i, t := range batch {
		parts[i] = t.Part
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	s.busyWith, s.cancelSweep = parts[0], cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busyWith, s.cancelSweep = "", nil
		s.mu.Unlock()
	}()

	byPart, anchors, covered, complete := s.sweep(ctx, parts)
	if ctx.Err() != nil {
		return false, false
	}

	for _, part := range parts {
		if err := s.db.MarkAttempt(part, covered > 0); err != nil {
			log.Printf("syncer: mark attempt %s: %v", part, err)
		}
	}

	// A sweep that reached nothing tells us nothing; recording it would
	// overwrite good data with an outage.
	if covered == 0 {
		s.noteFailure()
		log.Printf("syncer: batch of %d covered no regions, backing off", len(parts))
		return true, true
	}
	s.noteSuccess()

	saved := 0
	for _, part := range parts {
		stores := byPart[part]
		if len(stores) == 0 {
			continue
		}
		if err := s.db.SaveAvailability(part, stores, anchors, covered, complete); err != nil {
			log.Printf("syncer: save %s: %v", part, err)
			continue
		}
		saved++
	}

	log.Printf("syncer: %d configurations synced from %d/%d regions (%s…)",
		saved, covered, anchors, parts[0])
	return true, false
}

// nextBatch picks the configurations to refresh next: the stalest first, up to
// what apple.com will answer for in one request.
//
// Every configuration is refreshed, not only the ones someone has opened —
// batching makes a full pass cost about six hundred requests rather than nine
// thousand, so there is no reason to make people wait for data that could
// already be there.
func (s *Syncer) nextBatch() []store.Tracked {
	known, err := s.db.KnownParts()
	if err != nil {
		log.Printf("syncer: list configurations: %v", err)
		return nil
	}
	if len(known) == 0 {
		return nil
	}
	ages, err := s.db.SyncAges()
	if err != nil {
		log.Printf("syncer: read sync ages: %v", err)
		return nil
	}

	// Never-synced first, then oldest. Ties break on part number so the
	// rotation is deterministic rather than wandering.
	sort.Slice(known, func(i, j int) bool {
		ai, aj := ages[known[i].Part], ages[known[j].Part]
		if ai.Equal(aj) {
			return known[i].Part < known[j].Part
		}
		return ai.Before(aj)
	})

	cutoff := time.Now().Add(-s.opts.KeepFresh)
	batch := make([]store.Tracked, 0, apple.MaxPartsPerRequest)
	for _, t := range known {
		at, synced := ages[t.Part]
		if synced && at.After(cutoff) {
			break // everything from here on is fresher still
		}
		batch = append(batch, t)
		if len(batch) == apple.MaxPartsPerRequest {
			break
		}
	}
	return batch
}
