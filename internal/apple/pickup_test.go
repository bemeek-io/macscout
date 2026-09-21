package apple

import (
	"context"
	"fmt"
	"net/http"
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
		{"quote says tomorrow", "", "Available Tomorrow", 1},
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

// A store's encoded pickup date covers only one of the parts in a multi-part
// request, so each part must be dated from its own quote. Dating them all from
// the store field painted long waits as in stock.
func TestPickupDatesEachPartFromItsOwnQuote(t *testing.T) {
	today := time.Now()
	later := today.AddDate(0, 0, 18)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"body":{"stores":[{
			"storeNumber": "R420", "storeName": "River Park Square", "state": "WA",
			"pickupEncodedUpperDateString": %q,
			"partsAvailability": {
				"SOON1LL/A": {"pickupDisplay": "available", "pickupSearchQuote": "Available Today"},
				"LATE1LL/A": {"pickupDisplay": "ships-to-store", "pickupSearchQuote": %q}
			}
		}]}}`, today.Format("20060102"), "Available "+later.Format("Mon, Jan 2"))
	}))

	got, err := c.Pickup(context.Background(), []string{"SOON1LL/A", "LATE1LL/A"}, "99201")
	if err != nil {
		t.Fatal(err)
	}
	if s := got["SOON1LL/A"][0]; s.Status != StatusInStock {
		t.Errorf("SOON1LL/A = %s (%d days), want in_stock", s.Status, s.DaysOut)
	}
	if s := got["LATE1LL/A"][0]; s.Status != StatusLongWait || s.DaysOut != 18 {
		t.Errorf("LATE1LL/A = %s (%d days), want long_wait at 18 days", s.Status, s.DaysOut)
	}
}

func TestRedate(t *testing.T) {
	observed := time.Date(2026, 9, 20, 15, 0, 0, 0, time.Local)
	cases := []struct {
		status  Status
		quote   string
		want    Status
		days    int
		changed bool
	}{
		{StatusInStock, "Available Today", StatusInStock, 0, false},
		{StatusInStock, "Available Tomorrow", StatusInStock, 1, false},
		{StatusInStock, "Available Wed, Sep 23", StatusShortWait, 3, true},
		{StatusInStock, "Available Thu, Oct 8", StatusLongWait, 18, true},
		{StatusLongWait, "Available Tomorrow", StatusInStock, 1, true},
		{StatusUnavailable, "Available Thu, Oct 8", StatusUnavailable, 0, false},
		{StatusInStock, "Currently unavailable", StatusInStock, -1, false},
	}
	for _, c := range cases {
		got, days, changed := Redate(c.status, c.quote, observed)
		if got != c.want || days != c.days || changed != c.changed {
			t.Errorf("Redate(%s, %q) = %s, %d, %v; want %s, %d, %v",
				c.status, c.quote, got, days, changed, c.want, c.days, c.changed)
		}
	}
}
