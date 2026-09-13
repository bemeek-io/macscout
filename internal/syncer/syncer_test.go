package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
	"github.com/benmeeker/macscout/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedCatalog registers a family's configurations, which is what the rotation
// works from — every configuration Apple sells, not only ones someone opened.
func seedCatalog(t *testing.T, db *store.Store, family string, parts ...string) {
	t.Helper()
	type variant struct {
		Part string `json:"part"`
		Name string `json:"name"`
	}
	cat := struct {
		Variants []variant `json:"variants"`
	}{}
	for _, p := range parts {
		cat.Variants = append(cat.Variants, variant{Part: p, Name: "Test " + p})
	}
	payload, err := json.Marshal(cat)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if err := db.SaveCatalog(family, payload); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}
}

func fastOptions() Options {
	return Options{
		KeepFresh:     time.Hour,
		ForgetAfter:   time.Hour,
		BetweenParts:  time.Millisecond,
		IdlePoll:      5 * time.Millisecond,
		Batch:         5,
		CatalogMaxAge: time.Hour,
	}
}

func oneStore(number string) apple.StoreAvailability {
	return apple.StoreAvailability{
		StoreNumber: number, StoreName: "Store " + number, State: "CA",
		Status: apple.StatusInStock, Lat: 37.4, Lng: -122.1,
	}
}

// The point of the background rotation: everything has data, so a reader can
// click between configurations and see results immediately.
func TestRotationSweepsEveryConfigurationNotJustViewedOnes(t *testing.T) {
	db := openStore(t)
	parts := []string{"A1LL/A", "B1LL/A", "C1LL/A"}
	seedCatalog(t, db, "macbook-air", parts...)

	var swept atomic.Value
	s := New(db, func(_ context.Context, batch []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		swept.Store(append([]string(nil), batch...))
		out := map[string][]apple.StoreAvailability{}
		for _, p := range batch {
			out[p] = []apple.StoreAvailability{oneStore("R" + p[:1])}
		}
		return out, 35, 35, true
	}, fastOptions())

	if worked, _ := s.step(context.Background()); !worked {
		t.Fatal("step reported no work with configurations waiting")
	}

	got, _ := swept.Load().([]string)
	if len(got) != len(parts) {
		t.Errorf("swept %v, want all %d configurations in one batch", got, len(parts))
	}
	for _, p := range parts {
		snap, err := db.Availability(p)
		if err != nil {
			t.Fatalf("Availability(%s): %v", p, err)
		}
		if len(snap.Stores) == 0 {
			t.Errorf("%s has no stored availability; nobody opened it, which is the point", p)
		}
	}
}

// Apple answers a bounded number of parts per request, so a batch must not
// exceed it however many configurations are waiting.
func TestBatchNeverExceedsWhatAppleAccepts(t *testing.T) {
	db := openStore(t)
	parts := make([]string, apple.MaxPartsPerRequest*3)
	for i := range parts {
		parts[i] = fmt.Sprintf("P%03dLL/A", i)
	}
	seedCatalog(t, db, "ipad-air", parts...)

	var size atomic.Int64
	s := New(db, func(_ context.Context, batch []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		size.Store(int64(len(batch)))
		return nil, 35, 35, true
	}, fastOptions())

	s.step(context.Background())

	if n := size.Load(); n == 0 || n > int64(apple.MaxPartsPerRequest) {
		t.Errorf("batch of %d, want between 1 and %d", n, apple.MaxPartsPerRequest)
	}
}

func TestStalestConfigurationsGoFirst(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "FRESH1LL/A", "NEVER1LL/A")
	if err := db.SaveAvailability("FRESH1LL/A", []apple.StoreAvailability{oneStore("R1")}, 35, 35, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var swept atomic.Value
	s := New(db, func(_ context.Context, batch []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		swept.Store(append([]string(nil), batch...))
		return nil, 35, 35, true
	}, fastOptions())

	s.step(context.Background())

	got, _ := swept.Load().([]string)
	if len(got) == 0 || got[0] != "NEVER1LL/A" {
		t.Errorf("batch = %v, want the never-synced configuration first", got)
	}
}

func TestNothingDueWhenEverythingIsFresh(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")
	if err := db.SaveAvailability("A1LL/A", []apple.StoreAvailability{oneStore("R1")}, 35, 35, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := New(db, func(context.Context, []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		t.Error("swept a configuration that was already fresh")
		return nil, 0, 0, false
	}, fastOptions())

	if worked, _ := s.step(context.Background()); worked {
		t.Error("step claimed work when everything was fresh")
	}
}

// An outage says nothing about availability, so it must not overwrite data.
func TestFailedSweepLeavesPreviousDataAlone(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")
	if err := db.SaveAvailability("A1LL/A", []apple.StoreAvailability{oneStore("R1")}, 35, 35, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	opts := fastOptions()
	opts.KeepFresh = -time.Second // force it due
	s := New(db, func(context.Context, []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		return nil, 35, 0, false // apple.com refused everything
	}, opts)

	s.step(context.Background())

	snap, err := db.Availability("A1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 1 {
		t.Errorf("stores = %d, want the earlier good data kept", len(snap.Stores))
	}
}

// The person in front of the screen gets the request budget, not the rotation.
func TestRotationStandsDownWhileAForegroundRequestIsHeld(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")

	var swept atomic.Int64
	s := New(db, func(context.Context, []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		swept.Add(1)
		return nil, 35, 35, true
	}, fastOptions())

	release := s.Hold()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.Run(ctx) }()

	time.Sleep(60 * time.Millisecond)
	if n := swept.Load(); n != 0 {
		t.Errorf("rotation ran %d sweeps while a foreground request was held", n)
	}

	release()
	cancel()
	wg.Wait()
}

// A sweep in flight spends the same rate limit a waiting person needs, so it
// has to stop when one arrives rather than at its next convenient moment.
func TestHoldStopsASweepAlreadyInFlight(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")

	started := make(chan struct{})
	cancelled := make(chan struct{})
	s := New(db, func(ctx context.Context, _ []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		close(started)
		<-ctx.Done() // a real sweep would be mid-request here
		close(cancelled)
		return nil, 35, 0, false
	}, fastOptions())

	go func() { s.step(context.Background()) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep never started")
	}

	release := s.Hold()
	defer release()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("holding for a foreground request did not stop the in-flight sweep")
	}
}

func TestSyncingReportsCurrentWork(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")

	started := make(chan struct{})
	finish := make(chan struct{})
	s := New(db, func(context.Context, []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		close(started)
		<-finish
		return nil, 35, 35, true
	}, fastOptions())

	go func() { s.step(context.Background()) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep never started")
	}

	part, busy := s.Syncing()
	if !busy || part != "A1LL/A" {
		t.Errorf("Syncing() = %q, %v; want A1LL/A, true", part, busy)
	}
	close(finish)
}

// While blocked, rediscovering the block with a full sweep spends the very
// budget that would let it recover. One request is enough to ask.
func TestBlockedRotationProbesInsteadOfSweeping(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")

	var sweeps, probes atomic.Int64
	s := New(db, func(context.Context, []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		sweeps.Add(1)
		return nil, 35, 0, false // apple.com refuses
	}, fastOptions())
	s.WithProbe(func(context.Context) error {
		probes.Add(1)
		return fmt.Errorf("still refusing")
	})

	// First step sweeps and discovers the block.
	if _, outage := s.step(context.Background()); !outage {
		t.Fatal("a sweep that reached nothing should report an outage")
	}
	if sweeps.Load() != 1 {
		t.Fatalf("sweeps = %d, want 1", sweeps.Load())
	}

	// Subsequent steps should probe, not sweep again.
	for i := 0; i < 3; i++ {
		s.step(context.Background())
	}
	if n := sweeps.Load(); n != 1 {
		t.Errorf("swept %d times while blocked, want to stop after the first", n)
	}
	if n := probes.Load(); n != 3 {
		t.Errorf("probed %d times, want one per step while blocked", n)
	}
}

func TestSuccessfulProbeResumesNormalWork(t *testing.T) {
	db := openStore(t)
	seedCatalog(t, db, "macbook-air", "A1LL/A")

	var sweeps atomic.Int64
	var answering atomic.Bool
	s := New(db, func(context.Context, []string) (map[string][]apple.StoreAvailability, int, int, bool) {
		sweeps.Add(1)
		if !answering.Load() {
			return nil, 35, 0, false
		}
		return map[string][]apple.StoreAvailability{"A1LL/A": {oneStore("R1")}}, 35, 35, true
	}, fastOptions())
	s.WithProbe(func(context.Context) error {
		if answering.Load() {
			return nil
		}
		return fmt.Errorf("still refusing")
	})

	s.step(context.Background()) // sweep fails, outage recorded
	answering.Store(true)

	if _, outage := s.step(context.Background()); outage {
		t.Error("a successful probe should clear the outage")
	}
	s.step(context.Background()) // now free to sweep again

	if sweeps.Load() < 2 {
		t.Error("rotation did not resume sweeping after apple.com came back")
	}
}
