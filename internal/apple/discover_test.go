package apple

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFamilyName(t *testing.T) {
	tests := map[string]string{
		"macbook-air":        "MacBook Air",
		"mac-mini":           "Mac mini",
		"imac":               "iMac",
		"iphone-18-pro":      "iPhone 18 Pro",
		"iphone-17e":         "iPhone 17e",
		"ipad-mini":          "iPad mini",
		"apple-watch-ultra":  "Apple Watch Ultra",
		"apple-watch-se":     "Apple Watch SE",
		"apple-watch-hermes": "Apple Watch Hermès",
		"studio-display-xdr": "Studio Display XDR",
		"macbook-neo":        "MacBook Neo",
	}
	for slug, want := range tests {
		if got := familyName(slug); got != want {
			t.Errorf("familyName(%q) = %q, want %q", slug, got, want)
		}
	}
}

// A hard-coded list silently goes stale: Apple discontinues a model and ships
// its replacement, and the app keeps asking for the dead one.
func TestDiscoverFamiliesReadsApplesCurrentLineup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/shop/buy-mac":
			fmt.Fprint(w, `<a href="/shop/buy-mac/macbook-air">x</a><a href="/shop/buy-mac/macbook-neo">y</a>`)
		case "/shop/buy-iphone":
			fmt.Fprint(w, `<a href="/shop/buy-iphone/iphone-18-pro">x</a>
			               <a href="/shop/buy-iphone/carrier-offers">skip me</a>`)
		default:
			fmt.Fprint(w, `<html></html>`)
		}
	}))
	defer srv.Close()

	c := NewClient(1, WithBaseURL(srv.URL), WithRateLimit(0))
	families, err := c.DiscoverFamilies(context.Background())
	if err != nil {
		t.Fatalf("DiscoverFamilies: %v", err)
	}

	byID := map[string]Family{}
	for _, f := range families {
		byID[f.ID] = f
	}
	if _, ok := byID["carrier-offers"]; ok {
		t.Error("carrier-offers is not a product line")
	}
	for _, want := range []string{"macbook-air", "macbook-neo", "iphone-18-pro"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("did not discover %q; found %v", want, families)
		}
	}
	if got := byID["iphone-18-pro"]; got.Group != "iPhone" || got.Path != "/shop/buy-iphone/iphone-18-pro" {
		t.Errorf("iphone-18-pro = %+v", got)
	}
	if got := byID["macbook-neo"].Name; got != "MacBook Neo" {
		t.Errorf("name = %q, want MacBook Neo", got)
	}
}

func TestDiscoverFamiliesErrorsWhenNothingFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html>no products here</html>`)
	}))
	defer srv.Close()

	c := NewClient(1, WithBaseURL(srv.URL), WithRateLimit(0))
	if _, err := c.DiscoverFamilies(context.Background()); err == nil {
		t.Error("expected an error when no product lines are found, so the caller keeps its fallback")
	}
}
