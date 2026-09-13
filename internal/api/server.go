// Package api exposes the HTTP surface the React client talks to.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
	"github.com/benmeeker/macscout/internal/cache"
	"github.com/benmeeker/macscout/internal/store"
	"github.com/benmeeker/macscout/internal/syncer"
)

// ScanResult is a completed availability sweep for one part number.
type ScanResult struct {
	Part      string                    `json:"part"`
	Scope     string                    `json:"scope"`
	Stores    []apple.StoreAvailability `json:"stores"`
	Summary   apple.Summary             `json:"summary"`
	FetchedAt time.Time                 `json:"fetchedAt"`
	// Partial marks a result that did not cover every anchor, because the
	// client disconnected or apple.com started shedding traffic.
	Partial   bool `json:"partial"`
	Throttled bool `json:"throttled"`
	Anchors   int  `json:"anchors"`
	Covered   int  `json:"covered"`
	// Remaining anchors still to query; a resumable sweep finishes across
	// several attempts rather than restarting from nothing each time.
	Remaining int `json:"remaining"`
}

// newScanResult packages an accumulated sweep for the wire.
//
// A sweep counts as partial while any anchor is still unanswered, not just if
// it stopped early: an all-errors sweep otherwise reports zero stores as though
// apple.com had confidently said the product is nowhere to be found.
func newScanResult(part, scope string, stores []apple.StoreAvailability, anchors, remaining int, complete bool) *ScanResult {
	return &ScanResult{
		Part:      part,
		Scope:     scope,
		Stores:    stores,
		Summary:   apple.Summarize(stores),
		FetchedAt: time.Now(),
		Partial:   !complete,
		Anchors:   anchors,
		Covered:   anchors - remaining,
		Remaining: remaining,
	}
}

type Server struct {
	client   *apple.Client
	catalogs *cache.TTL[*apple.Catalog]
	scans    *cache.TTL[*ScanResult]
	sweeps   *sweeps
	coverage *apple.CoverageIndex
	db       *store.Store
	syncer   *syncer.Syncer
	static   fs.FS
}

func NewServer(client *apple.Client, coverage *apple.CoverageIndex, db *store.Store, static fs.FS) *Server {
	if coverage == nil {
		coverage = apple.NewCoverageIndex("")
	}
	return &Server{
		db:       db,
		client:   client,
		coverage: coverage,
		// Catalogs turn over on Apple's release cadence, not by the minute.
		catalogs: cache.New[*apple.Catalog](6 * time.Hour),
		// Availability is the whole point, so keep it fresh but not so fresh
		// that a page refresh re-runs 150 requests against Apple.
		scans: cache.New[*ScanResult](5 * time.Minute),
		// A sweep may span several throttle windows, but not so many that the
		// first anchors go stale against the last.
		sweeps: newSweeps(30 * time.Minute),
		static: static,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/families", s.handleFamilies)
	mux.HandleFunc("GET /api/catalog/{family}", s.handleCatalog)
	mux.HandleFunc("GET /api/availability", s.handleAvailability)
	mux.HandleFunc("GET /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/scan/stream", s.handleScanStream)
	mux.HandleFunc("POST /api/scan/stop", s.handleScanStop)
	mux.HandleFunc("GET /api/delivery", s.handleDelivery)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/status", s.handleStatus)
	if s.static != nil {
		mux.Handle("/", s.spaHandler())
	}
	return logRequests(mux)
}

func (s *Server) handleFamilies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Families())
}

// Families is the product lineup: what was last discovered from apple.com, or
// the built-in list if discovery has never succeeded. The built-in list is a
// fallback rather than the source of truth, because Apple's lineup changes and
// a frozen list quietly asks about discontinued products while missing new
// ones.
func (s *Server) Families() []apple.Family {
	families := apple.Families
	if s.db != nil {
		if payload, _, err := s.db.Families(); err == nil {
			var discovered []apple.Family
			if err := json.Unmarshal(payload, &discovered); err == nil && len(discovered) > 0 {
				families = discovered
			}
		}
	}
	return s.withoutDeadFamilies(families)
}

// unparseableStreak is how many parse failures make a product line dead rather
// than unlucky.
const unparseableStreak = 3

// withoutDeadFamilies drops product lines whose page has repeatedly failed to
// parse. Apple keeps linking discontinued models, and pages that are not really
// a configurable product, and offering those only dead-ends a reader.
func (s *Server) withoutDeadFamilies(families []apple.Family) []apple.Family {
	if s.db == nil {
		return families
	}
	dead, err := s.db.UnparseableFamilies(unparseableStreak)
	if err != nil || len(dead) == 0 {
		return families
	}
	out := make([]apple.Family, 0, len(families))
	for _, f := range families {
		if !dead[f.ID] {
			out = append(out, f)
		}
	}
	// Never hide everything: if the whole lineup looks dead, something else is
	// wrong and an empty picker helps nobody.
	if len(out) == 0 {
		return families
	}
	return out
}

// RefreshFamilies rediscovers the lineup and stores it.
func (s *Server) RefreshFamilies(ctx context.Context) error {
	families, err := s.client.DiscoverFamilies(ctx)
	if err != nil {
		return err
	}
	if s.db == nil {
		return nil
	}
	payload, err := json.Marshal(families)
	if err != nil {
		return err
	}
	return s.db.SaveFamilies(payload)
}

// FamilyIDs lists the lineup's identifiers, for the rotation.
func (s *Server) FamilyIDs() []string {
	families := s.Families()
	out := make([]string, 0, len(families))
	for _, f := range families {
		out = append(out, f.ID)
	}
	return out
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("family")
	f, ok := s.familyByID(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown product family %q", id)
		return
	}
	// Loading the options is a person waiting, so it outranks the rotation.
	defer s.holdSyncer()()

	cat, err := s.catalogs.Do(r.Context(), id, func(ctx context.Context) (*apple.Catalog, error) {
		fresh, err := s.client.FetchCatalog(ctx, f)
		if err != nil {
			return nil, err
		}
		s.rememberCatalog(id, fresh)
		return fresh, nil
	})
	if err == nil {
		writeJSON(w, http.StatusOK, cat)
		return
	}

	// apple.com is unreachable or refusing. Fall back to the last good copy —
	// the options list changes on Apple's release cadence, so a stored one is
	// almost always still correct, and without it the whole app is unusable
	// even though the availability data is sitting in the database.
	if stored, at, storeErr := s.storedCatalog(id); storeErr == nil {
		log.Printf("api: serving stored %s catalog from %s (live fetch failed: %v)",
			f.Name, at.Format(time.RFC3339), err)
		w.Header().Set("X-Macscout-Catalog-Age", time.Since(at).Round(time.Minute).String())
		writeJSON(w, http.StatusOK, stored)
		return
	}

	writeErr(w, http.StatusBadGateway, "could not load %s from apple.com: %v", f.Name, err)
}

// zipsFor resolves the request's scope into the anchor ZIPs to sweep. A
// nationwide sweep walks the anchor grid — reduced to its learned minimum once
// one full sweep has run — while a local one asks about a single postal code,
// which Apple answers with its ~12 nearest stores.
func (s *Server) zipsFor(r *http.Request) ([]string, string, bool, error) {
	zip := strings.TrimSpace(r.URL.Query().Get("zip"))
	if zip == "" {
		anchors := s.coverage.Anchors(apple.SeedZips)
		if n, ok := s.coverage.Reduced(); ok {
			log.Printf("nationwide sweep using %d learned anchors instead of %d", n, len(apple.SeedZips))
		}
		return anchors, "national", true, nil
	}
	if len(zip) < 3 {
		return nil, "", false, fmt.Errorf("zip %q is too short", zip)
	}
	return []string{zip}, "local:" + zip, false, nil
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	part := strings.TrimSpace(r.URL.Query().Get("part"))
	if part == "" {
		writeErr(w, http.StatusBadRequest, "part is required")
		return
	}
	zips, scope, national, err := s.zipsFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	key := scope + "|" + part
	if cached, ok := s.scans.Get(key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	defer s.holdSyncer()()

	runner := s.runnerFor(key, part, scope, zips)
	runner.start(s.client, s.coverage, national)

	// A one-anchor lookup finishes in about a second, so wait for it rather
	// than making the caller poll. A nationwide sweep can take minutes, so
	// after this window the sweep carries on in the background and the caller
	// gets its progress so far — marked partial, never as a finished answer.
	res := runner.await(r.Context(), scanWait)
	s.cacheIfComplete(key, res)
	writeJSON(w, http.StatusOK, res)
}

// handleScanStop ends a running sweep, leaving whatever it has already found.
func (s *Server) handleScanStop(w http.ResponseWriter, r *http.Request) {
	part := strings.TrimSpace(r.URL.Query().Get("part"))
	_, scope, _, err := s.zipsFor(r)
	if part == "" || err != nil {
		writeErr(w, http.StatusBadRequest, "part is required")
		return
	}
	if runner, ok := s.sweeps.get(scope + "|" + part); ok {
		runner.stop()
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

// scanWait is how long GET /api/scan will hold a request open before handing
// back a still-running sweep's progress.
const scanWait = 20 * time.Second

// Availability is what the UI reads: the stored dataset plus enough context to
// say how old it is and whether anything is refreshing it right now.
type Availability struct {
	Part     string                    `json:"part"`
	Stores   []apple.StoreAvailability `json:"stores"`
	Summary  apple.Summary             `json:"summary"`
	SyncedAt *time.Time                `json:"syncedAt"`
	Complete bool                      `json:"complete"`
	Anchors  int                       `json:"anchors"`
	Covered  int                       `json:"covered"`
	// Syncing is true while a sweep for this part is in flight.
	Syncing bool `json:"syncing"`
	// Tracked is false before the first sweep has ever run for this part.
	Tracked bool `json:"tracked"`
}

// familyByID looks up a family in the discovered lineup, falling back to the
// built-in list.
func (s *Server) familyByID(id string) (apple.Family, bool) {
	for _, f := range s.Families() {
		if f.ID == id {
			return f, true
		}
	}
	return apple.FamilyByID(id)
}

// rememberCatalog keeps the last good configurator data for a family.
func (s *Server) rememberCatalog(id string, cat *apple.Catalog) {
	if s.db == nil {
		return
	}
	payload, err := json.Marshal(cat)
	if err != nil {
		return
	}
	if err := s.db.SaveCatalog(id, payload); err != nil {
		log.Printf("api: remember %s catalog: %v", id, err)
	}
}

func (s *Server) storedCatalog(id string) (*apple.Catalog, time.Time, error) {
	if s.db == nil {
		return nil, time.Time{}, store.ErrNoCatalog
	}
	payload, at, err := s.db.Catalog(id)
	if err != nil {
		return nil, time.Time{}, err
	}
	var cat apple.Catalog
	if err := json.Unmarshal(payload, &cat); err != nil {
		return nil, time.Time{}, err
	}
	return &cat, at, nil
}

// Status is how far the background rotation has got, so "is it done yet" is
// answerable from the page instead of by reading the server log.
type Status struct {
	Lineup         int    `json:"lineup"`
	Configurations int    `json:"configurations"`
	Synced         int    `json:"synced"`
	Complete       int    `json:"complete"`
	Syncing        string `json:"syncing,omitempty"`
	// Throttled means apple.com is currently refusing us, which is the usual
	// reason progress has stalled.
	Throttled bool `json:"throttled"`
	// Rotation exposes why the background worker is or is not running. A
	// rotation that is standing down looks identical to one with nothing to
	// do, and telling those apart from outside is otherwise impossible.
	Rotation *syncer.State `json:"rotation,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := Status{Lineup: len(s.Families())}
	if s.db != nil {
		if p, err := s.db.Coverage(); err == nil {
			out.Configurations, out.Synced, out.Complete = p.Configurations, p.Synced, p.Complete
		}
	}
	if s.syncer != nil {
		if part, busy := s.syncer.Syncing(); busy {
			out.Syncing = part
		}
		st := s.syncer.State()
		out.Rotation = &st
	}
	out.Throttled = s.client.Throttled()
	writeJSON(w, http.StatusOK, out)
}

// handleAvailability answers instantly from the local dataset. Everything slow
// happens in the background; this endpoint never waits on apple.com.
func (s *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	part := strings.TrimSpace(r.URL.Query().Get("part"))
	if part == "" {
		writeErr(w, http.StatusBadRequest, "part is required")
		return
	}
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "no local dataset configured")
		return
	}

	// Looking at a configuration is what puts it in the refresh rotation.
	if err := s.db.View(part,
		r.URL.Query().Get("family"),
		r.URL.Query().Get("name"),
		r.URL.Query().Get("price")); err != nil {
		log.Printf("api: record view of %s: %v", part, err)
	}

	snap, err := s.db.Availability(part)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read dataset: %v", err)
		return
	}

	out := Availability{
		Part:     part,
		Stores:   snap.Stores,
		Summary:  apple.Summarize(snap.Stores),
		Complete: snap.Complete,
		Anchors:  snap.Anchors,
		Covered:  snap.Covered,
		Tracked:  !snap.SyncedAt.IsZero(),
	}
	if !snap.SyncedAt.IsZero() {
		at := snap.SyncedAt
		out.SyncedAt = &at
	}
	if s.syncer != nil {
		if busy, ok := s.syncer.Syncing(); ok && busy == part {
			out.Syncing = true
		}
	}
	if runner, ok := s.sweeps.get("national|" + part); ok {
		if _, _, finished := runner.snapshot(); !finished {
			out.Syncing = true
		}
	}
	// Nothing stored yet means the rotation has not reached this part; ask it
	// to look now rather than at its next poll.
	if !out.Tracked && s.syncer != nil {
		s.syncer.Nudge()
	}

	writeJSON(w, http.StatusOK, out)
}

// runnerFor returns the sweep runner for a key, wired to write each round's
// progress into the dataset as it lands.
func (s *Server) runnerFor(key, part, scope string, anchors []string) *sweepRunner {
	runner := s.sweeps.ensure(key, part, scope, anchors)
	if runner.onRound != nil || s.db == nil {
		return runner
	}

	national := scope == "national"
	runner.onRound = func(byPart map[string][]apple.StoreAvailability, total, covered int, complete bool) {
		for p, stores := range byPart {
			if len(stores) == 0 {
				continue
			}
			var err error
			if national {
				err = s.db.SaveAvailability(p, stores, total, covered, complete)
			} else {
				// A ZIP check refreshes the stores it saw, but says nothing
				// about national coverage, so it must not touch the sync
				// record.
				err = s.db.SaveObservations(p, stores)
			}
			if err != nil {
				log.Printf("api: persist %s: %v", p, err)
			}
		}
	}
	return runner
}

// holdSyncer stands the background rotation down for the duration of a
// foreground request, so a person waiting on a result gets the request budget.
func (s *Server) holdSyncer() func() {
	if s.syncer == nil {
		return func() {}
	}
	return s.syncer.Hold()
}

// AttachSyncer wires the background rotation in after construction, since the
// syncer needs a sweep function that closes over this server.
func (s *Server) AttachSyncer(sy *syncer.Syncer) { s.syncer = sy }

// FetchCatalog loads a family's options and stores them. It is the rotation's
// entry point for keeping every product browsable during an outage.
func (s *Server) FetchCatalog(ctx context.Context, familyID string) error {
	f, ok := s.familyByID(familyID)
	if !ok {
		return fmt.Errorf("unknown product family %q", familyID)
	}
	cat, err := s.client.FetchCatalog(ctx, f)
	if err != nil {
		return err
	}
	s.rememberCatalog(familyID, cat)
	s.catalogs.Set(familyID, cat)
	return nil
}

// Probe makes the smallest possible request to see whether apple.com is
// answering again: one part, one anchor.
//
// While blocked, a full batch sweep spends a dozen requests only to rediscover
// the block, and those requests are the very thing keeping the address in the
// penalty box. Checking with one request costs almost nothing and gives the
// budget room to recover.
func (s *Server) Probe(ctx context.Context) error {
	anchors := s.coverage.Anchors(apple.SeedZips)
	if len(anchors) == 0 {
		return fmt.Errorf("no anchors configured")
	}
	part := ""
	if s.db != nil {
		if known, err := s.db.KnownParts(); err == nil && len(known) > 0 {
			part = known[0].Part
		}
	}
	if part == "" {
		return fmt.Errorf("no configurations known yet")
	}
	_, err := s.client.Pickup(ctx, []string{part}, anchors[0])
	return err
}

// SweepBatch runs a nationwide sweep for a batch of configurations and returns
// each one's result. Apple answers for every requested part from the same
// requests, so the batch costs what a single configuration would.
//
// It is the background rotation's entry point, sharing the same runner, client
// and pacer as a user-initiated sweep.
func (s *Server) SweepBatch(ctx context.Context, parts []string) (map[string][]apple.StoreAvailability, int, int, bool) {
	if len(parts) == 0 {
		return nil, 0, 0, false
	}
	anchors := s.coverage.Anchors(apple.SeedZips)
	part := parts[0]
	key := "national|" + part

	runner := s.runnerFor(key, part, "national", anchors)
	runner.setBatch(parts)

	runner.start(s.client, s.coverage, true)

	res := runner.await(ctx, sweepDeadline)
	// A cancelled caller means someone is waiting on apple.com in the
	// foreground, so stop the sweep rather than leaving it competing for the
	// same budget. Its progress is already persisted and will be resumed.
	if ctx.Err() != nil {
		runner.stop()
	}
	if res == nil {
		return nil, len(anchors), 0, false
	}
	// Read every configuration from the runner's accumulated state, so a sweep
	// resumed across attempts still returns the whole batch rather than only
	// the part it was keyed on.
	return runner.results(), res.Anchors, res.Covered, !res.Partial
}

func (s *Server) handleDelivery(w http.ResponseWriter, r *http.Request) {
	part := strings.TrimSpace(r.URL.Query().Get("part"))
	zip := strings.TrimSpace(r.URL.Query().Get("zip"))
	if part == "" || zip == "" {
		writeErr(w, http.StatusBadRequest, "part and zip are required")
		return
	}
	d, err := s.client.Delivery(r.Context(), part, zip)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "delivery lookup failed: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s%s %s", r.Method, r.URL.Path, querySuffix(r), time.Since(start).Round(time.Millisecond))
		}
	})
}

func querySuffix(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}
