// Package store persists what has been learned from apple.com so the UI can
// read it instantly, instead of waiting on a rate-limited upstream.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benmeeker/macscout/internal/apple"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed record of store locations, per-part availability,
// and when each part was last swept.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS stores (
  store_number TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  city         TEXT NOT NULL,
  state        TEXT NOT NULL,
  address      TEXT NOT NULL,
  postal_code  TEXT NOT NULL,
  phone        TEXT NOT NULL,
  store_url    TEXT NOT NULL,
  lat          REAL NOT NULL,
  lng          REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS availability (
  part         TEXT NOT NULL,
  store_number TEXT NOT NULL,
  status       TEXT NOT NULL,
  quote        TEXT NOT NULL,
  days_out     INTEGER NOT NULL,
  observed_at  INTEGER NOT NULL,
  PRIMARY KEY (part, store_number)
);

CREATE INDEX IF NOT EXISTS availability_part ON availability(part);

-- The product options are scraped from Apple's buy pages, so they are the one
-- read path that could still fail when apple.com is unavailable. Keeping the
-- last good copy means a throttled lookup costs freshness, not the whole app:
-- without it, no options means no part number, which means the stored
-- availability below is unreachable.
CREATE TABLE IF NOT EXISTS catalogs (
  family   TEXT PRIMARY KEY,
  payload  TEXT NOT NULL,
  saved_at INTEGER NOT NULL
);

-- Tried, not necessarily succeeded. A family whose page will not parse has no
-- catalogs row to timestamp, so without this it stays "missing", sorts first
-- forever, and starves every other family — the same trap the availability
-- rotation fell into.
CREATE TABLE IF NOT EXISTS catalog_attempts (
  family       TEXT PRIMARY KEY,
  attempted_at INTEGER NOT NULL,
  fail_streak  INTEGER NOT NULL DEFAULT 0
);

-- One row per part the syncer keeps fresh. last_viewed_at is what makes the
-- rotation follow what people actually look at.
CREATE TABLE IF NOT EXISTS parts (
  part            TEXT PRIMARY KEY,
  family          TEXT NOT NULL DEFAULT '',
  name            TEXT NOT NULL DEFAULT '',
  price           TEXT NOT NULL DEFAULT '',
  last_viewed_at  INTEGER NOT NULL DEFAULT 0,
  synced_at       INTEGER NOT NULL DEFAULT 0,
  sync_complete   INTEGER NOT NULL DEFAULT 0,
  anchors         INTEGER NOT NULL DEFAULT 0,
  covered         INTEGER NOT NULL DEFAULT 0,
  -- attempted_at is what makes the rotation fair. Ordering by synced_at alone
  -- let a part that never succeeds sort first forever and starve every other
  -- part, so the queue turns on when each part was last *tried*.
  attempted_at    INTEGER NOT NULL DEFAULT 0,
  fail_streak     INTEGER NOT NULL DEFAULT 0
);
`

// Added after the first release; ignored when the column already exists.
var migrations = []string{
	`ALTER TABLE parts ADD COLUMN attempted_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE parts ADD COLUMN fail_streak INTEGER NOT NULL DEFAULT 0`,
}

// Open prepares the database at path, creating it if needed.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}

	// WAL keeps the background syncer's writes from blocking UI reads.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range migrations {
		// A duplicate-column error just means this database already has it.
		_, _ = db.Exec(m)
	}
	st := &Store{db: db}
	if err := st.redateOnce(); err != nil {
		db.Close()
		return nil, fmt.Errorf("repair availability: %w", err)
	}
	return st, nil
}

// redateVersion marks databases whose availability rows have been re-dated
// from their own quotes (see apple.Redate).
const redateVersion = 1

// redateOnce repairs rows saved before each part was dated from its own
// quote. It runs once per database and needs nothing from apple.com.
func (s *Store) redateOnce() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= redateVersion {
		return nil
	}

	type fix struct {
		part, store string
		status      apple.Status
		days        int
	}
	rows, err := s.db.Query(`SELECT part, store_number, status, quote, observed_at FROM availability`)
	if err != nil {
		return err
	}
	var fixes []fix
	for rows.Next() {
		var (
			f        fix
			status   string
			quote    string
			observed int64
		)
		if err := rows.Scan(&f.part, &f.store, &status, &quote, &observed); err != nil {
			rows.Close()
			return err
		}
		next, days, changed := apple.Redate(apple.Status(status), quote, time.Unix(observed, 0))
		if changed {
			f.status, f.days = next, days
			fixes = append(fixes, f)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range fixes {
		if _, err := tx.Exec(`UPDATE availability SET status = ?, days_out = ? WHERE part = ? AND store_number = ?`,
			string(f.status), f.days, f.part, f.store); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, redateVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

// Snapshot is everything known about one part right now.
type Snapshot struct {
	Part     string                    `json:"part"`
	Stores   []apple.StoreAvailability `json:"stores"`
	SyncedAt time.Time                 `json:"syncedAt"`
	Complete bool                      `json:"complete"`
	Anchors  int                       `json:"anchors"`
	Covered  int                       `json:"covered"`
}

// Fresh reports whether the snapshot is recent enough to show without caveat.
func (s Snapshot) Fresh(within time.Duration) bool {
	return !s.SyncedAt.IsZero() && time.Since(s.SyncedAt) < within
}

// Availability reads a part's last known availability. A part that has never
// been swept comes back with no stores and a zero SyncedAt, which callers must
// present as "not synced yet" rather than "not available anywhere".
func (s *Store) Availability(part string) (Snapshot, error) {
	snap := Snapshot{Part: part, Stores: []apple.StoreAvailability{}}

	var syncedAt, complete int64
	err := s.db.QueryRow(
		`SELECT synced_at, sync_complete, anchors, covered FROM parts WHERE part = ?`, part,
	).Scan(&syncedAt, &complete, &snap.Anchors, &snap.Covered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return snap, fmt.Errorf("read sync state: %w", err)
	}
	if syncedAt > 0 {
		snap.SyncedAt = time.Unix(syncedAt, 0)
	}
	snap.Complete = complete == 1

	rows, err := s.db.Query(`
		SELECT a.store_number, a.status, a.quote, a.days_out,
		       s.name, s.city, s.state, s.address, s.postal_code, s.phone, s.store_url, s.lat, s.lng
		FROM availability a
		JOIN stores s ON s.store_number = a.store_number
		WHERE a.part = ?
		ORDER BY s.state, s.name`, part)
	if err != nil {
		return snap, fmt.Errorf("read availability: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a apple.StoreAvailability
		if err := rows.Scan(
			&a.StoreNumber, &a.Status, &a.Quote, &a.DaysOut,
			&a.StoreName, &a.City, &a.State, &a.Address, &a.PostalCode, &a.Phone, &a.StoreURL,
			&a.Lat, &a.Lng,
		); err != nil {
			return snap, fmt.Errorf("scan availability: %w", err)
		}
		a.PickupURL = apple.PickupURL(part, a.StoreNumber)
		snap.Stores = append(snap.Stores, a)
	}
	return snap, rows.Err()
}

// SaveObservations records what a lookup saw, without claiming anything about
// how much of the country it covered.
//
// A ZIP check is real, current data for the dozen stores it touched — worth
// keeping — but it is not a nationwide sync, and letting it write the sync
// record would have "synced just now" imply national coverage from a single
// request.
func (s *Store) SaveObservations(part string, stores []apple.StoreAvailability) error {
	return s.save(part, stores, nil)
}

// SaveAvailability records a sweep's results together with its coverage. Store
// locations are upserted separately from availability because they change on a
// completely different timescale — a store moves once a decade, its stock
// changes hourly.
func (s *Store) SaveAvailability(part string, stores []apple.StoreAvailability, anchors, covered int, complete bool) error {
	return s.save(part, stores, &syncRecord{anchors: anchors, covered: covered, complete: complete})
}

type syncRecord struct {
	anchors, covered int
	complete         bool
}

func (s *Store) save(part string, stores []apple.StoreAvailability, sync *syncRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	upsertStore, err := tx.Prepare(`
		INSERT INTO stores (store_number, name, city, state, address, postal_code, phone, store_url, lat, lng)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(store_number) DO UPDATE SET
		  name=excluded.name, city=excluded.city, state=excluded.state,
		  address=excluded.address, postal_code=excluded.postal_code,
		  phone=excluded.phone, store_url=excluded.store_url,
		  lat=excluded.lat, lng=excluded.lng`)
	if err != nil {
		return fmt.Errorf("prepare store upsert: %w", err)
	}
	defer upsertStore.Close()

	upsertAvail, err := tx.Prepare(`
		INSERT INTO availability (part, store_number, status, quote, days_out, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(part, store_number) DO UPDATE SET
		  status=excluded.status, quote=excluded.quote,
		  days_out=excluded.days_out, observed_at=excluded.observed_at`)
	if err != nil {
		return fmt.Errorf("prepare availability upsert: %w", err)
	}
	defer upsertAvail.Close()

	now := time.Now().Unix()
	for _, a := range stores {
		if _, err := upsertStore.Exec(a.StoreNumber, a.StoreName, a.City, a.State,
			a.Address, a.PostalCode, a.Phone, a.StoreURL, a.Lat, a.Lng); err != nil {
			return fmt.Errorf("upsert store %s: %w", a.StoreNumber, err)
		}
		if _, err := upsertAvail.Exec(part, a.StoreNumber, string(a.Status), a.Quote, a.DaysOut, now); err != nil {
			return fmt.Errorf("upsert availability %s: %w", a.StoreNumber, err)
		}
	}

	if sync != nil {
		completeInt := 0
		if sync.complete {
			completeInt = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO parts (part, synced_at, sync_complete, anchors, covered)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(part) DO UPDATE SET
			  synced_at=excluded.synced_at, sync_complete=excluded.sync_complete,
			  anchors=excluded.anchors, covered=excluded.covered`,
			part, now, completeInt, sync.anchors, sync.covered); err != nil {
			return fmt.Errorf("record sync: %w", err)
		}
	}

	return tx.Commit()
}

// SaveCatalog stores a family's configurator data.
func (s *Store) SaveCatalog(family string, payload []byte) error {
	_, err := s.db.Exec(`
		INSERT INTO catalogs (family, payload, saved_at)
		VALUES (?, ?, ?)
		ON CONFLICT(family) DO UPDATE SET
		  payload=excluded.payload, saved_at=excluded.saved_at`,
		family, string(payload), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save catalog: %w", err)
	}
	return nil
}

// Catalog returns the last stored copy of a family's configurator data, and
// when it was stored. A missing family returns ErrNoCatalog.
func (s *Store) Catalog(family string) ([]byte, time.Time, error) {
	var payload string
	var savedAt int64
	err := s.db.QueryRow(
		`SELECT payload, saved_at FROM catalogs WHERE family = ?`, family,
	).Scan(&payload, &savedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNoCatalog
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read catalog: %w", err)
	}
	return []byte(payload), time.Unix(savedAt, 0), nil
}

// KnownParts returns every configuration in the stored catalogs, paired with
// its family, so the rotation can refresh all of them rather than only the
// ones somebody happened to open.
func (s *Store) KnownParts() ([]Tracked, error) {
	rows, err := s.db.Query(`SELECT family, payload FROM catalogs ORDER BY family`)
	if err != nil {
		return nil, fmt.Errorf("read catalogs: %w", err)
	}
	defer rows.Close()

	var out []Tracked
	seen := map[string]bool{}
	for rows.Next() {
		var family, payload string
		if err := rows.Scan(&family, &payload); err != nil {
			return nil, fmt.Errorf("scan catalog: %w", err)
		}
		if family == familiesKey {
			continue
		}
		var cat struct {
			Variants []struct {
				Part string `json:"part"`
				Name string `json:"name"`
			} `json:"variants"`
		}
		if err := json.Unmarshal([]byte(payload), &cat); err != nil {
			continue
		}
		for _, v := range cat.Variants {
			if v.Part == "" || seen[v.Part] {
				continue
			}
			seen[v.Part] = true
			out = append(out, Tracked{Part: v.Part, Family: family, Name: v.Name})
		}
	}
	return out, rows.Err()
}

// Progress counts how much of the lineup has been swept, for reporting.
type Progress struct {
	Configurations int `json:"configurations"`
	Synced         int `json:"synced"`
	Complete       int `json:"complete"`
}

// Coverage reports how far the rotation has got through the lineup.
func (s *Store) Coverage() (Progress, error) {
	known, err := s.KnownParts()
	if err != nil {
		return Progress{}, err
	}
	p := Progress{Configurations: len(known)}
	if len(known) == 0 {
		return p, nil
	}

	live := map[string]bool{}
	for _, t := range known {
		live[t.Part] = true
	}
	rows, err := s.db.Query(`SELECT part, sync_complete FROM parts WHERE synced_at > 0`)
	if err != nil {
		return p, fmt.Errorf("read coverage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var part string
		var complete int
		if err := rows.Scan(&part, &complete); err != nil {
			return p, fmt.Errorf("scan coverage: %w", err)
		}
		if !live[part] {
			continue
		}
		p.Synced++
		if complete == 1 {
			p.Complete++
		}
	}
	return p, rows.Err()
}

// SyncAges returns when each part was last synced, for parts that have been.
func (s *Store) SyncAges() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT part, synced_at FROM parts WHERE synced_at > 0`)
	if err != nil {
		return nil, fmt.Errorf("read sync ages: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var part string
		var at int64
		if err := rows.Scan(&part, &at); err != nil {
			return nil, fmt.Errorf("scan sync age: %w", err)
		}
		out[part] = time.Unix(at, 0)
	}
	return out, rows.Err()
}

// StaleCatalogs returns the families with no stored options, or options older
// than maxAge, missing ones first. A family whose options are missing cannot be
// browsed at all when apple.com is refusing, so it is the more urgent case.
// SaveFamilies stores the discovered product lineup.
func (s *Store) SaveFamilies(payload []byte) error {
	return s.SaveCatalog(familiesKey, payload)
}

// Families returns the last discovered lineup, or ErrNoCatalog if there is
// none yet.
func (s *Store) Families() ([]byte, time.Time, error) {
	return s.Catalog(familiesKey)
}

// familiesKey stores the lineup in the catalogs table. It is prefixed so it can
// never collide with a real family slug, and skipped when listing catalogs.
const familiesKey = "__families__"

// MarkCatalogAttempt records that a family's options were fetched, so the queue
// moves on either way.
//
// permanent separates "this page will not parse" from "apple.com would not
// answer". Only the former counts toward the failure streak: a family should
// not be judged unusable because the network was busy.
func (s *Store) MarkCatalogAttempt(family string, ok, permanent bool) error {
	streak := "catalog_attempts.fail_streak"
	switch {
	case ok:
		streak = "0"
	case permanent:
		streak = "catalog_attempts.fail_streak + 1"
	}
	_, err := s.db.Exec(`
		INSERT INTO catalog_attempts (family, attempted_at, fail_streak)
		VALUES (?, ?, ?)
		ON CONFLICT(family) DO UPDATE SET
		  attempted_at = excluded.attempted_at,
		  fail_streak = `+streak,
		family, time.Now().Unix(), boolToInt(!ok && permanent))
	if err != nil {
		return fmt.Errorf("mark catalog attempt: %w", err)
	}
	return nil
}

// UnparseableFamilies lists product lines whose page has failed to parse
// repeatedly — discontinued models Apple still links, or pages that are not
// really a configurable product. Offering them would only dead-end a reader.
func (s *Store) UnparseableFamilies(minStreak int) (map[string]bool, error) {
	rows, err := s.db.Query(
		`SELECT family FROM catalog_attempts WHERE fail_streak >= ?`, minStreak)
	if err != nil {
		return nil, fmt.Errorf("read failing families: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var family string
		if err := rows.Scan(&family); err != nil {
			return nil, fmt.Errorf("scan failing family: %w", err)
		}
		out[family] = true
	}
	return out, rows.Err()
}

func (s *Store) StaleCatalogs(families []string, maxAge time.Duration) ([]string, error) {
	attempts := map[string]struct {
		at     time.Time
		streak int
	}{}
	aRows, err := s.db.Query(`SELECT family, attempted_at, fail_streak FROM catalog_attempts`)
	if err != nil {
		return nil, fmt.Errorf("read catalog attempts: %w", err)
	}
	for aRows.Next() {
		var family string
		var at int64
		var streak int
		if err := aRows.Scan(&family, &at, &streak); err != nil {
			aRows.Close()
			return nil, fmt.Errorf("scan catalog attempt: %w", err)
		}
		attempts[family] = struct {
			at     time.Time
			streak int
		}{time.Unix(at, 0), streak}
	}
	aRows.Close()
	if err := aRows.Err(); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT family, saved_at FROM catalogs`)
	if err != nil {
		return nil, fmt.Errorf("read catalog ages: %w", err)
	}
	defer rows.Close()

	saved := map[string]time.Time{}
	for rows.Next() {
		var family string
		var at int64
		if err := rows.Scan(&family, &at); err != nil {
			return nil, fmt.Errorf("scan catalog age: %w", err)
		}
		saved[family] = time.Unix(at, 0)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing, stale []string
	for _, family := range families {
		// Hold back families that have just been tried and failed.
		if a, tried := attempts[family]; tried && a.streak > 0 {
			if time.Now().Before(a.at.Add(retryDelay(a.streak))) {
				continue
			}
		}
		at, ok := saved[family]
		switch {
		case !ok:
			missing = append(missing, family)
		case time.Since(at) > maxAge:
			stale = append(stale, family)
		}
	}
	return append(missing, stale...), nil
}

// ErrNoCatalog reports that a family has never been fetched successfully.
var ErrNoCatalog = errors.New("no stored catalog for this family")

// View registers a part as worth keeping fresh, and marks it as just looked at
// so the syncer prioritises it.
func (s *Store) View(part, family, name, price string) error {
	_, err := s.db.Exec(`
		INSERT INTO parts (part, family, name, price, last_viewed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(part) DO UPDATE SET
		  family=excluded.family, name=excluded.name, price=excluded.price,
		  last_viewed_at=excluded.last_viewed_at`,
		part, family, name, price, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("record view: %w", err)
	}
	return err
}

// Tracked is one part in the sync rotation.
type Tracked struct {
	Part     string
	Family   string
	Name     string
	SyncedAt time.Time
	Complete bool
	// FailStreak counts consecutive attempts that achieved nothing.
	FailStreak int
}

// retryDelay is how long to leave a part alone after consecutive failures.
// A part Apple will not answer for — discontinued, not stocked, or simply
// refused — must not be retried every cycle: doing so spends the request
// budget on the one part guaranteed not to use it.
func retryDelay(failStreak int) time.Duration {
	if failStreak <= 0 {
		return 0
	}
	delay := 5 * time.Minute
	for i := 1; i < failStreak && delay < 2*time.Hour; i++ {
		delay *= 2
	}
	if delay > 2*time.Hour {
		delay = 2 * time.Hour
	}
	return delay
}

// DueForSync returns the parts most worth refreshing, oldest attempt first so
// every tracked part gets a turn, skipping parts still inside their retry
// backoff.
func (s *Store) DueForSync(limit int, keepFresh time.Duration, viewedSince time.Duration) ([]Tracked, error) {
	cutoff := time.Now().Add(-viewedSince).Unix()
	stale := time.Now().Add(-keepFresh).Unix()
	now := time.Now()

	// An unfinished sweep is worth revisiting sooner than a finished one, but
	// not immediately: treating "incomplete" as always-due put every part
	// permanently at the front of the queue and had the rotation re-sweeping
	// the same data continuously.
	partialStale := time.Now().Add(-keepFresh / 3).Unix()

	rows, err := s.db.Query(`
		SELECT part, family, name, synced_at, sync_complete, attempted_at, fail_streak
		FROM parts
		WHERE last_viewed_at >= ?
		  AND (synced_at = 0
		       OR (sync_complete = 1 AND synced_at <= ?)
		       OR (sync_complete = 0 AND synced_at <= ?))
		-- Oldest attempt first keeps the rotation fair; among parts never
		-- tried, the one most recently looked at goes first, because that is
		-- the one somebody is waiting on.
		ORDER BY attempted_at ASC, last_viewed_at DESC
		LIMIT ?`, cutoff, stale, partialStale, limit)
	if err != nil {
		return nil, fmt.Errorf("query due parts: %w", err)
	}
	defer rows.Close()

	var out []Tracked
	for rows.Next() {
		var t Tracked
		var syncedAt, attemptedAt int64
		var complete, failStreak int
		if err := rows.Scan(&t.Part, &t.Family, &t.Name, &syncedAt, &complete,
			&attemptedAt, &failStreak); err != nil {
			return nil, fmt.Errorf("scan due part: %w", err)
		}
		if attemptedAt > 0 {
			if until := time.Unix(attemptedAt, 0).Add(retryDelay(failStreak)); now.Before(until) {
				continue
			}
		}
		if syncedAt > 0 {
			t.SyncedAt = time.Unix(syncedAt, 0)
		}
		t.Complete = complete == 1
		t.FailStreak = failStreak
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkAttempt records that a sweep was tried, whether or not it achieved
// anything. This is what moves a part to the back of the queue.
func (s *Store) MarkAttempt(part string, progressed bool) error {
	streak := "fail_streak + 1"
	if progressed {
		streak = "0"
	}
	_, err := s.db.Exec(`
		INSERT INTO parts (part, attempted_at, fail_streak)
		VALUES (?, ?, ?)
		ON CONFLICT(part) DO UPDATE SET
		  attempted_at = excluded.attempted_at,
		  fail_streak = `+streak,
		part, time.Now().Unix(), boolToInt(!progressed))
	if err != nil {
		return fmt.Errorf("mark attempt: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Prune drops configurations that are no longer sold.
//
// It deliberately does not prune by "last viewed": the rotation refreshes every
// configuration Apple lists, not only the ones somebody opened, so viewing was
// never the right signal — using it deleted the sync record of everything the
// rotation had just refreshed, making it all look never-synced and putting the
// rotation straight back to work on data it already had.
func (s *Store) Prune() error {
	known, err := s.KnownParts()
	if err != nil {
		return err
	}
	// No catalogs stored yet means nothing to compare against; pruning here
	// would delete the entire dataset on a cold start.
	if len(known) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin prune: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS live_parts (part TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("prune scratch table: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM live_parts`); err != nil {
		return fmt.Errorf("clear prune scratch: %w", err)
	}
	insert, err := tx.Prepare(`INSERT OR IGNORE INTO live_parts (part) VALUES (?)`)
	if err != nil {
		return fmt.Errorf("prepare prune insert: %w", err)
	}
	defer insert.Close()
	for _, t := range known {
		if _, err := insert.Exec(t.Part); err != nil {
			return fmt.Errorf("record live part: %w", err)
		}
	}

	for _, stmt := range []string{
		`DELETE FROM availability WHERE part NOT IN (SELECT part FROM live_parts)`,
		`DELETE FROM parts WHERE part NOT IN (SELECT part FROM live_parts)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	return tx.Commit()
}
