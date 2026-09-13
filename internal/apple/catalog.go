package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
)

// Family is one shoppable product line, e.g. "macbook-air".
type Family struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
	Path  string `json:"path"`
}

// Families are the buy pages that expose a store-stockable configurator.
// Apple serves each of these with an embedded PRODUCT_SELECTION_BOOTSTRAP blob.
var Families = []Family{
	{ID: "macbook-air", Name: "MacBook Air", Group: "Mac", Path: "/shop/buy-mac/macbook-air"},
	{ID: "macbook-pro", Name: "MacBook Pro", Group: "Mac", Path: "/shop/buy-mac/macbook-pro"},
	{ID: "imac", Name: "iMac", Group: "Mac", Path: "/shop/buy-mac/imac"},
	{ID: "mac-mini", Name: "Mac mini", Group: "Mac", Path: "/shop/buy-mac/mac-mini"},
	{ID: "mac-studio", Name: "Mac Studio", Group: "Mac", Path: "/shop/buy-mac/mac-studio"},
	{ID: "iphone-17-pro", Name: "iPhone 17 Pro", Group: "iPhone", Path: "/shop/buy-iphone/iphone-17-pro"},
	{ID: "iphone-17", Name: "iPhone 17", Group: "iPhone", Path: "/shop/buy-iphone/iphone-17"},
	{ID: "iphone-air", Name: "iPhone Air", Group: "iPhone", Path: "/shop/buy-iphone/iphone-air"},
	{ID: "iphone-16", Name: "iPhone 16", Group: "iPhone", Path: "/shop/buy-iphone/iphone-16"},
	{ID: "ipad-pro", Name: "iPad Pro", Group: "iPad", Path: "/shop/buy-ipad/ipad-pro"},
	{ID: "ipad-air", Name: "iPad Air", Group: "iPad", Path: "/shop/buy-ipad/ipad-air"},
	{ID: "ipad", Name: "iPad", Group: "iPad", Path: "/shop/buy-ipad/ipad"},
	{ID: "ipad-mini", Name: "iPad mini", Group: "iPad", Path: "/shop/buy-ipad/ipad-mini"},
	{ID: "apple-watch", Name: "Apple Watch Series", Group: "Watch", Path: "/shop/buy-watch/apple-watch"},
	{ID: "apple-watch-ultra", Name: "Apple Watch Ultra", Group: "Watch", Path: "/shop/buy-watch/apple-watch-ultra"},
	{ID: "apple-watch-se", Name: "Apple Watch SE", Group: "Watch", Path: "/shop/buy-watch/apple-watch-se"},
}

func FamilyByID(id string) (Family, bool) {
	for _, f := range Families {
		if f.ID == id {
			return f, true
		}
	}
	return Family{}, false
}

// Option is a selectable value within a Dimension, e.g. colour "starlight".
type Option struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	Swatch string `json:"swatch,omitempty"`
}

// Dimension is one filter axis, e.g. colour or screen size.
type Dimension struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Kind    string   `json:"kind"` // "color" renders as swatches in the UI
	Options []Option `json:"options"`
}

// Variant is a single orderable configuration with a real part number.
type Variant struct {
	Part  string            `json:"part"`
	Name  string            `json:"name"`
	Price string            `json:"price,omitempty"`
	Dims  map[string]string `json:"dims"`
	URL   string            `json:"url"`
}

// Catalog is a family's filter axes plus every stockable variant.
type Catalog struct {
	Family     Family      `json:"family"`
	Dimensions []Dimension `json:"dimensions"`
	Variants   []Variant   `json:"variants"`
}

var (
	bootstrapRe = regexp.MustCompile(`window\.PRODUCT_SELECTION_BOOTSTRAP\s*=\s*\{`)
	tagRe       = regexp.MustCompile(`<[^>]+>`)
	// Apple hangs footnote markers off option labels; they render as stray
	// "Footnote 1" text once the tags are stripped.
	footnoteRe = regexp.MustCompile(`(?s)<as-footnote.*?</as-footnote>`)
	// The secondary blurb under an option label is always in a form-label-small
	// element, so it makes a reliable split point.
	subLabelRe = regexp.MustCompile(`<(?:div|span)[^>]*class="[^"]*form-label-small[^"]*"[^>]*>`)
)

// FetchCatalog scrapes a buy page's embedded configurator data. Apple ships two
// shapes of that blob: Macs key products by dimension maps, while iPhone/iPad/
// Watch pages carry a flat product list with the dimensions inlined.
func (c *Client) FetchCatalog(ctx context.Context, f Family) (*Catalog, error) {
	body, err := c.get(ctx, f.Path, nil)
	if err != nil {
		return nil, err
	}
	raw, err := extractBootstrap(string(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.ID, err)
	}

	var psd productSelectionData
	if err := json.Unmarshal(raw, &psd); err != nil {
		return nil, fmt.Errorf("%s: decode bootstrap: %w", f.ID, err)
	}

	cat := &Catalog{Family: f}
	if psd.hasDimensionMaps() {
		buildDimensionalCatalog(cat, &psd)
	} else {
		buildFlatCatalog(cat, &psd)
	}
	if len(cat.Variants) == 0 {
		return nil, fmt.Errorf("%s: no stockable variants found", f.ID)
	}
	finalize(cat)
	return cat, nil
}

// extractBootstrap pulls the productSelectionData object out of the page by
// scanning for its balanced braces; the blob is JS, not a JSON script tag.
func extractBootstrap(page string) (json.RawMessage, error) {
	loc := bootstrapRe.FindStringIndex(page)
	if loc == nil {
		return nil, fmt.Errorf("PRODUCT_SELECTION_BOOTSTRAP not present")
	}
	key := strings.Index(page[loc[1]:], "productSelectionData")
	if key < 0 {
		return nil, fmt.Errorf("productSelectionData not present")
	}
	start := strings.Index(page[loc[1]+key:], "{")
	if start < 0 {
		return nil, fmt.Errorf("productSelectionData is not an object")
	}
	start += loc[1] + key

	depth, inStr, esc := 0, false, false
	for i := start; i < len(page); i++ {
		ch := page[i]
		switch {
		case esc:
			esc = false
		case ch == '\\' && inStr:
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return json.RawMessage(page[start : i+1]), nil
			}
		}
	}
	return nil, fmt.Errorf("productSelectionData object is unterminated")
}

func sortStrings(s []string) { sort.Strings(s) }

// finalize drops axes every variant shares — an aluminium-only Watch SE has no
// case-material choice to make — and renames the variants without them.
func finalize(cat *Catalog) {
	kept := cat.Dimensions[:0]
	for _, d := range cat.Dimensions {
		distinct := make(map[string]bool, 4)
		for _, v := range cat.Variants {
			distinct[v.Dims[d.Key]] = true
		}
		if len(distinct) > 1 {
			// Hide options no stocked variant actually offers.
			opts := d.Options[:0]
			for _, o := range d.Options {
				if distinct[o.Value] {
					opts = append(opts, o)
				}
			}
			d.Options = opts
			kept = append(kept, d)
		}
	}
	cat.Dimensions = kept

	for i := range cat.Variants {
		cat.Variants[i].Name = variantName(cat.Dimensions, cat.Variants[i].Dims)
	}
	sort.Slice(cat.Variants, func(i, j int) bool { return cat.Variants[i].Part < cat.Variants[j].Part })
}

func stripHTML(s string) string {
	s = html.UnescapeString(tagRe.ReplaceAllString(s, " "))
	// Non-breaking spaces and narrow hyphens survive unescaping; normalise them
	// so labels compare and wrap like ordinary text.
	s = strings.NewReplacer("\u00a0", " ", "\u2011", "-", "\u2010", "-").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// splitLabel separates an option's headline from its marketing sub-copy,
// e.g. "10-core CPU, 8-core GPU" + "Brings speed and fluidity to everything".
func splitLabel(raw string) (label, detail string) {
	raw = footnoteRe.ReplaceAllString(raw, "")
	if loc := subLabelRe.FindStringIndex(raw); loc != nil {
		return stripHTML(raw[:loc[0]]), stripHTML(raw[loc[1]:])
	}
	if i := strings.IndexAny(raw, "\n"); i >= 0 {
		return stripHTML(raw[:i]), stripHTML(raw[i:])
	}
	return stripHTML(raw), ""
}
