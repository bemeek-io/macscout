package apple

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// ScanEvent is one increment of a sweep: the stores a single anchor ZIP
// revealed that no earlier anchor had already reported.
type ScanEvent struct {
	Zip string `json:"zip"`
	// Stores carries the newly seen stores for the primary part, which is what
	// a watching UI is drawing.
	Stores []StoreAvailability `json:"stores"`
	// ByPart carries every part in the batch, for callers persisting results.
	ByPart map[string][]StoreAvailability `json:"-"`
	// Reached lists every store this anchor saw, including ones an earlier
	// anchor already reported. Stores carries only the new ones, so callers
	// tracking coverage need both.
	Reached   []string `json:"-"`
	Done      int      `json:"done"`
	Total     int      `json:"total"`
	Err       string   `json:"error,omitempty"`
	Throttled bool     `json:"throttled,omitempty"`
}

// ScanReport is the outcome of a sweep, including why it stopped short.
type ScanReport struct {
	// Stores is the primary part's result, kept for single-part callers.
	Stores []StoreAvailability
	// ByPart holds every part the sweep asked about.
	ByPart    map[string][]StoreAvailability
	Anchors   int
	Completed int
	Failed    int
	Throttled bool
	// Covers records which stores each anchor reached, which is what lets a
	// later sweep drop the redundant anchors.
	Covers map[string][]string
}

// Complete reports whether every anchor answered.
func (r ScanReport) Complete() bool {
	return !r.Throttled && r.Failed == 0 && r.Completed == r.Anchors
}

// Scan sweeps the given anchor ZIPs for one part number, deduplicating stores
// as they arrive and streaming each batch of newly seen stores to emit.
// emit is called from a single goroutine, so it needs no locking of its own.
//
// If apple.com starts shedding load the sweep stops early and reports what it
// managed to collect, rather than queuing hundreds of doomed retries.
// Scan sweeps anchors for a batch of parts at once. Apple answers for every
// requested part from the same request, so a batch costs no more than a single
// part would — which is what makes refreshing every configuration affordable.
func (c *Client) Scan(ctx context.Context, parts []string, zips []string, emit func(ScanEvent)) ScanReport {
	if len(parts) == 0 {
		return ScanReport{Anchors: len(zips), ByPart: map[string][]StoreAvailability{}}
	}
	primary := parts[0]
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		zip    string
		byPart map[string][]StoreAvailability
		err    error
	}

	// A worker pool rather than one goroutine per ZIP, so cancelling actually
	// prevents the remaining requests from being made.
	pending := make(chan string)
	results := make(chan result)

	workers := cap(c.sem)
	if workers > len(zips) {
		workers = len(zips)
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for zip := range pending {
				byPart, err := c.Pickup(ctx, parts, zip)
				select {
				case results <- result{zip: zip, byPart: byPart, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(pending)
		for _, z := range zips {
			select {
			case pending <- z:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	// Deduplication is per part: the same store appears from many anchors.
	seen := make(map[string]map[string]bool, len(parts))
	byPart := make(map[string][]StoreAvailability, len(parts))
	for _, p := range parts {
		seen[p] = map[string]bool{}
	}
	report := ScanReport{
		Anchors: len(zips),
		Stores:  make([]StoreAvailability, 0, 300),
		ByPart:  byPart,
		Covers:  make(map[string][]string, len(zips)),
	}

	for r := range results {
		report.Completed++
		ev := ScanEvent{Zip: r.zip, Done: report.Completed, Total: len(zips)}

		if r.err != nil {
			report.Failed++
			ev.Err = r.err.Error()
			if errors.Is(r.err, ErrThrottled) {
				report.Throttled = true
				ev.Throttled = true
				if emit != nil {
					emit(ev)
				}
				cancel()
				break
			}
			if emit != nil {
				emit(ev)
			}
			continue
		}

		var reached []string
		fresh := make(map[string][]StoreAvailability, len(r.byPart))
		for part, stores := range r.byPart {
			for _, s := range stores {
				if part == primary {
					reached = append(reached, s.StoreNumber)
				}
				if seen[part][s.StoreNumber] {
					continue
				}
				seen[part][s.StoreNumber] = true
				byPart[part] = append(byPart[part], s)
				fresh[part] = append(fresh[part], s)
				if part == primary {
					report.Stores = append(report.Stores, s)
					ev.Stores = append(ev.Stores, s)
				}
			}
		}
		report.Covers[r.zip] = reached
		ev.Reached = reached
		ev.ByPart = fresh
		if emit != nil {
			emit(ev)
		}
	}

	// Drain so the workers are not left blocked on an unread channel.
	cancel()
	for range results {
	}

	SortStores(report.Stores)
	for _, stores := range byPart {
		SortStores(stores)
	}
	return report
}

// SortStores puts stores in a stable, browsable order: by state, then name.
func SortStores(stores []StoreAvailability) {
	sort.Slice(stores, func(i, j int) bool {
		if stores[i].State != stores[j].State {
			return stores[i].State < stores[j].State
		}
		return stores[i].StoreName < stores[j].StoreName
	})
}

// Summary counts stores per status for the legend and headline stats.
type Summary struct {
	Total       int `json:"total"`
	InStock     int `json:"inStock"`
	ShortWait   int `json:"shortWait"`
	LongWait    int `json:"longWait"`
	Unavailable int `json:"unavailable"`
}

func Summarize(stores []StoreAvailability) Summary {
	var s Summary
	s.Total = len(stores)
	for _, st := range stores {
		switch st.Status {
		case StatusInStock:
			s.InStock++
		case StatusShortWait:
			s.ShortWait++
		case StatusLongWait:
			s.LongWait++
		default:
			s.Unavailable++
		}
	}
	return s
}
