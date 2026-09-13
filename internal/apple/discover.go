package apple

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// buyLandingPages are the shop sections that list product lines.
var buyLandingPages = map[string]string{
	"Mac":    "/shop/buy-mac",
	"iPhone": "/shop/buy-iphone",
	"iPad":   "/shop/buy-ipad",
	"Watch":  "/shop/buy-watch",
}

// notProducts are paths under a buy section that are not a product line.
var notProducts = map[string]bool{
	"carrier-offers": true,
	"accessories":    true,
	"trade-in":       true,
}

var buyPathRe = regexp.MustCompile(`/shop/buy-(mac|iphone|ipad|watch)/([a-z0-9][a-z0-9-]*)`)

// DiscoverFamilies reads Apple's buy sections for the product lines currently
// on sale.
//
// A hard-coded list goes stale silently: Apple discontinued iPhone 17 Pro and
// shipped iPhone 18 Pro, and an app carrying last season's list would keep
// asking about a page that now redirects while never showing the replacement.
func (c *Client) DiscoverFamilies(ctx context.Context) ([]Family, error) {
	seen := map[string]Family{}
	var firstErr error

	for group, path := range buyLandingPages {
		body, err := c.get(ctx, path, nil)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", path, err)
			}
			continue
		}
		for _, m := range buyPathRe.FindAllStringSubmatch(string(body), -1) {
			section, slug := m[1], m[2]
			if notProducts[slug] || slug == section {
				continue
			}
			if _, ok := seen[slug]; ok {
				continue
			}
			seen[slug] = Family{
				ID:    slug,
				Name:  familyName(slug),
				Group: group,
				Path:  fmt.Sprintf("/shop/buy-%s/%s", section, slug),
			}
		}
	}

	if len(seen) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("apple: no product lines found in the buy sections")
	}

	out := make([]Family, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	// Group order first so the picker reads like Apple's own navigation.
	order := map[string]int{"Mac": 0, "iPhone": 1, "iPad": 2, "Watch": 3}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return order[out[i].Group] < order[out[j].Group]
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// familyWords maps slug fragments to Apple's own capitalisation.
var familyWords = map[string]string{
	"iphone":  "iPhone",
	"ipad":    "iPad",
	"imac":    "iMac",
	"macbook": "MacBook",
	"mac":     "Mac",
	"apple":   "Apple",
	"watch":   "Watch",
	"se":      "SE",
	"xdr":     "XDR",
	"hermes":  "Hermès",
	"pro":     "Pro",
	"air":     "Air",
	"neo":     "Neo",
	"duo":     "Duo",
	"ultra":   "Ultra",
	"studio":  "Studio",
	"display": "Display",
	// Apple lower-cases "mini" in "Mac mini" and "iPad mini".
	"mini": "mini",
}

// familyName renders a slug the way Apple writes it: "macbook-air" becomes
// "MacBook Air", "mac-mini" becomes "Mac mini".
func familyName(slug string) string {
	parts := strings.Split(slug, "-")
	words := make([]string, 0, len(parts))
	for _, p := range parts {
		if w, ok := familyWords[p]; ok {
			words = append(words, w)
			continue
		}
		// Version fragments such as "10-2" read better joined with a dot.
		if len(words) > 0 && isNumeric(p) && isNumeric(parts[len(words)-1]) {
			words[len(words)-1] += "." + p
			continue
		}
		words = append(words, p)
	}
	return strings.Join(words, " ")
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
