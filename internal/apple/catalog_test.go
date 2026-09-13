package apple

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractBootstrap(t *testing.T) {
	page := `<html><script>
	window.PRODUCT_SELECTION_BOOTSTRAP = {
		productSelectionData: {"a":1,"nested":{"b":"}"},"s":"a \" quoted } brace"},
		other: 2
	};
	</script></html>`

	raw, err := extractBootstrap(page)
	if err != nil {
		t.Fatalf("extractBootstrap: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, raw)
	}
	if got["a"] != float64(1) {
		t.Errorf("a = %v, want 1", got["a"])
	}
	// Braces inside strings must not terminate the object early.
	if got["s"] != `a " quoted } brace` {
		t.Errorf("s = %q, want the full quoted string", got["s"])
	}
}

func TestExtractBootstrapMissing(t *testing.T) {
	if _, err := extractBootstrap("<html>no configurator here</html>"); err == nil {
		t.Fatal("expected an error for a page with no bootstrap")
	}
}

func TestSplitLabel(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantLabel  string
		wantDetail string
	}{
		{
			name:      "footnote markers are dropped",
			raw:       `13-inch<as-footnote data-id="x"><sup class="as-footnote"><span class="visuallyhidden">&nbsp;Footnote&nbsp;</span>1</sup></as-footnote>`,
			wantLabel: "13-inch",
		},
		{
			name:       "sub-copy splits off",
			raw:        `10&#8209;core&nbsp;CPU, 8&#8209;core&nbsp;GPU <div class="form-label-small">Brings speed and fluidity.</div>`,
			wantLabel:  "10-core CPU, 8-core GPU",
			wantDetail: "Brings speed and fluidity.",
		},
		{
			name:       "newline splits when there is no sub-element",
			raw:        "iPhone&nbsp;17&nbsp;Pro\nsecondary text",
			wantLabel:  "iPhone 17 Pro",
			wantDetail: "secondary text",
		},
		{
			name:      "plain label passes through",
			raw:       "Starlight",
			wantLabel: "Starlight",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, detail := splitLabel(tt.raw)
			if label != tt.wantLabel {
				t.Errorf("label = %q, want %q", label, tt.wantLabel)
			}
			if detail != tt.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tt.wantDetail)
			}
		})
	}
}

func TestBuildDimensionalCatalog(t *testing.T) {
	// Shaped like a Mac buy page: only btrOrFdPartNumber rows are stocked.
	raw := `{
	  "mainSections": [
	    {"formFieldName":"chassis-dimensionColor","header":"<span>Color. </span><span>Pick one.</span>","view":"selectorColorNav"}
	  ],
	  "mainDisplayValues": {
	    "chassis-dimensionColor": {
	      "variantOrder": ["starlight","midnight"],
	      "starlight": {"header":"Starlight","image":{"sources":[{"srcSet":"https://img/star.jpg"}]}},
	      "midnight": {"header":"Midnight","image":{"sources":[{"srcSet":"https://img/mid.jpg"}]}}
	    },
	    "prices": {"k-star": {"amount": 1499.0}}
	  },
	  "products": [
	    {"btrOrFdPartNumber":"MDVD4LL/A","priceKey":"k-star","dimensions":{"chassis-dimensionColor":"starlight"}},
	    {"btrOrFdPartNumber":null,"dimensions":{"chassis-dimensionColor":"midnight"}},
	    {"btrOrFdPartNumber":"SOON1LL/A","isComingSoon":true,"dimensions":{"chassis-dimensionColor":"midnight"}}
	  ]
	}`

	var psd productSelectionData
	if err := json.Unmarshal([]byte(raw), &psd); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	cat := &Catalog{}
	buildDimensionalCatalog(cat, &psd)

	if len(cat.Dimensions) != 1 {
		t.Fatalf("got %d dimensions, want 1", len(cat.Dimensions))
	}
	dim := cat.Dimensions[0]
	if dim.Label != "Color" || dim.Kind != "color" {
		t.Errorf("dimension = %q/%q, want Color/color", dim.Label, dim.Kind)
	}
	if len(dim.Options) != 2 || dim.Options[0].Value != "starlight" {
		t.Errorf("options = %+v, want starlight first", dim.Options)
	}
	if dim.Options[0].Swatch != "https://img/star.jpg" {
		t.Errorf("swatch = %q", dim.Options[0].Swatch)
	}

	// Configurable and coming-soon rows are not stocked in stores.
	if len(cat.Variants) != 1 {
		t.Fatalf("got %d variants, want only the stocked one: %+v", len(cat.Variants), cat.Variants)
	}
	v := cat.Variants[0]
	if v.Part != "MDVD4LL/A" || v.Price != "$1499" || v.Name != "Starlight" {
		t.Errorf("variant = %+v", v)
	}
	if !strings.HasSuffix(v.URL, "/shop/product/MDVD4LL/A") {
		t.Errorf("url = %q", v.URL)
	}
}

func TestBuildFlatCatalog(t *testing.T) {
	// Shaped like an iPhone buy page: dimensions inline on each product, and
	// the same part repeated once per carrier.
	raw := `{
	  "sections": [
	    {"formFieldName":"dimensionCapacity","dimension":"dimensionCapacity","header":"<span>Storage. </span>","view":"selectorFullWidth"}
	  ],
	  "displayValues": {
	    "dimensionCapacity": {
	      "title": {"singleVariantDisplayTitle":""},
	      "256gb": {"value":"256 GB","image":""},
	      "512gb": {"value":"512 GB","image":""}
	    },
	    "prices": {"p256": {"amountBeforeTradeIn": 1099.0}}
	  },
	  "products": [
	    {"partNumber":"MFXG4LL/A","dimensionCapacity":"256gb","fullPrice":"p256"},
	    {"partNumber":"MFXG4LL/A","dimensionCapacity":"256gb","fullPrice":"p256"},
	    {"partNumber":"MFXH4LL/A","dimensionCapacity":"512gb","comingSoon":true}
	  ]
	}`

	var psd productSelectionData
	if err := json.Unmarshal([]byte(raw), &psd); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	cat := &Catalog{}
	buildFlatCatalog(cat, &psd)

	if len(cat.Dimensions) != 1 || len(cat.Dimensions[0].Options) != 2 {
		t.Fatalf("dimensions = %+v", cat.Dimensions)
	}
	if len(cat.Variants) != 1 {
		t.Fatalf("got %d variants, want 1 after dedupe: %+v", len(cat.Variants), cat.Variants)
	}
	if cat.Variants[0].Price != "$1099" {
		t.Errorf("price = %q, want $1099", cat.Variants[0].Price)
	}
}

func TestEncodeLooseKeepsPartSlashes(t *testing.T) {
	got := encodeLoose(map[string][]string{"parts.0": {"MDVD4LL/A"}, "location": {"94301"}})
	want := "location=94301&parts.0=MDVD4LL/A"
	if got != want {
		t.Errorf("encodeLoose() = %q, want %q", got, want)
	}
}
