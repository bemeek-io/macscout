import { useEffect, useState } from "react";
import { getDelivery, type Delivery, type Store, type Variant } from "../lib/api";
import { STATUS_COLOR, STATUS_LABEL, waitText } from "../lib/status";

interface Props {
  store: Store;
  variant: Variant | null;
  onClose: () => void;
}

export default function StorePanel({ store, variant, onClose }: Props) {
  const [delivery, setDelivery] = useState<Delivery | null>(null);

  // The ship-to-home quote is a separate Apple lookup, so it is fetched only
  // once a store — and therefore a postal code — is actually in view.
  useEffect(() => {
    setDelivery(null);
    if (!variant || !store.postalCode) return;
    const ctrl = new AbortController();
    getDelivery(variant.part, store.postalCode, ctrl.signal)
      .then(setDelivery)
      .catch(() => undefined);
    return () => ctrl.abort();
  }, [variant, store.postalCode]);

  const url = variant ? store.pickupUrl : store.storeUrl;

  return (
    <aside className="store-panel" role="dialog" aria-label={`${store.storeName} availability`}>
      <button className="store-close" onClick={onClose} aria-label="Close store details">
        ×
      </button>

      <div className="store-status" style={{ ["--status" as string]: STATUS_COLOR[store.status] }}>
        <span className="store-status-dot" />
        <span>{STATUS_LABEL[store.status]}</span>
        <span className="store-wait">{waitText(store)}</span>
      </div>

      <h2 className="store-name">Apple {store.storeName}</h2>
      <p className="store-address">
        {store.address}
        <br />
        {store.city}, {store.state} {store.postalCode}
      </p>

      <dl className="store-facts">
        <div>
          <dt>Pickup</dt>
          <dd>{store.quote || "—"}</dd>
        </div>
        <div>
          <dt>Delivery</dt>
          <dd>{delivery ? delivery.quote || "Not quoted" : "Checking…"}</dd>
        </div>
        {store.phone && (
          <div>
            <dt>Phone</dt>
            <dd>
              <a href={`tel:${store.phone.replace(/[^\d+]/g, "")}`}>{store.phone}</a>
            </dd>
          </div>
        )}
      </dl>

      {variant && <p className="store-part">{variant.name}</p>}

      <a className="store-cta" href={url} target="_blank" rel="noreferrer">
        Open at Apple {store.storeName} ↗
      </a>
    </aside>
  );
}
