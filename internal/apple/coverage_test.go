package apple

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGreedyCoverDropsRedundantAnchors(t *testing.T) {
	// "b" is fully contained in "a" plus "c", so it should be dropped.
	covers := map[string][]string{
		"a": {"R1", "R2", "R3"},
		"b": {"R2", "R3"},
		"c": {"R3", "R4", "R5"},
	}
	anchors, stores := greedyCover(covers)

	if stores != 5 {
		t.Errorf("stores = %d, want 5", stores)
	}
	if len(anchors) != 2 {
		t.Fatalf("anchors = %v, want 2", anchors)
	}
	got := map[string]bool{anchors[0]: true, anchors[1]: true}
	if !got["a"] || !got["c"] {
		t.Errorf("anchors = %v, want a and c", anchors)
	}
}

func TestGreedyCoverIsDeterministic(t *testing.T) {
	covers := map[string][]string{
		"x": {"R1", "R2"},
		"y": {"R1", "R2"},
		"z": {"R3"},
	}
	first, _ := greedyCover(covers)
	for i := 0; i < 20; i++ {
		again, _ := greedyCover(covers)
		if len(again) != len(first) || again[0] != first[0] {
			t.Fatalf("run %d gave %v, first gave %v", i, again, first)
		}
	}
}

func TestIndexFallsBackToFullListUntilObserved(t *testing.T) {
	idx := NewCoverageIndex("")
	full := []string{"1", "2", "3"}

	if got := idx.Anchors(full); len(got) != 3 {
		t.Errorf("Anchors() = %v, want the full list before any sweep", got)
	}
	if _, ok := idx.Reduced(); ok {
		t.Error("Reduced() should be false before any sweep")
	}
}

func TestIndexIgnoresPartialSweeps(t *testing.T) {
	idx := NewCoverageIndex("")
	full := []string{"1", "2", "3"}

	// A truncated sweep makes unreported anchors look redundant.
	idx.Observe(map[string][]string{"1": {"R1"}}, false)
	if got := idx.Anchors(full); len(got) != 3 {
		t.Errorf("Anchors() = %v, want the full list after a partial sweep", got)
	}
}

func TestIndexReducesAfterFullSweep(t *testing.T) {
	idx := NewCoverageIndex("")
	full := []string{"1", "2", "3"}

	idx.Observe(map[string][]string{
		"1": {"R1", "R2"},
		"2": {"R1", "R2"},
		"3": {"R3"},
	}, true)

	got := idx.Anchors(full)
	if len(got) != 2 {
		t.Errorf("Anchors() = %v, want 2 after learning the cover", got)
	}
	if n, ok := idx.Reduced(); !ok || n != 2 {
		t.Errorf("Reduced() = %d, %v; want 2, true", n, ok)
	}
}

func TestIndexKeepsBestCover(t *testing.T) {
	idx := NewCoverageIndex("")
	idx.Observe(map[string][]string{"1": {"R1", "R2", "R3"}}, true)

	// A later sweep that saw fewer stores must not shrink coverage.
	idx.Observe(map[string][]string{"9": {"R9"}}, true)

	got := idx.Anchors([]string{"1", "9"})
	if len(got) != 1 || got[0] != "1" {
		t.Errorf("Anchors() = %v, want the richer earlier cover", got)
	}
}

func TestIndexPersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchors.json")

	first := NewCoverageIndex(path)
	first.Observe(map[string][]string{
		"1": {"R1", "R2"},
		"2": {"R1"},
		"3": {"R3"},
	}, true)

	reopened := NewCoverageIndex(path)
	got := reopened.Anchors([]string{"1", "2", "3"})
	if len(got) != 2 {
		t.Errorf("Anchors() = %v after reopening, want the persisted cover", got)
	}
}

func TestIndexRefreshesAfterTTL(t *testing.T) {
	idx := NewCoverageIndex("")
	idx.refreshAfter = time.Millisecond
	idx.Observe(map[string][]string{"1": {"R1"}}, true)

	time.Sleep(5 * time.Millisecond)

	full := []string{"1", "2", "3"}
	if got := idx.Anchors(full); len(got) != 3 {
		t.Errorf("Anchors() = %v, want a full sweep once the cover goes stale", got)
	}
}
