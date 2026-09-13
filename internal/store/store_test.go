package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedTestCatalog registers configurations the way the rotation discovers them.
func seedTestCatalog(t *testing.T, s *Store, family string, parts ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"variants":[`)
	for i, p := range parts {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"part":%q,"name":"Test"}`, p)
	}
	b.WriteString(`]}`)
	if err := s.SaveCatalog(family, []byte(b.String())); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}
}

func sample(number, name, state string, status apple.Status) apple.StoreAvailability {
	return apple.StoreAvailability{
		StoreNumber: number, StoreName: name, State: state, City: "Testville",
		Address: "1 Test St", PostalCode: "94301", Phone: "555-0100",
		Lat: 37.4, Lng: -122.1, Status: status, Quote: "Available Today",
	}
}

func TestUnsyncedPartIsDistinguishableFromEmpty(t *testing.T) {
	s := open(t)

	snap, err := s.Availability("NEVER1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 0 {
		t.Errorf("stores = %d, want none", len(snap.Stores))
	}
	// A never-synced part must not look like a synced part with no stock.
	if !snap.SyncedAt.IsZero() {
		t.Errorf("SyncedAt = %v, want zero for a part never swept", snap.SyncedAt)
	}
	if snap.Fresh(time.Hour) {
		t.Error("a never-synced part must never report as fresh")
	}
}

func TestSaveAndReadRoundTrip(t *testing.T) {
	s := open(t)
	in := []apple.StoreAvailability{
		sample("R002", "Palo Alto", "CA", apple.StatusInStock),
		sample("R075", "Union Square", "CA", apple.StatusLongWait),
	}
	in[1].DaysOut = 21

	if err := s.SaveAvailability("TEST1LL/A", in, 91, 91, true); err != nil {
		t.Fatalf("SaveAvailability: %v", err)
	}

	snap, err := s.Availability("TEST1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 2 {
		t.Fatalf("stores = %d, want 2", len(snap.Stores))
	}
	if !snap.Complete || snap.Anchors != 91 || snap.Covered != 91 {
		t.Errorf("sync state = %+v, want a complete 91/91 sweep", snap)
	}
	if !snap.Fresh(time.Hour) {
		t.Error("a sweep just saved should read as fresh")
	}

	got := snap.Stores[0]
	if got.StoreName != "Palo Alto" || got.Status != apple.StatusInStock || got.Lat == 0 {
		t.Errorf("round-tripped store = %+v", got)
	}
	// The pickup deep link is derived, not stored, so it must be rebuilt.
	if got.PickupURL == "" {
		t.Error("PickupURL should be reconstructed on read")
	}
}

func TestResyncReplacesStatusWithoutDuplicating(t *testing.T) {
	s := open(t)
	first := []apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusLongWait)}
	if err := s.SaveAvailability("TEST1LL/A", first, 91, 91, true); err != nil {
		t.Fatalf("first save: %v", err)
	}

	second := []apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusInStock)}
	if err := s.SaveAvailability("TEST1LL/A", second, 91, 91, true); err != nil {
		t.Fatalf("second save: %v", err)
	}

	snap, err := s.Availability("TEST1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 1 {
		t.Fatalf("stores = %d, want 1 after re-sync", len(snap.Stores))
	}
	if snap.Stores[0].Status != apple.StatusInStock {
		t.Errorf("status = %q, want the newer in_stock", snap.Stores[0].Status)
	}
}

func TestPartsAreIsolatedFromEachOther(t *testing.T) {
	s := open(t)
	if err := s.SaveAvailability("AAA1LL/A",
		[]apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusInStock)}, 1, 1, true); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := s.SaveAvailability("BBB1LL/A",
		[]apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusLongWait)}, 1, 1, true); err != nil {
		t.Fatalf("save B: %v", err)
	}

	a, _ := s.Availability("AAA1LL/A")
	b, _ := s.Availability("BBB1LL/A")
	if a.Stores[0].Status != apple.StatusInStock || b.Stores[0].Status != apple.StatusLongWait {
		t.Errorf("parts bled into each other: A=%q B=%q", a.Stores[0].Status, b.Stores[0].Status)
	}
}

func TestDueForSyncPrefersNeverSynced(t *testing.T) {
	s := open(t)
	for _, part := range []string{"OLD1LL/A", "NEW1LL/A"} {
		if err := s.View(part, "macbook-air", "Test", "$999"); err != nil {
			t.Fatalf("View: %v", err)
		}
	}
	// Only one of them has ever been swept.
	if err := s.SaveAvailability("OLD1LL/A", nil, 91, 91, true); err != nil {
		t.Fatalf("save: %v", err)
	}

	due, err := s.DueForSync(10, time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(due) == 0 {
		t.Fatal("nothing due, want at least the never-synced part")
	}
	if due[0].Part != "NEW1LL/A" {
		t.Errorf("first due = %q, want the never-synced NEW1LL/A", due[0].Part)
	}
}

func TestDueForSyncSkipsFreshParts(t *testing.T) {
	s := open(t)
	if err := s.View("FRESH1LL/A", "macbook-air", "Test", "$999"); err != nil {
		t.Fatalf("View: %v", err)
	}
	if err := s.SaveAvailability("FRESH1LL/A", nil, 91, 91, true); err != nil {
		t.Fatalf("save: %v", err)
	}

	due, err := s.DueForSync(10, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	for _, d := range due {
		if d.Part == "FRESH1LL/A" {
			t.Error("a part synced seconds ago should not be due for another sweep")
		}
	}
}

func TestDueForSyncIgnoresLongUnviewedParts(t *testing.T) {
	s := open(t)
	if err := s.View("STALE1LL/A", "macbook-air", "Test", "$999"); err != nil {
		t.Fatalf("View: %v", err)
	}

	// Nobody has looked at it within the window.
	due, err := s.DueForSync(10, time.Minute, -time.Minute)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("due = %+v, want nothing for a part nobody is watching", due)
	}
}

// Pruning by "last viewed" deleted the sync record of everything the rotation
// had just refreshed, because the rotation covers configurations nobody has
// opened. Discontinuation is the only reason to forget one.
func TestPruneKeepsConfigurationsNobodyHasOpened(t *testing.T) {
	s := open(t)
	seedTestCatalog(t, s, "macbook-air", "LIVE1LL/A")

	if err := s.SaveAvailability("LIVE1LL/A",
		[]apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusInStock)},
		35, 35, true); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	snap, err := s.Availability("LIVE1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 1 || snap.SyncedAt.IsZero() {
		t.Errorf("a configuration Apple still sells was pruned: %+v", snap)
	}
}

func TestPruneDropsDiscontinuedConfigurations(t *testing.T) {
	s := open(t)
	seedTestCatalog(t, s, "macbook-air", "LIVE1LL/A")

	// GONE1LL/A was swept before Apple stopped listing it.
	if err := s.SaveAvailability("GONE1LL/A",
		[]apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusInStock)},
		35, 35, true); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	snap, err := s.Availability("GONE1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 0 || !snap.SyncedAt.IsZero() {
		t.Errorf("a configuration Apple no longer lists survived pruning: %+v", snap)
	}
}

// A cold start has no catalogs yet, and pruning against an empty list would
// delete the whole dataset.
func TestPruneIsANoOpWithoutCatalogs(t *testing.T) {
	s := open(t)
	if err := s.SaveAvailability("A1LL/A",
		[]apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusInStock)},
		35, 35, true); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	snap, err := s.Availability("A1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if len(snap.Stores) != 1 {
		t.Error("pruning with no catalogs stored wiped the dataset")
	}
}

func TestObservationsDoNotOverwriteSyncCoverage(t *testing.T) {
	s := open(t)

	nationwide := []apple.StoreAvailability{
		sample("R002", "Palo Alto", "CA", apple.StatusLongWait),
		sample("R095", "Fifth Avenue", "NY", apple.StatusLongWait),
	}
	if err := s.SaveAvailability("TEST1LL/A", nationwide, 88, 88, true); err != nil {
		t.Fatalf("nationwide save: %v", err)
	}
	before, err := s.Availability("TEST1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}

	// A single-ZIP check that happens to see one of those stores in stock.
	local := []apple.StoreAvailability{sample("R002", "Palo Alto", "CA", apple.StatusInStock)}
	if err := s.SaveObservations("TEST1LL/A", local); err != nil {
		t.Fatalf("SaveObservations: %v", err)
	}

	after, err := s.Availability("TEST1LL/A")
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}

	if after.Anchors != 88 || after.Covered != 88 || !after.Complete {
		t.Errorf("coverage = %d/%d complete=%v, want the nationwide sweep's 88/88 intact",
			after.Covered, after.Anchors, after.Complete)
	}
	if !after.SyncedAt.Equal(before.SyncedAt) {
		t.Errorf("SyncedAt moved from %v to %v; a ZIP check is not a sync",
			before.SyncedAt, after.SyncedAt)
	}
	if len(after.Stores) != 2 {
		t.Fatalf("stores = %d, want both still present", len(after.Stores))
	}
	// The fresher observation should still win for the store it covered.
	for _, st := range after.Stores {
		if st.StoreNumber == "R002" && st.Status != apple.StatusInStock {
			t.Errorf("R002 status = %q, want the newer in_stock reading", st.Status)
		}
	}
}

// A part apple.com will not answer for used to sort first forever, so the
// rotation retried it and nothing else for hours.
func TestFailingPartDoesNotStarveTheRotation(t *testing.T) {
	s := open(t)
	for _, part := range []string{"BROKEN1LL/A", "FINE1LL/A"} {
		if err := s.View(part, "macbook-air", "Test", "$999"); err != nil {
			t.Fatalf("View: %v", err)
		}
	}

	// The broken part is tried and achieves nothing.
	if err := s.MarkAttempt("BROKEN1LL/A", false); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}

	due, err := s.DueForSync(10, time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(due) == 0 {
		t.Fatal("nothing due")
	}
	if due[0].Part != "FINE1LL/A" {
		t.Errorf("next up = %q, want the part that has not been tried yet", due[0].Part)
	}
	// And it is inside its backoff window, so it should not appear at all yet.
	for _, d := range due {
		if d.Part == "BROKEN1LL/A" {
			t.Error("a just-failed part should be held back, not retried immediately")
		}
	}
}

func TestRetryBackoffGrowsWithFailures(t *testing.T) {
	if retryDelay(0) != 0 {
		t.Errorf("retryDelay(0) = %v, want no delay for a healthy part", retryDelay(0))
	}
	if retryDelay(1) >= retryDelay(3) {
		t.Errorf("backoff should grow: 1 failure = %v, 3 failures = %v",
			retryDelay(1), retryDelay(3))
	}
	if retryDelay(50) > 2*time.Hour {
		t.Errorf("retryDelay(50) = %v, want it capped", retryDelay(50))
	}
}

func TestSuccessClearsTheFailStreak(t *testing.T) {
	s := open(t)
	if err := s.View("PART1LL/A", "macbook-air", "Test", "$999"); err != nil {
		t.Fatalf("View: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.MarkAttempt("PART1LL/A", false); err != nil {
			t.Fatalf("MarkAttempt: %v", err)
		}
	}
	if err := s.MarkAttempt("PART1LL/A", true); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}

	due, err := s.DueForSync(10, time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(due) != 1 || due[0].FailStreak != 0 {
		t.Errorf("due = %+v, want the streak cleared so it is retried normally", due)
	}
}

func TestRotationCyclesThroughParts(t *testing.T) {
	s := open(t)
	parts := []string{"A1LL/A", "B1LL/A", "C1LL/A"}
	for _, p := range parts {
		if err := s.View(p, "macbook-air", "Test", "$999"); err != nil {
			t.Fatalf("View: %v", err)
		}
	}

	// Walk the rotation, marking each pick as attempted, and check every part
	// gets a turn rather than the first one repeating.
	seen := map[string]bool{}
	for i := 0; i < len(parts); i++ {
		due, err := s.DueForSync(10, time.Minute, time.Hour)
		if err != nil {
			t.Fatalf("DueForSync: %v", err)
		}
		if len(due) == 0 {
			t.Fatalf("round %d: nothing due", i)
		}
		seen[due[0].Part] = true
		if err := s.MarkAttempt(due[0].Part, true); err != nil {
			t.Fatalf("MarkAttempt: %v", err)
		}
	}
	if len(seen) != len(parts) {
		t.Errorf("visited %d distinct parts in %d rounds, want all %d", len(seen), len(parts), len(parts))
	}
}

// Treating an unfinished sweep as always-due kept every part pinned to the
// front of the queue, so the rotation re-swept the same data forever.
func TestIncompleteSweepIsNotImmediatelyDueAgain(t *testing.T) {
	s := open(t)
	if err := s.View("PART1LL/A", "macbook-air", "Test", "$999"); err != nil {
		t.Fatalf("View: %v", err)
	}
	if err := s.SaveAvailability("PART1LL/A", nil, 35, 31, false); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Just synced, incomplete: it should wait its turn rather than requeue now.
	due, err := s.DueForSync(10, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	for _, d := range due {
		if d.Part == "PART1LL/A" {
			t.Error("an incomplete sweep from seconds ago should not be due again immediately")
		}
	}

	// It should still come back sooner than a complete one would.
	sooner, err := s.DueForSync(10, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(sooner) != 1 {
		t.Errorf("due = %+v, want the incomplete sweep once it has aged", sooner)
	}
}

// A family whose options were never stored cannot be opened at all while
// apple.com is refusing, so those come before merely stale ones.
func TestStaleCatalogsPutsMissingFirst(t *testing.T) {
	s := open(t)
	if err := s.SaveCatalog("macbook-air", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}

	families := []string{"macbook-air", "ipad-pro", "iphone-17-pro"}

	// Everything stored is fresh, so only the never-stored families are due.
	due, err := s.StaleCatalogs(families, time.Hour)
	if err != nil {
		t.Fatalf("StaleCatalogs: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %v, want the two families with no stored options", due)
	}
	for _, d := range due {
		if d == "macbook-air" {
			t.Error("a freshly stored catalog should not be due")
		}
	}

	// Once the stored one ages out it joins the list, behind the missing ones.
	aged, err := s.StaleCatalogs(families, -time.Second)
	if err != nil {
		t.Fatalf("StaleCatalogs: %v", err)
	}
	if len(aged) != 3 || aged[len(aged)-1] != "macbook-air" {
		t.Errorf("due = %v, want the stale stored family last", aged)
	}
}

// Only configurations someone has opened are tracked, so the one just opened
// is the one being waited on and should be swept next.
func TestJustViewedPartIsSweptFirst(t *testing.T) {
	s := open(t)
	for _, part := range []string{"OLD1LL/A", "OLDER1LL/A"} {
		if err := s.View(part, "macbook-air", "Test", "$999"); err != nil {
			t.Fatalf("View: %v", err)
		}
	}
	time.Sleep(1100 * time.Millisecond) // last_viewed_at has second resolution

	if err := s.View("JUST1LL/A", "ipad-pro", "Just opened", "$999"); err != nil {
		t.Fatalf("View: %v", err)
	}

	due, err := s.DueForSync(10, time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	if len(due) == 0 {
		t.Fatal("nothing due")
	}
	if due[0].Part != "JUST1LL/A" {
		t.Errorf("first due = %q, want the configuration just opened", due[0].Part)
	}
}

// A family whose page will not parse has no catalogs row to age, so without a
// separate record of attempts it stays "missing", sorts first forever, and
// starves every other family.
func TestFailingCatalogDoesNotStarveTheOthers(t *testing.T) {
	s := open(t)
	families := []string{"iphone-17-pro", "ipad-pro", "apple-watch"}

	if err := s.MarkCatalogAttempt("iphone-17-pro", false, true); err != nil {
		t.Fatalf("MarkCatalogAttempt: %v", err)
	}

	due, err := s.StaleCatalogs(families, time.Hour)
	if err != nil {
		t.Fatalf("StaleCatalogs: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %v, want the two families not just tried", due)
	}
	for _, d := range due {
		if d == "iphone-17-pro" {
			t.Error("a family that just failed should be held back, not retried immediately")
		}
	}
}

func TestCatalogSuccessClearsItsBackoff(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		if err := s.MarkCatalogAttempt("ipad-pro", false, true); err != nil {
			t.Fatalf("MarkCatalogAttempt: %v", err)
		}
	}
	if err := s.MarkCatalogAttempt("ipad-pro", true, false); err != nil {
		t.Fatalf("MarkCatalogAttempt: %v", err)
	}

	due, err := s.StaleCatalogs([]string{"ipad-pro"}, time.Hour)
	if err != nil {
		t.Fatalf("StaleCatalogs: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("due = %v, want the family retried normally after a success", due)
	}
}
