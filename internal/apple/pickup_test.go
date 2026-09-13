package apple

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		display string
		days    int
		want    Status
	}{
		{"on the shelf", "available", 0, StatusInStock},
		{"shelf stock with a stale date", "available", 9, StatusInStock},
		{"arrives tomorrow", "ships-to-store", 1, StatusInStock},
		{"arrives this week", "ships-to-store", 5, StatusShortWait},
		{"boundary of a week", "ships-to-store", 7, StatusShortWait},
		{"just past a week", "ships-to-store", 8, StatusLongWait},
		{"three weeks out", "ships-to-store", 22, StatusLongWait},
		{"undated ship-to-store", "ships-to-store", -1, StatusLongWait},
		{"not offered", "unavailable", -1, StatusUnavailable},
		{"unknown display", "something-new", 3, StatusUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.display, tt.days); got != tt.want {
				t.Errorf("classify(%q, %d) = %q, want %q", tt.display, tt.days, got, tt.want)
			}
		})
	}
}

func TestDaysUntil(t *testing.T) {
	now := time.Date(2026, time.September, 8, 14, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		encoded string
		quote   string
		want    int
	}{
		{"encoded date wins", "20260930", "Available Wed, Sep 30", 22},
		{"encoded same day", "20260908", "Available Today", 0},
		{"quote says today", "", "Available Today", 0},
		{"quote only", "", "Available Thu, Oct 1", 23},
		{"quote rolls into next year", "", "Available Fri, Jan 2", 116},
		{"unparseable", "", "Unavailable for Pickup", -1},
		{"past dates clamp to zero", "20260901", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := daysUntil(tt.encoded, tt.quote, now); got != tt.want {
				t.Errorf("daysUntil(%q, %q) = %d, want %d", tt.encoded, tt.quote, got, tt.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	got := Summarize([]StoreAvailability{
		{Status: StatusInStock}, {Status: StatusInStock},
		{Status: StatusShortWait},
		{Status: StatusLongWait}, {Status: StatusLongWait}, {Status: StatusLongWait},
		{Status: StatusUnavailable},
	})
	want := Summary{Total: 7, InStock: 2, ShortWait: 1, LongWait: 3, Unavailable: 1}
	if got != want {
		t.Errorf("Summarize() = %+v, want %+v", got, want)
	}
}
