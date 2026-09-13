package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
	"github.com/benmeeker/macscout/internal/store"
)

// appleStub stands in for apple.com and counts the pickup requests it sees.
func appleStub(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/shop/retail/pickup-message"):
			atomic.AddInt64(hits, 1)
			zip := r.URL.Query().Get("location")
			fmt.Fprintf(w, `{"body":{"stores":[{
				"storeNumber":"R%s","storeName":"Store %s","state":"CA","city":"Testville",
				"storelatitude":37.4,"storelongitude":-122.1,
				"address":{"address2":"1 Test St","postalCode":%q},
				"partsAvailability":{"TEST1LL/A":{"pickupDisplay":"available","pickupSearchQuote":"Available Today"}}
			}]}}`, zip, zip, zip)
		case strings.HasPrefix(r.URL.Path, "/shop/delivery-message"):
			fmt.Fprint(w, `{"body":{"content":{"deliveryMessage":{"TEST1LL/A":{"regular":{
				"deliveryOptions":[{"displayName":"Express","date":"Sep 23 - Sep 30","shippingCost":"Free"}],
				"buyability":{"isBuyable":true}}}}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testDB gives each test its own dataset file.
func testDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open dataset: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	stub := appleStub(t, hits)
	client := apple.NewClient(4, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	srv := httptest.NewServer(NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil).Routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestScanEndpoint(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	resp, err := http.Get(srv.URL + "/api/scan?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("GET /api/scan: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got ScanResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Stores) != 1 || got.Summary.InStock != 1 {
		t.Errorf("result = %+v, want one in-stock store", got)
	}
	if got.Scope != "local:94301" {
		t.Errorf("scope = %q, want local:94301", got.Scope)
	}
	if got.Partial || got.Throttled {
		t.Errorf("a clean single-anchor scan should not be partial: %+v", got)
	}
}

func TestScanIsCachedAcrossRequests(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/api/scan?part=TEST1LL/A&zip=94301")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("hit apple.com %d times for three identical scans, want 1", n)
	}
}

func TestScanRequiresPart(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	resp, err := http.Get(srv.URL + "/api/scan")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestScanStreamEmitsBatchesThenDone(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	resp, err := http.Get(srv.URL + "/api/scan/stream?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	var events []string
	var doneData string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, name)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && len(events) > 0 &&
			events[len(events)-1] == "done" {
			doneData = data
		}
	}

	want := []string{"start", "batch", "done"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, events[i], want[i])
		}
	}

	var result ScanResult
	if err := json.Unmarshal([]byte(doneData), &result); err != nil {
		t.Fatalf("done payload: %v", err)
	}
	if len(result.Stores) != 1 {
		t.Errorf("done carried %d stores, want 1", len(result.Stores))
	}
}

func TestScanStreamReplaysFromCache(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	// Prime the cache through the plain endpoint.
	resp, err := http.Get(srv.URL + "/api/scan?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	resp.Body.Close()

	stream, err := http.Get(srv.URL + "/api/scan/stream?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Body.Close()

	var events []string
	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
			events = append(events, name)
		}
	}
	// A warm cache skips "start" and replays the stores in one batch.
	if len(events) != 2 || events[0] != "batch" || events[1] != "done" {
		t.Errorf("events = %v, want [batch done] from cache", events)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("hit apple.com %d times, want 1: the stream should replay the cache", n)
	}
}

func TestDeliveryEndpoint(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	resp, err := http.Get(srv.URL + "/api/delivery?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var got apple.Delivery
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Quote != "Sep 23 - Sep 30" || !got.Buyable {
		t.Errorf("delivery = %+v", got)
	}
}

func TestDeliveryRequiresBothParams(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	for _, q := range []string{"", "?part=TEST1LL/A", "?zip=94301"} {
		resp, err := http.Get(srv.URL + "/api/delivery" + q)
		if err != nil {
			t.Fatalf("GET %q: %v", q, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /api/delivery%s = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestFamiliesEndpoint(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	resp, err := http.Get(srv.URL + "/api/families")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var families []apple.Family
	if err := json.NewDecoder(resp.Body).Decode(&families); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("no families returned")
	}
	for _, f := range families {
		if f.ID == "" || f.Name == "" || f.Path == "" {
			t.Errorf("incomplete family: %+v", f)
		}
	}
}

func TestUnknownFamilyIs404(t *testing.T) {
	var hits int64
	srv := newTestServer(t, &hits)

	resp, err := http.Get(srv.URL + "/api/catalog/not-a-product")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// flakyStub answers only for the ZIPs in ok, and fails everything else. The
// set can be widened mid-test to simulate apple.com recovering.
func flakyStub(t *testing.T, ok *sync.Map) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zip := r.URL.Query().Get("location")
		if _, allowed := ok.Load(zip); !allowed {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"body":{"stores":[{
			"storeNumber":"R%s","storeName":"Store %s","state":"CA",
			"storelatitude":37.4,"storelongitude":-122.1,
			"address":{"postalCode":%q},
			"partsAvailability":{"TEST1LL/A":{"pickupDisplay":"available","pickupSearchQuote":"Available Today"}}
		}]}}`, zip, zip, zip)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingStub answers every pickup request with an upstream error.
func failingStub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUnansweredSweepIsNeverPresentedAsEmpty(t *testing.T) {
	stub := failingStub(t, http.StatusBadGateway)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	srv := httptest.NewServer(NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil).Routes())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/scan?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var got ScanResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The endpoint reports a sweep's progress rather than blocking on it, so an
	// empty result is fine — but it must be marked as unfinished, never handed
	// back as a confident "this product is in no store".
	if len(got.Stores) == 0 && !got.Partial {
		t.Errorf("zero stores reported as a complete answer: %+v", got)
	}
	if got.Covered != 0 || got.Remaining != 1 {
		t.Errorf("covered/remaining = %d/%d, want 0/1 when nothing answered",
			got.Covered, got.Remaining)
	}
}

// quicken collapses a runner's cooldowns so a test drives the same loop
// without waiting out real apple.com backoffs.
func quicken(r *sweepRunner) {
	r.throttledWait = 10 * time.Millisecond
	r.transientWait = 10 * time.Millisecond
	r.maxStalled = 1
}

// waitForSweep drains a runner's events until it reports done, so tests assert
// on a settled sweep rather than racing the background loop.
func waitForSweep(t *testing.T, runner *sweepRunner) *ScanResult {
	t.Helper()
	events := runner.watch()
	defer runner.unwatch(events)

	for {
		select {
		case ev := <-events:
			if ev.Kind == "done" {
				return ev.Result
			}
		case <-time.After(30 * time.Second):
			t.Fatal("sweep did not finish")
			return nil
		}
	}
}

func TestSweepKeepsWhatAnsweredAndReportsTheRest(t *testing.T) {
	var ok sync.Map
	ok.Store("94301", true)

	stub := flakyStub(t, &ok)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	s := NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil)

	runner := s.sweeps.ensure("k", "TEST1LL/A", "national", []string{"94301", "99999"})
	quicken(runner)
	runner.start(client, s.coverage, true)
	result := waitForSweep(t, runner)

	if !result.Partial {
		t.Errorf("result should be partial while an anchor is unanswered: %+v", result)
	}
	if result.Covered != 1 || result.Remaining != 1 {
		t.Errorf("covered/remaining = %d/%d, want 1/1", result.Covered, result.Remaining)
	}
	if len(result.Stores) != 1 {
		t.Errorf("stores = %d, want the answering anchor's result kept", len(result.Stores))
	}
}

func TestSweepFinishesOnceAppleRecovers(t *testing.T) {
	var ok sync.Map
	ok.Store("94301", true)

	stub := flakyStub(t, &ok)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	s := NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil)
	anchors := []string{"94301", "10001", "60601"}

	runner := s.sweeps.ensure("k", "TEST1LL/A", "national", anchors)
	quicken(runner)
	runner.start(client, s.coverage, true)
	first := waitForSweep(t, runner)
	if first.Remaining != 2 {
		t.Fatalf("remaining = %d after the first pass, want 2", first.Remaining)
	}

	// apple.com recovers; resuming should ask only about what is outstanding
	// and keep the store already found.
	ok.Store("10001", true)
	ok.Store("60601", true)

	runner.start(client, s.coverage, true)
	second := waitForSweep(t, runner)

	if second.Remaining != 0 || second.Partial {
		t.Errorf("resumed sweep should be complete: %+v", second)
	}
	if len(second.Stores) != 3 {
		t.Errorf("stores = %d, want 3 accumulated across both passes", len(second.Stores))
	}
}

func TestFinishedSweepDoesNotRequery(t *testing.T) {
	var hits int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		zip := r.URL.Query().Get("location")
		fmt.Fprintf(w, `{"body":{"stores":[{
			"storeNumber":"R%s","storeName":"Store %s","state":"CA",
			"storelatitude":37.4,"storelongitude":-122.1,
			"address":{"postalCode":%q},
			"partsAvailability":{"TEST1LL/A":{"pickupDisplay":"available"}}
		}]}}`, zip, zip, zip)
	}))
	t.Cleanup(stub.Close)

	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	s := NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil)
	anchors := []string{"94301", "10001"}

	runner := s.sweeps.ensure("k", "TEST1LL/A", "national", anchors)
	quicken(runner)
	runner.start(client, s.coverage, true)
	waitForSweep(t, runner)
	after := atomic.LoadInt64(&hits)

	runner.start(client, s.coverage, true)
	if _, _, finished := runner.snapshot(); !finished {
		t.Fatal("sweep should be finished")
	}
	if n := atomic.LoadInt64(&hits); n != after {
		t.Errorf("re-queried apple.com %d extra times for a finished sweep", n-after)
	}
}

func TestStopEndsASweep(t *testing.T) {
	var ok sync.Map
	stub := flakyStub(t, &ok) // answers nothing, so the sweep keeps waiting

	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	s := NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil)
	handler := httptest.NewServer(s.Routes())
	t.Cleanup(handler.Close)

	runner := s.sweeps.ensure("national|TEST1LL/A", "TEST1LL/A", "national", []string{"94301"})
	runner.throttledWait = time.Second
	runner.transientWait = time.Second
	runner.maxStalled = 50
	runner.start(client, s.coverage, true)

	resp, err := http.Post(handler.URL+"/api/scan/stop?part=TEST1LL/A", "", nil)
	if err != nil {
		t.Fatalf("POST stop: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop returned %d", resp.StatusCode)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		running := runner.running
		runner.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("sweep still running after stop")
}

func TestAvailabilityAnswersInstantlyWithoutTouchingApple(t *testing.T) {
	var hits int64
	stub := appleStub(t, &hits)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	db := testDB(t)
	srv := httptest.NewServer(NewServer(client, apple.NewCoverageIndex(""), db, nil).Routes())
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := http.Get(srv.URL + "/api/availability?part=TEST1LL/A&family=macbook-air&name=Test")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var got Availability
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The whole point of the local dataset: reads never wait on apple.com.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want an immediate answer from the dataset", elapsed)
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("hit apple.com %d times serving a dataset read", n)
	}
	// Never swept, so it must say so rather than imply the product is nowhere.
	if got.Tracked || got.SyncedAt != nil {
		t.Errorf("unsynced part reported as tracked: %+v", got)
	}
}

func TestAvailabilityServesStoredResults(t *testing.T) {
	var hits int64
	stub := appleStub(t, &hits)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	db := testDB(t)
	srv := httptest.NewServer(NewServer(client, apple.NewCoverageIndex(""), db, nil).Routes())
	t.Cleanup(srv.Close)

	seed := []apple.StoreAvailability{{
		StoreNumber: "R002", StoreName: "Palo Alto", State: "CA", City: "Palo Alto",
		Lat: 37.4, Lng: -122.1, Status: apple.StatusInStock, Quote: "Available Today",
	}}
	if err := db.SaveAvailability("TEST1LL/A", seed, 91, 91, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/availability?part=TEST1LL/A")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var got Availability
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Stores) != 1 || got.Summary.InStock != 1 {
		t.Errorf("availability = %+v, want the stored store", got)
	}
	if !got.Tracked || got.SyncedAt == nil || !got.Complete {
		t.Errorf("sync state = %+v, want a complete recent sync", got)
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("hit apple.com %d times serving stored data", n)
	}
}

func TestViewingAPartPutsItInTheRotation(t *testing.T) {
	var hits int64
	stub := appleStub(t, &hits)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	db := testDB(t)
	srv := httptest.NewServer(NewServer(client, apple.NewCoverageIndex(""), db, nil).Routes())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/availability?part=NEW1LL/A&family=macbook-air&name=Air")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	due, err := db.DueForSync(10, time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(due) != 1 || due[0].Part != "NEW1LL/A" {
		t.Errorf("due = %+v, want the just-viewed part queued for refresh", due)
	}
}

func TestFinishedSweepIsPersisted(t *testing.T) {
	var hits int64
	stub := appleStub(t, &hits)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	db := testDB(t)
	s := NewServer(client, apple.NewCoverageIndex(""), db, nil)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/scan?part=TEST1LL/A&zip=94301")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	snap, err := db.Availability("TEST1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) == 0 {
		t.Error("a finished sweep should have been written to the dataset")
	}
}

// The options list is the one read path that still needs apple.com. If a
// throttled fetch left it empty, no part number could be resolved and the
// stored availability became unreachable — the app looked completely broken
// while its database was full.
func TestCatalogFallsBackToStoredCopy(t *testing.T) {
	var live atomic.Bool
	live.Store(true)

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !live.Load() {
			w.WriteHeader(541)
			return
		}
		fmt.Fprint(w, `<html><script>
		window.PRODUCT_SELECTION_BOOTSTRAP = {
			productSelectionData: {
				"mainSections":[{"formFieldName":"chassis-dimensionColor","header":"<span>Color. </span>","view":"selectorColorNav"}],
				"mainDisplayValues":{"chassis-dimensionColor":{"variantOrder":["silver","starlight"],
					"silver":{"header":"Silver"},"starlight":{"header":"Starlight"}},"prices":{}},
				"products":[
					{"btrOrFdPartNumber":"TEST1LL/A","dimensions":{"chassis-dimensionColor":"silver"}},
					{"btrOrFdPartNumber":"TEST2LL/A","dimensions":{"chassis-dimensionColor":"starlight"}}]
			}
		};
		</script></html>`)
	}))
	t.Cleanup(stub.Close)

	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	db := testDB(t)
	srv := httptest.NewServer(NewServer(client, apple.NewCoverageIndex(""), db, nil).Routes())
	t.Cleanup(srv.Close)

	fetch := func() (int, apple.Catalog) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/catalog/macbook-air")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var cat apple.Catalog
		_ = json.NewDecoder(resp.Body).Decode(&cat)
		return resp.StatusCode, cat
	}

	status, first := fetch()
	if status != http.StatusOK || len(first.Variants) != 2 {
		t.Fatalf("live fetch: status %d, %d variants", status, len(first.Variants))
	}

	// apple.com starts refusing, and the in-memory cache is bypassed by using
	// a fresh server against the same database — as happens after a restart.
	live.Store(false)
	cold := httptest.NewServer(NewServer(
		apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0)),
		apple.NewCoverageIndex(""), db, nil).Routes())
	t.Cleanup(cold.Close)

	resp, err := http.Get(cold.URL + "/api/catalog/macbook-air")
	if err != nil {
		t.Fatalf("GET after outage: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d during an outage; the stored options should still be served",
			resp.StatusCode)
	}
	var cat apple.Catalog
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cat.Variants) != 2 || len(cat.Dimensions) != 1 {
		t.Errorf("stored catalog = %d variants / %d dimensions, want 2 / 1",
			len(cat.Variants), len(cat.Dimensions))
	}
	if resp.Header.Get("X-Macscout-Catalog-Age") == "" {
		t.Error("a stored catalog should say how old it is")
	}
}

// A sweep that reaches nothing is being refused outright. Because the rotation
// is serial, spending the full stalled-round budget on it holds every other
// part behind a dead one — with the real backoff that is minutes versus most
// of an hour.
func TestSweepGivesUpQuicklyWhenNothingIsReachable(t *testing.T) {
	stub := failingStub(t, http.StatusBadGateway)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	s := NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil)

	runner := s.sweeps.ensure("k", "TEST1LL/A", "national", []string{"94301", "10001"})
	runner.throttledWait = time.Millisecond
	runner.transientWait = time.Millisecond
	// The general budget stays generous; the early exit is what should apply.
	runner.maxStalled = 6

	events := runner.watch()
	defer runner.unwatch(events)
	runner.start(client, s.coverage, true)

	// Each round that decides to try again announces a wait, so counting them
	// counts rounds regardless of how long the HTTP layer takes.
	rounds := 0
	for done := false; !done; {
		select {
		case ev := <-events:
			switch ev.Kind {
			case "waiting":
				rounds++
			case "done":
				if ev.Result.Covered != 0 {
					t.Fatalf("covered = %d, want 0 against a stub refusing everything",
						ev.Result.Covered)
				}
				done = true
			}
		case <-time.After(60 * time.Second):
			t.Fatal("sweep never finished")
		}
	}

	if rounds > earlyGiveUp {
		t.Errorf("retried %d times having covered nothing, want at most %d", rounds, earlyGiveUp)
	}
}

// The central fix for sweeps that never finished: completeness is having every
// store the anchor set is known to reach, not having every redundant anchor
// reply. Without this a few permanently unreachable anchors kept each product
// marked unfinished forever, so the rotation re-swept complete data endlessly.
func TestSweepCompletesOnceEveryKnownStoreIsFound(t *testing.T) {
	var ok sync.Map
	ok.Store("94301", true) // only this anchor answers

	stub := flakyStub(t, &ok)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))

	// A previous full sweep learned that the anchor set reaches one store.
	coverage := apple.NewCoverageIndex("")
	coverage.Observe(map[string][]string{"94301": {"R94301"}}, true)
	if coverage.ExpectedStores() != 1 {
		t.Fatalf("ExpectedStores() = %d, want 1", coverage.ExpectedStores())
	}

	s := NewServer(client, coverage, testDB(t), nil)
	runner := s.sweeps.ensure("k", "TEST1LL/A", "national", []string{"94301", "10001", "60601"})
	quicken(runner)
	runner.start(client, coverage, true)

	result := waitForSweep(t, runner)

	if result.Partial {
		t.Errorf("sweep marked partial with every known store found: %+v", result)
	}
	if len(result.Stores) != 1 {
		t.Errorf("stores = %d, want the one known store", len(result.Stores))
	}
	// Two anchors never answered, and that is fine — they are redundant.
	if result.Remaining == 0 {
		t.Errorf("remaining = 0; the unanswered anchors should still be reported honestly")
	}
}

// A sweep that stopped early has no evidence about the anchors it skipped, so
// it must not be used to recompute which anchors are redundant.
func TestEarlyStopDoesNotShrinkTheAnchorSet(t *testing.T) {
	var ok sync.Map
	ok.Store("94301", true)

	stub := flakyStub(t, &ok)
	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))

	coverage := apple.NewCoverageIndex("")
	coverage.Observe(map[string][]string{
		"94301": {"R94301"},
		"10001": {"R10001"},
	}, true)
	before := len(coverage.Anchors(apple.SeedZips))

	s := NewServer(client, coverage, testDB(t), nil)
	runner := s.sweeps.ensure("k", "TEST1LL/A", "national", []string{"94301", "10001"})
	quicken(runner)
	runner.start(client, coverage, true)
	waitForSweep(t, runner)

	if after := len(coverage.Anchors(apple.SeedZips)); after < before {
		t.Errorf("anchor set shrank from %d to %d on a sweep that never queried them all",
			before, after)
	}
}

// A sweep asks about a whole batch at once. Accumulating only the primary
// part's results meant a resumed sweep saved one configuration and silently
// dropped the fifteen others it had already paid for.
func TestResumedSweepReturnsTheWholeBatch(t *testing.T) {
	var answering atomic.Bool
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answering.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		zip := r.URL.Query().Get("location")
		var avail []string
		for i := 0; ; i++ {
			p := r.URL.Query().Get(fmt.Sprintf("parts.%d", i))
			if p == "" {
				break
			}
			avail = append(avail, fmt.Sprintf(`%q:{"pickupDisplay":"available","pickupSearchQuote":"q"}`, p))
		}
		fmt.Fprintf(w, `{"body":{"stores":[{
			"storeNumber":"R%s","storeName":"Store %s","state":"CA",
			"storelatitude":37.4,"storelongitude":-122.1,
			"address":{"postalCode":%q},
			"partsAvailability":{%s}}]}}`, zip, zip, zip, strings.Join(avail, ","))
	}))
	t.Cleanup(stub.Close)

	client := apple.NewClient(1, apple.WithBaseURL(stub.URL), apple.WithRateLimit(0))
	s := NewServer(client, apple.NewCoverageIndex(""), testDB(t), nil)
	batch := []string{"AAA1LL/A", "BBB1LL/A", "CCC1LL/A"}

	// Collapse the runner's real-world cooldowns for the test.
	runner := s.sweeps.ensure("national|"+batch[0], batch[0], "national", []string{"94301", "10001"})
	quicken(runner)

	// First attempt: apple.com refuses, so the sweep gets nothing.
	first, cancelFirst := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelFirst()
	_, _, covered, _ := s.SweepBatch(first, batch)
	if covered != 0 {
		t.Fatalf("covered = %d on a refused sweep, want 0", covered)
	}

	// apple.com recovers and the sweep resumes on the same runner.
	answering.Store(true)
	second, cancelSecond := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelSecond()
	byPart, _, covered, _ := s.SweepBatch(second, batch)
	if covered == 0 {
		t.Fatal("resumed sweep covered nothing")
	}

	for _, part := range batch {
		if len(byPart[part]) == 0 {
			t.Errorf("%s came back empty; a batch sweep must return every configuration, got %d of %d",
				part, len(byPart), len(batch))
		}
	}
}
