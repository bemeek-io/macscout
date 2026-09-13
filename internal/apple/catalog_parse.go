package apple

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// productSelectionData covers both bootstrap shapes Apple serves. Macs populate
// the Main* fields; iPhone/iPad/Watch populate Sections/DisplayValues.
type productSelectionData struct {
	MainSections      []section                  `json:"mainSections"`
	MainDisplayValues map[string]json.RawMessage `json:"mainDisplayValues"`
	Sections          []section                  `json:"sections"`
	DisplayValues     map[string]json.RawMessage `json:"displayValues"`
	Products          []json.RawMessage          `json:"products"`
}

type section struct {
	FormFieldName string `json:"formFieldName"`
	Dimension     string `json:"dimension"`
	Header        string `json:"header"`
	View          string `json:"view"`
}

func (s section) key() string {
	if s.Dimension != "" && s.Dimension != "None" {
		return s.Dimension
	}
	return s.FormFieldName
}

// label turns "<span>Color. </span><span class=...>Pick your favorite.</span>"
// into "Color". Some sections carry only instruction copy — "Select a finish:"
// or "Select your processing power." — which becomes the noun it asks for.
func (s section) label() string {
	txt := stripHTML(s.Header)
	if i := strings.Index(txt, "."); i > 0 {
		txt = txt[:i]
	}
	return cleanLabel(txt)
}

var labelPrefixes = []string{"select a ", "select your ", "select ", "choose a ", "choose your ", "choose ", "pick a ", "pick your ", "pick "}

func cleanLabel(txt string) string {
	txt = strings.TrimSpace(strings.Trim(strings.TrimSpace(txt), ".:"))
	lower := strings.ToLower(txt)
	for _, p := range labelPrefixes {
		if strings.HasPrefix(lower, p) {
			txt = strings.TrimSpace(txt[len(p):])
			break
		}
	}
	if txt == "" {
		return ""
	}
	if first := txt[0]; first >= 'a' && first <= 'z' {
		txt = string(first-32) + txt[1:]
	}
	return txt
}

func (s section) kind() string {
	if strings.Contains(strings.ToLower(s.View), "color") {
		return "color"
	}
	return "list"
}

type macProduct struct {
	Part string `json:"btrOrFdPartNumber"`
	// AltPart is the Watch pages' spelling of the same field.
	AltPart      string            `json:"part"`
	PriceKey     string            `json:"priceKey"`
	Dimensions   map[string]string `json:"dimensions"`
	IsComingSoon bool              `json:"isComingSoon"`
}

type priceEntry struct {
	Amount              float64 `json:"amount"`
	AmountBeforeTradeIn float64 `json:"amountBeforeTradeIn"`
}

var imgSrcRe = regexp.MustCompile(`src="([^"]+)"`)

// buildDimensionalCatalog handles the Mac shape: products keyed by a dimension
// map, where only btrOrFdPartNumber entries are stocked in retail stores.
func buildDimensionalCatalog(cat *Catalog, psd *productSelectionData) {
	sections, values := psd.dimensionSource()
	prices := decodePrices(values["prices"])
	buildMacDimensions(cat, sections, values)
	addMissingDimensions(cat, psd, values)

	for _, raw := range psd.Products {
		var p macProduct
		if err := json.Unmarshal(raw, &p); err != nil || p.IsComingSoon {
			continue
		}
		part := p.Part
		if part == "" {
			part = p.AltPart
		}
		if part == "" {
			continue
		}
		cat.Variants = append(cat.Variants, Variant{
			Part:  part,
			Name:  variantName(cat.Dimensions, p.Dimensions),
			Price: formatPrice(prices[p.PriceKey]),
			Dims:  p.Dimensions,
			URL:   ProductURL(part),
		})
	}
}

// dimensionSource picks whichever pair of section/display-value fields this
// page populated. Macs use the Main* fields; everything else uses the plain
// ones, and Watch pages mix a plain section list with Mac-shaped products.
func (psd *productSelectionData) dimensionSource() ([]section, map[string]json.RawMessage) {
	if len(psd.MainSections) > 0 {
		return psd.MainSections, psd.MainDisplayValues
	}
	return psd.Sections, psd.DisplayValues
}

// hasDimensionMaps reports whether products carry a nested dimensions object
// (Mac and Watch) rather than inline dimension fields (iPhone, iPad).
func (psd *productSelectionData) hasDimensionMaps() bool {
	for _, raw := range psd.Products {
		var probe struct {
			Dimensions map[string]string `json:"dimensions"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && len(probe.Dimensions) > 0 {
			return true
		}
	}
	return false
}

func buildMacDimensions(cat *Catalog, sections []section, values map[string]json.RawMessage) {
	for _, s := range sections {
		key := s.key()
		raw, ok := values[key]
		if !ok {
			continue
		}
		var vals map[string]json.RawMessage
		if err := json.Unmarshal(raw, &vals); err != nil {
			continue
		}
		dim := Dimension{Key: key, Label: s.label(), Kind: s.kind()}
		for _, v := range decodeVariantOrder(raw) {
			// Watch pages describe options with the "value"/"image" pair even
			// though their products are Mac-shaped, so accept either field.
			var entry struct {
				Header string `json:"header"`
				Value  string `json:"value"`
				Image  struct {
					Sources []struct {
						SrcSet string `json:"srcSet"`
					} `json:"sources"`
				} `json:"image"`
				ImageHTML string `json:"-"`
			}
			if err := json.Unmarshal(vals[v], &entry); err != nil {
				continue
			}
			// Watch case options carry only a swatch image, so fall back to
			// the option key itself rather than dropping the choice.
			text := entry.Header
			if text == "" {
				text = entry.Value
			}
			if text == "" {
				text = fallbackLabel(key, v)
			}
			label, detail := splitLabel(text)
			opt := Option{Value: v, Label: label, Detail: detail}
			if len(entry.Image.Sources) > 0 {
				opt.Swatch = entry.Image.Sources[0].SrcSet
			} else if m := imgSrcRe.FindStringSubmatch(rawImageString(vals[v])); m != nil {
				opt.Swatch = strings.ReplaceAll(m[1], "&amp;", "&")
			}
			dim.Options = append(dim.Options, opt)
		}
		if len(dim.Options) > 0 {
			cat.Dimensions = append(cat.Dimensions, dim)
		}
	}
}

// addMissingDimensions covers axes the products vary along but the page's
// section list omits — Watch Ultra, for instance, lists only the case finish
// as a section while its products also vary by case size.
func addMissingDimensions(cat *Catalog, psd *productSelectionData, values map[string]json.RawMessage) {
	known := make(map[string]bool, len(cat.Dimensions))
	for _, d := range cat.Dimensions {
		known[d.Key] = true
	}

	var order []string
	seen := make(map[string]map[string]bool)
	for _, raw := range psd.Products {
		var p macProduct
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		for k, v := range p.Dimensions {
			if known[k] {
				continue
			}
			if seen[k] == nil {
				seen[k] = map[string]bool{}
				order = append(order, k)
			}
			seen[k][v] = true
		}
	}
	sortStrings(order)

	for _, key := range order {
		dim := Dimension{Key: key, Label: humanize(dimensionSuffix(key)), Kind: "list"}
		vals := make([]string, 0, len(seen[key]))
		for v := range seen[key] {
			vals = append(vals, v)
		}
		sortStrings(vals)
		// Prefer Apple's own ordering when the page happens to carry it.
		if raw, ok := values[key]; ok {
			if ordered := decodeVariantOrder(raw); len(ordered) > 0 {
				vals = intersect(ordered, seen[key])
			}
		}
		for _, v := range vals {
			opt := Option{Value: v, Label: fallbackLabel(key, v)}
			if raw, ok := values[key]; ok {
				if lbl, swatch := lookupOption(raw, v); lbl != "" || swatch != "" {
					if lbl != "" {
						opt.Label = lbl
					}
					opt.Swatch = swatch
					if swatch != "" {
						dim.Kind = "color"
					}
				}
			}
			dim.Options = append(dim.Options, opt)
		}
		if len(dim.Options) > 0 {
			cat.Dimensions = append(cat.Dimensions, dim)
		}
	}
}

// lookupOption reads one option's label and swatch out of a display-values map.
func lookupOption(raw json.RawMessage, value string) (label, swatch string) {
	var vals map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vals); err != nil {
		return "", ""
	}
	entry, ok := vals[value]
	if !ok {
		return "", ""
	}
	var decoded struct {
		Header string `json:"header"`
		Value  string `json:"value"`
		Image  struct {
			Sources []struct {
				SrcSet string `json:"srcSet"`
			} `json:"sources"`
		} `json:"image"`
	}
	if err := json.Unmarshal(entry, &decoded); err != nil {
		return "", ""
	}
	text := decoded.Header
	if text == "" {
		text = decoded.Value
	}
	if text != "" {
		label, _ = splitLabel(text)
	}
	if len(decoded.Image.Sources) > 0 {
		swatch = decoded.Image.Sources[0].SrcSet
	}
	return label, swatch
}

func intersect(order []string, want map[string]bool) []string {
	out := make([]string, 0, len(want))
	for _, v := range order {
		if want[v] {
			out = append(out, v)
		}
	}
	return out
}

// dimensionSuffix reduces a key like "watch_cases-dimensionCaseSize" to
// "CaseSize", which humanize can render as a heading.
func dimensionSuffix(key string) string {
	if i := strings.LastIndex(key, "dimension"); i >= 0 {
		return key[i+len("dimension"):]
	}
	if i := strings.LastIndex(key, "-"); i >= 0 {
		return key[i+1:]
	}
	return key
}

var coreCountRe = regexp.MustCompile(`(\d+)-(\d+)$`)

// fallbackLabel names an option Apple left undescribed. Core-count axes ship
// as bare values like "m6-12-12", which read as nothing without expansion.
func fallbackLabel(dimKey, value string) string {
	if strings.Contains(dimKey, "cpuCoreCount") && strings.Contains(dimKey, "gpuCoreCount") {
		if m := coreCountRe.FindStringSubmatch(value); m != nil {
			return fmt.Sprintf("%s-core CPU, %s-core GPU", m[1], m[2])
		}
	}
	return humanize(value)
}

// humanize renders an Apple option key as a label: "caseSize" becomes
// "Case size" and "natural" becomes "Natural", while "49mm" is left alone.
func humanize(v string) string {
	if v == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range v {
		switch {
		case r == '_' || r == '-':
			b.WriteByte(' ')
		case r >= 'A' && r <= 'Z' && i > 0:
			b.WriteByte(' ')
			b.WriteRune(r + 32)
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return v
	}
	if first := out[0]; first >= 'a' && first <= 'z' {
		out = string(first-32) + out[1:]
	}
	return out
}

// rawImageString reads an option's "image" field when it is an HTML string
// rather than the structured object Macs use.
func rawImageString(raw json.RawMessage) string {
	var entry struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}
	return entry.Image
}

// buildFlatCatalog handles the iPhone/iPad/Watch shape: a flat product list
// carrying its dimension values as top-level fields.
func buildFlatCatalog(cat *Catalog, psd *productSelectionData) {
	sections, values := psd.dimensionSource()
	prices := decodePrices(values["prices"])

	for _, s := range sections {
		key := s.key()
		raw, ok := values[key]
		if !ok {
			continue
		}
		var vals map[string]json.RawMessage
		if err := json.Unmarshal(raw, &vals); err != nil {
			continue
		}
		dim := Dimension{Key: key, Label: s.label(), Kind: s.kind()}
		for _, v := range decodeVariantOrder(raw) {
			var entry struct {
				Value string `json:"value"`
				Image string `json:"image"`
			}
			if err := json.Unmarshal(vals[v], &entry); err != nil || entry.Value == "" {
				continue
			}
			label, detail := splitLabel(entry.Value)
			opt := Option{Value: v, Label: label, Detail: detail}
			if m := imgSrcRe.FindStringSubmatch(entry.Image); m != nil {
				opt.Swatch = strings.ReplaceAll(m[1], "&amp;", "&")
			}
			dim.Options = append(dim.Options, opt)
		}
		if len(dim.Options) > 0 {
			cat.Dimensions = append(cat.Dimensions, dim)
		}
	}

	// Carrier rows repeat a part number across financing options; keep one row
	// per distinct part + dimension combination.
	seen := make(map[string]bool)
	for _, raw := range psd.Products {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		part, _ := p["partNumber"].(string)
		if part == "" {
			continue
		}
		if soon, _ := p["comingSoon"].(bool); soon {
			continue
		}
		dims := make(map[string]string, len(cat.Dimensions))
		for _, d := range cat.Dimensions {
			if v, ok := p[d.Key].(string); ok {
				dims[d.Key] = v
			}
		}
		sig := part + "|" + strings.Join(dimValues(cat.Dimensions, dims), "|")
		if seen[sig] {
			continue
		}
		seen[sig] = true
		fp, _ := p["fullPrice"].(string)
		cat.Variants = append(cat.Variants, Variant{
			Part:  part,
			Name:  variantName(cat.Dimensions, dims),
			Price: formatPrice(prices[fp]),
			Dims:  dims,
			URL:   ProductURL(part),
		})
	}
}

func dimValues(dims []Dimension, m map[string]string) []string {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, m[d.Key])
	}
	return out
}

// decodeVariantOrder reads Apple's display ordering hint, falling back to the
// raw keys minus the bookkeeping entries that share the same map.
func decodeVariantOrder(raw json.RawMessage) []string {
	var probe struct {
		VariantOrder []string `json:"variantOrder"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.VariantOrder) > 0 {
		return probe.VariantOrder
	}
	var vals map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vals); err != nil {
		return nil
	}
	out := make([]string, 0, len(vals))
	for k := range vals {
		if k != "variantOrder" && k != "title" {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}

func decodePrices(raw json.RawMessage) map[string]priceEntry {
	out := map[string]priceEntry{}
	if len(raw) == 0 {
		return out
	}
	var vals map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vals); err != nil {
		return out
	}
	for k, v := range vals {
		var p priceEntry
		if err := json.Unmarshal(v, &p); err == nil {
			out[k] = p
		}
	}
	return out
}

func formatPrice(p priceEntry) string {
	amt := p.Amount
	if amt == 0 {
		amt = p.AmountBeforeTradeIn
	}
	if amt == 0 {
		return ""
	}
	return "$" + trimZeros(amt)
}

func trimZeros(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.2f", f), ".00")
}

// variantName renders a human title like "15-inch · Starlight · 10-core CPU".
func variantName(dims []Dimension, values map[string]string) string {
	var parts []string
	for _, d := range dims {
		v, ok := values[d.Key]
		if !ok {
			continue
		}
		for _, o := range d.Options {
			if o.Value == v {
				parts = append(parts, o.Label)
				break
			}
		}
	}
	return strings.Join(parts, " · ")
}

// ProductURL is Apple's canonical shortcut for a part number; it redirects to
// the buy page preconfigured for that exact variant.
func ProductURL(part string) string {
	return baseURL + "/shop/product/" + part
}
