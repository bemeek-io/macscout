export type Status = "in_stock" | "short_wait" | "long_wait" | "unavailable";

export interface Family {
  id: string;
  name: string;
  group: string;
  path: string;
}

export interface Option {
  value: string;
  label: string;
  detail?: string;
  swatch?: string;
}

export interface Dimension {
  key: string;
  label: string;
  kind: "color" | "list";
  options: Option[];
}

export interface Variant {
  part: string;
  name: string;
  price?: string;
  dims: Record<string, string>;
  url: string;
}

export interface Catalog {
  family: Family;
  dimensions: Dimension[];
  variants: Variant[];
}

export interface Store {
  storeNumber: string;
  storeName: string;
  city: string;
  state: string;
  address: string;
  postalCode: string;
  phone: string;
  lat: number;
  lng: number;
  status: Status;
  quote: string;
  pickupUrl: string;
  storeUrl: string;
  daysOut: number;
}

export interface Summary {
  total: number;
  inStock: number;
  shortWait: number;
  longWait: number;
  unavailable: number;
}

export interface ScanResult {
  part: string;
  scope: string;
  stores: Store[];
  summary: Summary;
  fetchedAt: string;
  partial: boolean;
  throttled: boolean;
  anchors: number;
  covered: number;
  remaining: number;
  retryAfter?: number;
}

export interface Delivery {
  quote: string;
  option: string;
  cost: string;
  buyable: boolean;
  location: string;
}

async function json<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(url, { signal });
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: string } | null;
    throw new Error(body?.error ?? `${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}

export const getFamilies = (signal?: AbortSignal) => json<Family[]>("/api/families", signal);

export const getCatalog = (id: string, signal?: AbortSignal) =>
  json<Catalog>(`/api/catalog/${encodeURIComponent(id)}`, signal);

export const getDelivery = (part: string, zip: string, signal?: AbortSignal) =>
  json<Delivery>(`/api/delivery?part=${encodeURIComponent(part)}&zip=${encodeURIComponent(zip)}`, signal);

/** stopScan ends the server-side sweep, keeping whatever it has already found. */
export async function stopScan(part: string, zip: string | null): Promise<void> {
  const params = new URLSearchParams({ part });
  if (zip) params.set("zip", zip);
  await fetch(`/api/scan/stop?${params}`, { method: "POST" }).catch(() => undefined);
}

export interface ScanBatch {
  zip: string;
  stores: Store[] | null;
  done: number;
  total: number;
  error?: string;
  throttled?: boolean;
}

export interface Availability {
  part: string;
  stores: Store[];
  summary: Summary;
  /** null until the part has been swept at least once. */
  syncedAt: string | null;
  complete: boolean;
  anchors: number;
  covered: number;
  syncing: boolean;
  tracked: boolean;
}

/**
 * getAvailability reads the local dataset. It never waits on apple.com, so it
 * answers immediately whether or not a sweep is running.
 */
export function getAvailability(
  variant: { part: string; name: string; price?: string },
  family: string,
  signal?: AbortSignal,
): Promise<Availability> {
  const params = new URLSearchParams({
    part: variant.part,
    family,
    name: variant.name,
    price: variant.price ?? "",
  });
  return json<Availability>(`/api/availability?${params}`, signal);
}

/** How far the background rotation has got through the lineup. */
export interface RotationStatus {
  lineup: number;
  configurations: number;
  synced: number;
  complete: number;
  syncing?: string;
  throttled: boolean;
}

export const getStatus = (signal?: AbortSignal) => json<RotationStatus>("/api/status", signal);

export interface ScanWaiting {
  retryAfter: number;
  remaining: number;
}

export interface ScanHandlers {
  onStart?: (total: number, covered: number) => void;
  onBatch: (batch: ScanBatch) => void;
  /** The sweep is pausing for apple.com's cooldown and will carry on by itself. */
  onWaiting: (waiting: ScanWaiting) => void;
  onNote?: (message: string) => void;
  onDone: (result: ScanResult) => void;
  onError: (message: string) => void;
}

/**
 * streamScan subscribes to a sweep over Server-Sent Events so stores appear on
 * the map as each anchor ZIP reports, instead of after the whole sweep.
 * Returns a function that aborts the stream.
 */
export function streamScan(part: string, zip: string | null, handlers: ScanHandlers): () => void {
  const params = new URLSearchParams({ part });
  if (zip) params.set("zip", zip);

  const source = new EventSource(`/api/scan/stream?${params}`);
  let finished = false;

  source.addEventListener("start", (e) => {
    const data = JSON.parse((e as MessageEvent).data) as { total: number; covered: number };
    handlers.onStart?.(data.total, data.covered);
  });
  source.addEventListener("batch", (e) => {
    handlers.onBatch(JSON.parse((e as MessageEvent).data) as ScanBatch);
  });
  source.addEventListener("done", (e) => {
    finished = true;
    handlers.onDone(JSON.parse((e as MessageEvent).data) as ScanResult);
    source.close();
  });
  source.addEventListener("waiting", (e) => {
    handlers.onWaiting(JSON.parse((e as MessageEvent).data) as ScanWaiting);
  });
  source.addEventListener("note", (e) => {
    handlers.onNote?.((JSON.parse((e as MessageEvent).data) as { message: string }).message);
  });
  // EventSource reports every transport failure the same way, so only surface
  // one if the stream never reached its "done" event.
  source.onerror = () => {
    if (!finished) handlers.onError("Lost connection to the scan stream.");
    source.close();
  };

  return () => {
    finished = true;
    source.close();
  };
}
