import type { Status, Store } from "./api";

export const STATUS_COLOR: Record<Status, string> = {
  in_stock: "#3ddc84",
  short_wait: "#ffd166",
  long_wait: "#ff7043",
  unavailable: "#5b6472",
};

export const STATUS_LABEL: Record<Status, string> = {
  in_stock: "In stock today",
  short_wait: "Short wait",
  long_wait: "Long wait",
  unavailable: "Unavailable",
};

export const STATUS_HINT: Record<Status, string> = {
  in_stock: "On the shelf now",
  short_wait: "Ships to store within a week",
  long_wait: "More than a week out",
  unavailable: "Not offered for pickup here",
};

export const STATUS_ORDER: Status[] = ["in_stock", "short_wait", "long_wait", "unavailable"];

/** waitText renders a store's wait as a compact human phrase. */
export function waitText(store: Store): string {
  if (store.status === "unavailable") return "Unavailable";
  if (store.daysOut <= 0) return "Today";
  if (store.daysOut === 1) return "Tomorrow";
  if (store.daysOut < 14) return `${store.daysOut} days`;
  const weeks = Math.round(store.daysOut / 7);
  return `~${weeks} weeks`;
}
