package apple

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CoverageIndex remembers which anchor ZIPs actually contributed stores to a
// sweep, so later sweeps can query a much smaller set.
//
// SeedZips is deliberately redundant — anchors overlap so that no store is
// missed — but once a full sweep has run, the overlap is measurable: a greedy
// set cover over the observed per-anchor store lists typically reduces ~90
// anchors to the low twenties, which matters a lot against a host that sheds
// sustained traffic.
type CoverageIndex struct {
	mu         sync.Mutex
	anchors    []string
	storeCount int
	computedAt time.Time
	path       string

	// refreshAfter forces an occasional full sweep so stores that opened since
	// the cover was computed are not invisible forever.
	refreshAfter time.Duration
}

type coverageFile struct {
	Anchors    []string  `json:"anchors"`
	StoreCount int       `json:"storeCount"`
	ComputedAt time.Time `json:"computedAt"`
}

func NewCoverageIndex(path string) *CoverageIndex {
	c := &CoverageIndex{path: path, refreshAfter: 24 * time.Hour}
	c.load()
	return c
}

// Anchors returns the reduced anchor set when one is known and still fresh,
// and the full list otherwise.
func (c *CoverageIndex) Anchors(full []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.anchors) == 0 || time.Since(c.computedAt) > c.refreshAfter {
		return full
	}
	return append([]string(nil), c.anchors...)
}

// ExpectedStores is how many stores the learned anchor set is known to reach,
// or zero before one has been computed. It is what makes "we have everything"
// answerable without insisting every anchor reply.
func (c *CoverageIndex) ExpectedStores() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.anchors) == 0 || time.Since(c.computedAt) > c.refreshAfter {
		return 0
	}
	return c.storeCount
}

// Reduced reports whether the last sweep used a learned anchor set.
func (c *CoverageIndex) Reduced() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.anchors) == 0 || time.Since(c.computedAt) > c.refreshAfter {
		return 0, false
	}
	return len(c.anchors), true
}

// Observe recomputes the cover from a completed sweep. A partial sweep is
// ignored: anchors that never reported would look redundant.
func (c *CoverageIndex) Observe(covers map[string][]string, complete bool) {
	if !complete || len(covers) == 0 {
		return
	}
	anchors, stores := greedyCover(covers)
	if len(anchors) == 0 {
		return
	}

	c.mu.Lock()
	// Only accept a cover that sees at least as many stores as the last one,
	// so a degraded sweep cannot shrink future coverage.
	if stores < c.storeCount {
		c.mu.Unlock()
		return
	}
	c.anchors, c.storeCount, c.computedAt = anchors, stores, time.Now()
	snapshot := coverageFile{Anchors: anchors, StoreCount: stores, ComputedAt: c.computedAt}
	path := c.path
	c.mu.Unlock()

	if path != "" {
		save(path, snapshot)
	}
}

// greedyCover picks the fewest anchors that still reach every store seen,
// repeatedly taking whichever anchor adds the most not-yet-covered stores.
func greedyCover(covers map[string][]string) (anchors []string, stores int) {
	uncovered := make(map[string]bool)
	for _, list := range covers {
		for _, store := range list {
			uncovered[store] = true
		}
	}
	stores = len(uncovered)

	zips := make([]string, 0, len(covers))
	for z := range covers {
		zips = append(zips, z)
	}
	sort.Strings(zips) // deterministic tie-breaking

	for len(uncovered) > 0 {
		bestZip, bestGain := "", 0
		for _, z := range zips {
			gain := 0
			for _, store := range covers[z] {
				if uncovered[store] {
					gain++
				}
			}
			if gain > bestGain {
				bestZip, bestGain = z, gain
			}
		}
		if bestZip == "" {
			break
		}
		anchors = append(anchors, bestZip)
		for _, store := range covers[bestZip] {
			delete(uncovered, store)
		}
	}
	sort.Strings(anchors)
	return anchors, stores
}

func (c *CoverageIndex) load() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var f coverageFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	c.anchors, c.storeCount, c.computedAt = f.Anchors, f.StoreCount, f.ComputedAt
}

func save(path string, f coverageFile) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
