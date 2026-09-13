import type { Status, Summary } from "../lib/api";
import { STATUS_COLOR, STATUS_HINT, STATUS_LABEL, STATUS_ORDER } from "../lib/status";

interface Props {
  summary: Summary;
  active: Set<Status>;
  onToggle: (status: Status) => void;
}

const COUNT: Record<Status, keyof Summary> = {
  in_stock: "inStock",
  short_wait: "shortWait",
  long_wait: "longWait",
  unavailable: "unavailable",
};

export default function Legend({ summary, active, onToggle }: Props) {
  return (
    <div className="legend">
      {STATUS_ORDER.map((status) => {
        const count = summary[COUNT[status]] as number;
        const on = active.has(status);
        return (
          <button
            key={status}
            type="button"
            className={`legend-row${on ? "" : " is-off"}`}
            onClick={() => onToggle(status)}
            title={`${STATUS_HINT[status]} — click to ${on ? "hide" : "show"}`}
            aria-pressed={on}
          >
            <span className="legend-dot" style={{ background: STATUS_COLOR[status] }} />
            <span className="legend-label">{STATUS_LABEL[status]}</span>
            <span className="legend-count">{count}</span>
          </button>
        );
      })}
    </div>
  );
}
