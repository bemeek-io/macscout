import type { Catalog, Dimension, Family, Variant } from "../lib/api";

interface Props {
  families: Family[];
  familyId: string;
  onFamily: (id: string) => void;
  catalog: Catalog | null;
  loadingCatalog: boolean;
  selection: Record<string, string>;
  onSelect: (key: string, value: string) => void;
  variant: Variant | null;
  /** Values that no variant offers under the current selection. */
  blocked: Record<string, Set<string>>;
}

export default function Controls({
  families,
  familyId,
  onFamily,
  catalog,
  loadingCatalog,
  selection,
  onSelect,
  variant,
  blocked,
}: Props) {
  const groups = [...new Set(families.map((f) => f.group))];

  return (
    <section className="controls">
      <label className="field">
        <span className="field-label">Product</span>
        <div className="select-wrap">
          <select value={familyId} onChange={(e) => onFamily(e.target.value)}>
            {groups.map((g) => (
              <optgroup key={g} label={g}>
                {families
                  .filter((f) => f.group === g)
                  .map((f) => (
                    <option key={f.id} value={f.id}>
                      {f.name}
                    </option>
                  ))}
              </optgroup>
            ))}
          </select>
        </div>
      </label>

      {loadingCatalog && <div className="skeleton-stack" aria-label="Loading configuration options" />}

      {catalog?.dimensions.map((dim) => (
        <DimensionPicker
          key={dim.key}
          dim={dim}
          value={selection[dim.key]}
          blocked={blocked[dim.key] ?? new Set()}
          onSelect={(v) => onSelect(dim.key, v)}
        />
      ))}

      {variant && (
        <div className="variant-chip">
          <div className="variant-part">
            <span className="mono">{variant.part}</span>
            {variant.price && <span className="variant-price">{variant.price}</span>}
          </div>
          <a className="variant-link" href={variant.url} target="_blank" rel="noreferrer">
            View on apple.com ↗
          </a>
        </div>
      )}
    </section>
  );
}

function DimensionPicker({
  dim,
  value,
  blocked,
  onSelect,
}: {
  dim: Dimension;
  value?: string;
  blocked: Set<string>;
  onSelect: (value: string) => void;
}) {
  if (dim.kind === "color") {
    return (
      <div className="field">
        <span className="field-label">{dim.label}</span>
        <div className="swatches">
          {dim.options.map((o) => (
            <button
              key={o.value}
              type="button"
              className={`swatch${value === o.value ? " is-active" : ""}${blocked.has(o.value) ? " is-blocked" : ""}`}
              title={blocked.has(o.value) ? `${o.label} — not sold in this configuration` : o.label}
              aria-label={o.label}
              aria-pressed={value === o.value}
              onClick={() => onSelect(o.value)}
            >
              {o.swatch ? <img src={o.swatch} alt="" loading="lazy" /> : <span className="swatch-blank" />}
            </button>
          ))}
        </div>
        <span className="field-value">{dim.options.find((o) => o.value === value)?.label ?? "—"}</span>
      </div>
    );
  }

  return (
    <div className="field">
      <span className="field-label">{dim.label}</span>
      <div className="pills">
        {dim.options.map((o) => (
          <button
            key={o.value}
            type="button"
            className={`pill${value === o.value ? " is-active" : ""}${blocked.has(o.value) ? " is-blocked" : ""}`}
            title={o.detail || o.label}
            aria-pressed={value === o.value}
            onClick={() => onSelect(o.value)}
          >
            {o.label}
          </button>
        ))}
      </div>
    </div>
  );
}
