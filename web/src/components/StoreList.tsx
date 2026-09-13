import type { Store } from "../lib/api";
import { STATUS_COLOR, waitText } from "../lib/status";

interface Props {
  stores: Store[];
  selected: Store | null;
  onSelect: (store: Store) => void;
}

export default function StoreList({ stores, selected, onSelect }: Props) {
  if (!stores.length) {
    return <p className="empty">No stores match the current filters.</p>;
  }
  return (
    <ul className="store-list">
      {stores.map((s) => (
        <li key={s.storeNumber}>
          <button
            type="button"
            className={`store-row${selected?.storeNumber === s.storeNumber ? " is-active" : ""}`}
            onClick={() => onSelect(s)}
          >
            <span className="store-row-dot" style={{ background: STATUS_COLOR[s.status] }} />
            <span className="store-row-name">
              {s.storeName}
              <em>
                {s.city}, {s.state}
              </em>
            </span>
            <span className="store-row-wait">{waitText(s)}</span>
          </button>
        </li>
      ))}
    </ul>
  );
}
