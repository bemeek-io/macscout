package apple

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a fast, unthrottled client at a stub server.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return NewClient(4, WithBaseURL(srv.URL), WithRateLimit(0))
}

// storeJSON renders one store in the shape pickup-message returns.
func storeJSON(number, name, state, display, encoded string) string {
	return fmt.Sprintf(`{
		"storeNumber": %q, "storeName": %q, "state": %q, "city": "Testville",
		"storelatitude": 37.4, "storelongitude": -122.1,
		"address": {"address2": "1 Test St", "postalCode": "94301"},
		"pickupEncodedUpperDateString": %q,
		"partsAvailability": {"TEST1LL/A": {"pickupDisplay": %q, "pickupSearchQuote": "quote"}}
	}`, number, name, state, encoded, display)
}

func pickupBody(stores ...string) string {
	return `{"body":{"stores":[` + strings.Join(stores, ",") + `]}}`
}

func TestScanDeduplicatesAcrossAnchors(t *testing.T) {
	// Two anchors whose neighbourhoods overlap on R002.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("location") {
		case "94301":
			fmt.Fprint(w, pickupBody(
				storeJSON("R002", "Palo Alto", "CA", "available", ""),
				storeJSON("R099", "Stanford", "CA", "ships-to-store", ""),
			))
		default:
			fmt.Fprint(w, pickupBody(
				storeJSON("R002", "Palo Alto", "CA", "available", ""),
				storeJSON("R075", "Union Square", "CA", "unavailable", ""),
			))
		}
	}))

	var batches int
	report := c.Scan(context.Background(), []string{"TEST1LL/A"}, []string{"94301", "94105"}, func(ScanEvent) {
		batches++
	})

	if len(report.Stores) != 3 {
		t.Fatalf("got %d stores, want 3 after dedupe: %+v", len(report.Stores), report.Stores)
	}
	if batches != 2 {
		t.Errorf("emitted %d batches, want one per anchor", batches)
	}
	if report.Completed != 2 || report.Failed != 0 || report.Throttled {
		t.Errorf("report = %+v, want a clean sweep", report)
	}

	seen := map[string]bool{}
	for _, s := range report.Stores {
		if seen[s.StoreNumber] {
			t.Errorf("store %s reported twice", s.StoreNumber)
		}
		seen[s.StoreNumber] = true
	}
}

func TestScanStopsWhenThrottled(t *testing.T) {
	var requests int64
	// Every response sheds load, so the breaker should trip and the sweep
	// should abandon the remaining anchors instead of retrying them all.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.WriteHeader(541)
	}))
	c.breaker.threshold = 2
	c.breaker.cooldown = time.Minute

	zips := make([]string, 60)
	for i := range zips {
		zips[i] = fmt.Sprintf("%05d", 10000+i)
	}

	done := make(chan ScanReport, 1)
	go func() { done <- c.Scan(context.Background(), []string{"TEST1LL/A"}, zips, nil) }()

	select {
	case report := <-done:
		if !report.Throttled {
			t.Error("report should be marked throttled")
		}
		if report.Completed >= len(zips) {
			t.Errorf("swept %d of %d anchors; the breaker should have cut it short",
				report.Completed, len(zips))
		}
		// Bounded by attempts for the handful of requests before the trip,
		// not by the full 60-anchor list.
		if n := atomic.LoadInt64(&requests); n > 40 {
			t.Errorf("sent %d requests after being throttled; expected the sweep to stop early", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("scan did not return; a throttled sweep must not hang")
	}
}

func TestScanSurvivesOneBadAnchor(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("location") == "00000" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, pickupBody(storeJSON("R002", "Palo Alto", "CA", "available", "")))
	}))

	report := c.Scan(context.Background(), []string{"TEST1LL/A"}, []string{"00000", "94301"}, nil)
	if report.Failed != 1 {
		t.Errorf("failed = %d, want 1", report.Failed)
	}
	if len(report.Stores) != 1 {
		t.Errorf("got %d stores, want the one good anchor's result", len(report.Stores))
	}
	if report.Throttled {
		t.Error("a 404 is not throttling")
	}
}

func TestPickupParsesStatusAndLinks(t *testing.T) {
	tomorrow := time.Now().AddDate(0, 0, 1).Format("20060102")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, pickupBody(
			storeJSON("R002", "Palo Alto", "CA", "available", ""),
			storeJSON("R099", "Stanford", "CA", "ships-to-store", tomorrow),
		))
	}))

	byPart, err := c.Pickup(context.Background(), []string{"TEST1LL/A"}, "94301")
	if err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	stores := byPart["TEST1LL/A"]
	if len(stores) != 2 {
		t.Fatalf("got %d stores, want 2", len(stores))
	}
	if stores[0].Status != StatusInStock {
		t.Errorf("status = %q, want in_stock", stores[0].Status)
	}
	if stores[1].DaysOut != 1 || stores[1].Status != StatusInStock {
		t.Errorf("next-day ship-to-store = %q at %d days", stores[1].Status, stores[1].DaysOut)
	}
	if !strings.Contains(stores[0].PickupURL, "TEST1LL/A") ||
		!strings.Contains(stores[0].PickupURL, "R002") {
		t.Errorf("pickup url = %q, want the part and store in it", stores[0].PickupURL)
	}
	if stores[0].Address != "1 Test St" || stores[0].Lat == 0 {
		t.Errorf("store details not parsed: %+v", stores[0])
	}
}

func TestPickupIgnoresOtherParts(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"body":{"stores":[{
			"storeNumber":"R002","storeName":"Palo Alto",
			"partsAvailability":{"OTHER1LL/A":{"pickupDisplay":"available"}}
		}]}}`)
	}))

	byPart, err := c.Pickup(context.Background(), []string{"TEST1LL/A"}, "94301")
	if err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	stores := byPart["TEST1LL/A"]
	if len(stores) != 0 {
		t.Errorf("got %d stores, want none: the response was about another part", len(stores))
	}
}

// Apple answers for every requested part from the same request, which is what
// makes refreshing every configuration affordable rather than impossible.
func TestScanCoversAWholeBatchInOnePass(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		parts := []string{}
		for i := 0; ; i++ {
			p := r.URL.Query().Get(fmt.Sprintf("parts.%d", i))
			if p == "" {
				break
			}
			parts = append(parts, p)
		}
		avail := make([]string, 0, len(parts))
		for i, p := range parts {
			display := "available"
			if i%2 == 1 {
				display = "ships-to-store"
			}
			avail = append(avail, fmt.Sprintf(`%q:{"pickupDisplay":%q,"pickupSearchQuote":"q"}`, p, display))
		}
		fmt.Fprintf(w, `{"body":{"stores":[{
			"storeNumber":"R002","storeName":"Palo Alto","state":"CA",
			"storelatitude":37.4,"storelongitude":-122.1,
			"address":{"postalCode":"94301"},
			"partsAvailability":{%s}}]}}`, strings.Join(avail, ","))
	}))

	batch := []string{"AAA1LL/A", "BBB1LL/A", "CCC1LL/A"}
	report := c.Scan(context.Background(), batch, []string{"94301", "10001"}, nil)

	// Two anchors, one request each — not one per part per anchor.
	if n := requests.Load(); n != 2 {
		t.Errorf("made %d requests for %d parts across 2 anchors, want 2", n, len(batch))
	}
	for _, part := range batch {
		if len(report.ByPart[part]) != 1 {
			t.Errorf("part %s got %d stores, want 1", part, len(report.ByPart[part]))
		}
	}
	// Statuses must not bleed between parts in the same response.
	if report.ByPart["AAA1LL/A"][0].Status != StatusInStock {
		t.Errorf("AAA status = %q, want in_stock", report.ByPart["AAA1LL/A"][0].Status)
	}
	if report.ByPart["BBB1LL/A"][0].Status == StatusInStock {
		t.Error("BBB should not share AAA's in_stock status")
	}
	// The primary part is still reported plainly for single-part callers.
	if len(report.Stores) != 1 {
		t.Errorf("primary Stores = %d, want 1", len(report.Stores))
	}
}

func TestPickupRejectsOversizedBatches(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"body":{"stores":[]}}`)
	}))
	parts := make([]string, MaxPartsPerRequest+1)
	for i := range parts {
		parts[i] = fmt.Sprintf("P%02dLL/A", i)
	}
	if _, err := c.Pickup(context.Background(), parts, "94301"); err == nil {
		t.Error("expected an error for a batch larger than apple.com accepts")
	}
}
