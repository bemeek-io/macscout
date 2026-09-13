package apple

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Status buckets a store's pickup quote into the three states the map colours by.
type Status string

const (
	StatusInStock     Status = "in_stock"   // sitting on the shelf today
	StatusShortWait   Status = "short_wait" // ships to store within a week
	StatusLongWait    Status = "long_wait"  // ships to store beyond a week
	StatusUnavailable Status = "unavailable"
)

// StoreAvailability is one Apple Store's answer for one part number.
type StoreAvailability struct {
	StoreNumber string  `json:"storeNumber"`
	StoreName   string  `json:"storeName"`
	City        string  `json:"city"`
	State       string  `json:"state"`
	Address     string  `json:"address"`
	PostalCode  string  `json:"postalCode"`
	Phone       string  `json:"phone"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`

	Status    Status `json:"status"`
	Quote     string `json:"quote"`
	PickupURL string `json:"pickupUrl"`
	StoreURL  string `json:"storeUrl"`
	// DaysOut is the wait in days from today, -1 when unavailable or unknown.
	DaysOut int `json:"daysOut"`
}

type pickupResponse struct {
	Body struct {
		ErrorMessage string `json:"errorMessage"`
		Stores       []struct {
			StoreNumber string  `json:"storeNumber"`
			StoreName   string  `json:"storeName"`
			City        string  `json:"city"`
			State       string  `json:"state"`
			PhoneNumber string  `json:"phoneNumber"`
			Latitude    float64 `json:"storelatitude"`
			Longitude   float64 `json:"storelongitude"`
			HoursURL    string  `json:"hoursUrl"`
			Address     struct {
				Address    string `json:"address"`
				Address2   string `json:"address2"`
				PostalCode string `json:"postalCode"`
			} `json:"address"`
			PartsAvailability map[string]struct {
				PickupDisplay     string `json:"pickupDisplay"`
				PickupSearchQuote string `json:"pickupSearchQuote"`
				StorePickEligible bool   `json:"storePickEligible"`
			} `json:"partsAvailability"`
			PickupEncodedUpperDateString string `json:"pickupEncodedUpperDateString"`
		} `json:"stores"`
	} `json:"body"`
}

// MaxPartsPerRequest is how many part numbers to ask about at once.
//
// Apple's pickup endpoint takes parts.0, parts.1, ... and answers for all of
// them across the same ~12 stores, which is the difference between a full
// national refresh of every configuration costing ~9,000 requests and costing
// ~600. Sixteen is measured: it answers reliably, where thirty-two did not.
const MaxPartsPerRequest = 16

// Pickup asks Apple which stores near a postal code can hand over each of the
// given parts, and how soon. Apple answers with the ~12 nearest stores, with
// every requested part reported per store.
func (c *Client) Pickup(ctx context.Context, parts []string, location string) (map[string][]StoreAvailability, error) {
	if len(parts) == 0 {
		return map[string][]StoreAvailability{}, nil
	}
	if len(parts) > MaxPartsPerRequest {
		return nil, fmt.Errorf("apple: %d parts per request exceeds the limit of %d",
			len(parts), MaxPartsPerRequest)
	}

	q := url.Values{}
	for i, part := range parts {
		q.Set(fmt.Sprintf("parts.%d", i), part)
	}
	q.Set("location", location)

	var resp pickupResponse
	if err := c.getJSON(ctx, "/shop/retail/pickup-message", q, &resp); err != nil {
		return nil, err
	}

	today := time.Now()
	out := make(map[string][]StoreAvailability, len(parts))
	for _, s := range resp.Body.Stores {
		addr := s.Address.Address2
		if addr == "" {
			addr = s.Address.Address
		}
		days := daysUntil(s.PickupEncodedUpperDateString, "", today)

		for _, part := range parts {
			pa, ok := s.PartsAvailability[part]
			if !ok {
				continue
			}
			// The encoded date is per store, but the quote is per part, so a
			// part with no date of its own falls back to parsing its quote.
			partDays := days
			if partDays < 0 {
				partDays = daysUntil("", pa.PickupSearchQuote, today)
			}
			out[part] = append(out[part], StoreAvailability{
				StoreNumber: s.StoreNumber,
				StoreName:   s.StoreName,
				City:        s.City,
				State:       s.State,
				Address:     addr,
				PostalCode:  s.Address.PostalCode,
				Phone:       s.PhoneNumber,
				Lat:         s.Latitude,
				Lng:         s.Longitude,
				Status:      classify(pa.PickupDisplay, partDays),
				Quote:       pa.PickupSearchQuote,
				PickupURL:   PickupURL(part, s.StoreNumber),
				StoreURL:    s.HoursURL,
				DaysOut:     partDays,
			})
		}
	}
	return out, nil
}

// classify maps Apple's pickupDisplay plus the quoted date onto a map colour.
// "available" means today; anything ships-to-store is bucketed by how far out
// the promised date lands, with a week as the short/long boundary.
func classify(display string, days int) Status {
	switch display {
	case "available":
		return StatusInStock
	case "ships-to-store":
		if days < 0 {
			return StatusLongWait
		}
		if days <= 1 {
			return StatusInStock
		}
		if days <= 7 {
			return StatusShortWait
		}
		return StatusLongWait
	default:
		return StatusUnavailable
	}
}

var quoteDateRe = regexp.MustCompile(`([A-Z][a-z]{2})\s+(\d{1,2})`)

// daysUntil prefers Apple's encoded YYYYMMDD field and falls back to parsing
// the human quote ("Available Thu, Oct 1"). Returns -1 when neither parses.
func daysUntil(encoded, quote string, now time.Time) int {
	if len(encoded) == 8 {
		if t, err := time.Parse("20060102", encoded); err == nil {
			return diffDays(now, t)
		}
	}
	if strings.Contains(strings.ToLower(quote), "today") {
		return 0
	}
	if m := quoteDateRe.FindStringSubmatch(quote); m != nil {
		day, _ := strconv.Atoi(m[2])
		if t, err := time.Parse("Jan", m[1]); err == nil {
			year := now.Year()
			cand := time.Date(year, t.Month(), day, 0, 0, 0, 0, now.Location())
			if cand.Before(now.AddDate(0, 0, -14)) { // quote rolled into next year
				cand = cand.AddDate(1, 0, 0)
			}
			return diffDays(now, cand)
		}
	}
	return -1
}

func diffDays(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	d := int(b.Sub(a).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// PickupURL deep-links to the part's buy page with the store's pickup overlay
// preselected, which is as close as Apple gets to a per-store product link.
func PickupURL(part, storeNumber string) string {
	return ProductURL(part) + "?fnode=" + url.QueryEscape(storeNumber) +
		"&selectedStore=" + url.QueryEscape(storeNumber)
}

// Delivery reports the ship-to-home window Apple quotes for a postal code.
type Delivery struct {
	Quote    string `json:"quote"`
	Option   string `json:"option"`
	Cost     string `json:"cost"`
	Buyable  bool   `json:"buyable"`
	Location string `json:"location"`
}

func (c *Client) Delivery(ctx context.Context, part, location string) (*Delivery, error) {
	q := url.Values{}
	q.Set("parts.0", part)
	q.Set("location", location)

	var resp struct {
		Body struct {
			Content struct {
				DeliveryMessage map[string]struct {
					Regular struct {
						DeliveryOptions []struct {
							DisplayName  string `json:"displayName"`
							Date         string `json:"date"`
							ShippingCost string `json:"shippingCost"`
						} `json:"deliveryOptions"`
						Buyability struct {
							IsBuyable bool `json:"isBuyable"`
						} `json:"buyability"`
					} `json:"regular"`
				} `json:"deliveryMessage"`
			} `json:"content"`
		} `json:"body"`
	}
	if err := c.getJSON(ctx, "/shop/delivery-message", q, &resp); err != nil {
		return nil, err
	}

	msg, ok := resp.Body.Content.DeliveryMessage[part]
	if !ok {
		return &Delivery{Location: location}, nil
	}
	d := &Delivery{Buyable: msg.Regular.Buyability.IsBuyable, Location: location}
	if len(msg.Regular.DeliveryOptions) > 0 {
		o := msg.Regular.DeliveryOptions[0]
		d.Quote, d.Option, d.Cost = o.Date, o.DisplayName, o.ShippingCost
	}
	return d, nil
}
