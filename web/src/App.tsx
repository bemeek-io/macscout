import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  getAvailability,
  getCatalog,
  getFamilies,
  getStatus,
  stopScan,
  streamScan,
  type Catalog,
  type Family,
  type ScanResult,
  type Status,
  type RotationStatus,
  type Store,
  type Summary,
  type Variant,
} from "./lib/api";
import { STATUS_ORDER } from "./lib/status";
import MapView from "./components/MapView";
import Controls from "./components/Controls";
import Legend from "./components/Legend";
import StoreList from "./components/StoreList";
import StorePanel from "./components/StorePanel";
import ResumeNotice from "./components/ResumeNotice";
import SyncStatus from "./components/SyncStatus";
import PerfHud from "./components/PerfHud";

const EMPTY_SUMMARY: Summary = { total: 0, inStock: 0, shortWait: 0, longWait: 0, unavailable: 0 };

interface Progress {
  done: number;
  total: number;
}

export default function App() {
  const [families, setFamilies] = useState<Family[]>([]);
  const [familyId, setFamilyId] = useState("macbook-air");
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [loadingCatalog, setLoadingCatalog] = useState(true);
  const [selection, setSelection] = useState<Record<string, string>>({});

  const [stores, setStores] = useState<Store[]>([]);
  const [summary, setSummary] = useState<Summary>(EMPTY_SUMMARY);
  const [progress, setProgress] = useState<Progress | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [zip, setZip] = useState("");
  const [scanZip, setScanZip] = useState<string | null>(null);
  const [heatmap, setHeatmap] = useState(false);
  // Place names cost a second full-screen layer to composite. Remembered per
  // browser so a slow machine only has to be told once.
  const [showLabels, setShowLabels] = useState(() => {
    try {
      return localStorage.getItem("macscout.labels") !== "off";
    } catch {
      return true;
    }
  });
  const [visible, setVisible] = useState<Set<Status>>(new Set(STATUS_ORDER));
  // Selection is held as a store number and resolved against the current data,
  // not as a snapshot of the store. A snapshot goes stale the moment the data
  // refreshes or the product changes, and the panel would then contradict the
  // dot it was opened from.
  const [selectedNumber, setSelectedNumber] = useState<string | null>(null);
  const setSelected = useCallback(
    (store: Store | null) => setSelectedNumber(store?.storeNumber ?? null),
    [],
  );
  const [flyTo, setFlyTo] = useState<{ lng: number; lat: number; zoom: number } | null>(null);
  const [fitVersion, setFitVersion] = useState(0);
  const [resume, setResume] = useState<{ at: number; remaining: number } | null>(null);
  const [rotation, setRotation] = useState<RotationStatus | null>(null);
  const [sync, setSync] = useState<{
    syncedAt: string | null;
    complete: boolean;
    covered: number;
    anchors: number;
    syncing: boolean;
  } | null>(null);

  const cancelScan = useRef<(() => void) | null>(null);
  // The map is framed once, on the first data that arrives. Every product
  // shares the same store locations, so refitting when the configuration
  // changes only throws away wherever the reader had panned to.
  const hasFramed = useRef(false);

  // How far the background rotation has got, so the answer to "is everything
  // there yet" is on the page rather than in a log.
  useEffect(() => {
    const ctrl = new AbortController();
    const load = () => getStatus(ctrl.signal).then(setRotation).catch(() => undefined);
    void load();
    const id = window.setInterval(load, 20000);
    return () => {
      ctrl.abort();
      window.clearInterval(id);
    };
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    getFamilies(ctrl.signal)
      .then(setFamilies)
      .catch((e: Error) => setError(e.message));
    return () => ctrl.abort();
  }, []);

  // Loading a family resets the configuration to Apple's first listed option
  // on every axis, which is always a real, buyable combination.
  useEffect(() => {
    const ctrl = new AbortController();
    setLoadingCatalog(true);
    setCatalog(null);
    setSelected(null);
    getCatalog(familyId, ctrl.signal)
      .then((cat) => {
        setCatalog(cat);
        const first = cat.variants[0];
        const next: Record<string, string> = {};
        for (const dim of cat.dimensions) {
          next[dim.key] = first?.dims[dim.key] ?? dim.options[0]?.value ?? "";
        }
        setSelection(next);
        setError(null);
      })
      .catch((e: Error) => {
        if (e.name === "AbortError") return;
        // The options come from apple.com and the availability data does not,
        // so say which one is missing rather than implying the whole app is
        // down.
        setError(
          `Can't load the ${familyId.replace(/-/g, " ")} options from apple.com right now. ` +
            `Stored availability for other products is unaffected.`,
        );
      })
      .finally(() => setLoadingCatalog(false));
    return () => ctrl.abort();
  }, [familyId]);

  const variant = useMemo<Variant | null>(() => {
    if (!catalog) return null;
    const exact = catalog.variants.find((v) =>
      catalog.dimensions.every((d) => v.dims[d.key] === selection[d.key]),
    );
    return exact ?? null;
  }, [catalog, selection]);

  // Apple does not sell every dimension combination; grey out the values that
  // no remaining variant can satisfy given the other choices.
  const blocked = useMemo(() => {
    const out: Record<string, Set<string>> = {};
    if (!catalog) return out;
    for (const dim of catalog.dimensions) {
      const unreachable = new Set<string>();
      for (const opt of dim.options) {
        const possible = catalog.variants.some((v) =>
          catalog.dimensions.every((d) =>
            d.key === dim.key ? v.dims[d.key] === opt.value : v.dims[d.key] === selection[d.key],
          ),
        );
        if (!possible) unreachable.add(opt.value);
      }
      out[dim.key] = unreachable;
    }
    return out;
  }, [catalog, selection]);

  /**
   * Choosing a value can strand the rest of the configuration on a combination
   * Apple does not sell, so snap the other axes to the closest real variant.
   */
  const handleSelect = useCallback(
    (key: string, value: string) => {
      setSelection((prev) => {
        if (!catalog) return { ...prev, [key]: value };
        const wanted = { ...prev, [key]: value };
        const exact = catalog.variants.find((v) =>
          catalog.dimensions.every((d) => v.dims[d.key] === wanted[d.key]),
        );
        if (exact) return wanted;

        const best = catalog.variants
          .filter((v) => v.dims[key] === value)
          .map((v) => ({
            v,
            score: catalog.dimensions.filter((d) => v.dims[d.key] === wanted[d.key]).length,
          }))
          .sort((a, b) => b.score - a.score)[0];
        if (!best) return wanted;

        const next: Record<string, string> = {};
        for (const d of catalog.dimensions) next[d.key] = best.v.dims[d.key];
        return next;
      });
    },
    [catalog],
  );

  const runScan = useCallback(
    (part: string, targetZip: string | null) => {
      cancelScan.current?.();
      setStores([]);
      setSummary(EMPTY_SUMMARY);
      setSelected(null);
      setResume(null);
      setError(null);
      setProgress({ done: 0, total: targetZip ? 1 : 0 });

      cancelScan.current = streamScan(part, targetZip, {
        onStart: (total, covered) => setProgress({ done: covered, total }),
        onBatch: (batch) => {
          setProgress({ done: batch.done, total: batch.total });
          if (batch.stores?.length) {
            setStores((prev) => {
              // A resumed sweep replays anchors, so drop repeats.
              const known = new Set(prev.map((s) => s.storeNumber));
              const added = batch.stores!.filter((s) => !known.has(s.storeNumber));
              return added.length ? [...prev, ...added] : prev;
            });
          }
        },
        // The server pauses for apple.com's cooldown and carries on by
        // itself; this only reports the wait.
        onWaiting: ({ retryAfter, remaining }) => {
          setProgress(null);
          setResume({ at: Date.now() + retryAfter * 1000, remaining });
        },
        onNote: (message) => setError(message),
        onDone: (result: ScanResult) => {
          setStores(result.stores);
          setSummary(result.summary);
          setProgress(null);
          setResume(null);
          cancelScan.current = null;
          if (result.stores.length) setFitVersion((v) => v + 1);
          if (result.partial && result.covered > 0) {
            setError(
              `Stopped with ${result.covered} of ${result.anchors} regions in. Press Scan to carry on.`,
            );
          }
        },
        onError: (message) => {
          setError(message);
          setProgress(null);
          setResume(null);
          cancelScan.current = null;
        },
      });
    },
    [],
  );

  useEffect(() => () => cancelScan.current?.(), []);

  /**
   * Selecting a configuration reads the stored dataset, which is instant, and
   * registers the part so the background rotation keeps it fresh. A sweep only
   * happens if you ask for one.
   */
  useEffect(() => {
    // No resolved configuration means nothing legitimate to draw. Returning
    // early here left the previous product's stores on the map, labelled with
    // the newly selected product — showing the wrong answer confidently, which
    // is worse than showing none.
    if (!variant) {
      setStores([]);
      setSummary(EMPTY_SUMMARY);
      setSelected(null);
      setSync(null);
      return;
    }

    const ctrl = new AbortController();
    let live = true;
    let timer = 0;
    let lastSignature = "";

    const load = () =>
      getAvailability(variant, familyId, ctrl.signal)
        .then((data) => {
          if (!live) return;

          setSync({
            syncedAt: data.syncedAt,
            complete: data.complete,
            covered: data.covered,
            anchors: data.anchors,
            syncing: data.syncing,
          });

          // Only touch the map when the data actually moved. A poll that
          // changes nothing must not restyle every marker — or, worse, close
          // the store panel the reader is in the middle of.
          const signature = `${data.syncedAt}|${data.stores.length}|${JSON.stringify(data.summary)}`;
          if (signature !== lastSignature) {
            lastSignature = signature;
            setStores(data.stores);
            setSummary(data.summary);
            if (!hasFramed.current && data.stores.length) {
              hasFramed.current = true;
              setFitVersion((v) => v + 1);
            }
          }

          // Poll briskly only while something is actually refreshing.
          schedule(data.syncing ? 8000 : 60000);
        })
        .catch((e: Error) => {
          if (e.name !== "AbortError" && live) setError(e.message);
          schedule(60000);
        });

    const schedule = (delay: number) => {
      if (!live) return;
      window.clearTimeout(timer);
      timer = window.setTimeout(() => {
        if (!cancelScan.current) void load();
        else schedule(delay);
      }, delay);
    };

    setSelected(null);
    void load();

    return () => {
      live = false;
      ctrl.abort();
      window.clearTimeout(timer);
    };
  }, [variant, familyId]);

  const shown = useMemo(() => stores.filter((s) => visible.has(s.status)), [stores, visible]);

  const selected = useMemo(
    () => (selectedNumber ? (stores.find((s) => s.storeNumber === selectedNumber) ?? null) : null),
    [stores, selectedNumber],
  );

  const liveSummary = useMemo<Summary>(() => {
    if (!progress) return summary;
    // While a sweep streams, count what has landed so far.
    return stores.reduce<Summary>(
      (acc, s) => {
        acc.total++;
        if (s.status === "in_stock") acc.inStock++;
        else if (s.status === "short_wait") acc.shortWait++;
        else if (s.status === "long_wait") acc.longWait++;
        else acc.unavailable++;
        return acc;
      },
      { ...EMPTY_SUMMARY },
    );
  }, [progress, stores, summary]);

  const scanning = progress !== null;
  const pct = progress && progress.total ? Math.round((progress.done / progress.total) * 100) : 0;

  const onStoreSelect = useCallback((store: Store) => {
    setSelected(store);
    setFlyTo({ lng: store.lng, lat: store.lat, zoom: 8.5 });
  }, []);

  // ?perf turns on a frame-rate readout, so "it feels slow" can become a number.
  const showPerf = useMemo(() => new URLSearchParams(window.location.search).has("perf"), []);

  return (
    <div className="app">
      {showPerf && <PerfHud markers={shown.length} />}
      <MapView
        stores={shown}
        heatmap={heatmap}
        selected={selected}
        onSelect={setSelected}
        flyTo={flyTo}
        fitVersion={fitVersion}
        showLabels={showLabels}
      />

      <div className="panel">
        <header className="brand">
          <div className={`brand-mark${scanning ? " is-scanning" : ""}`} aria-hidden />
          <div>
            <h1>MacScout</h1>
            <p>Live Apple Store availability, mapped</p>
          </div>
        </header>

        <Controls
          families={families}
          familyId={familyId}
          onFamily={setFamilyId}
          catalog={catalog}
          loadingCatalog={loadingCatalog}
          selection={selection}
          onSelect={handleSelect}
          variant={variant}
          blocked={blocked}
        />

        {sync && (
          <SyncStatus
            syncedAt={sync.syncedAt}
            complete={sync.complete}
            covered={sync.covered}
            anchors={sync.anchors}
            syncing={sync.syncing || scanning}
            onRefresh={() => {
              setScanZip(null);
              if (variant) runScan(variant.part, null);
            }}
          />
        )}

        <div className="scan-row">
          <input
            className="zip-input"
            value={zip}
            onChange={(e) => setZip(e.target.value.replace(/[^\d]/g, "").slice(0, 5))}
            placeholder="ZIP for a quick check"
            inputMode="numeric"
            aria-label="Postal code to check now"
          />
          <button
            type="button"
            className="scan-button"
            disabled={!variant || scanning || zip.length !== 5}
            onClick={() => {
              setScanZip(zip);
              if (variant) runScan(variant.part, zip);
            }}
          >
            {scanning && scanZip ? "Checking…" : "Check ZIP now"}
          </button>
        </div>

        {scanning && (
          <div className="progress" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
            <div className="progress-bar" style={{ width: `${pct}%` }} />
            <span className="progress-text">
              {progress?.done ?? 0} / {progress?.total ?? 0} regions · {stores.length} stores found
            </span>
          </div>
        )}

        {resume && variant && (
          <ResumeNotice
            at={resume.at}
            remaining={resume.remaining}
            onCancel={() => {
              void stopScan(variant.part, scanZip);
              cancelScan.current?.();
              cancelScan.current = null;
              setResume(null);
            }}
          />
        )}

        {error && <p className="error">{error}</p>}

        <div className="results">
          <div className="results-head">
            <div className="toggles">
              <label className="toggle">
                <input type="checkbox" checked={heatmap} onChange={(e) => setHeatmap(e.target.checked)} />
                <span>Heat map</span>
              </label>
              <label className="toggle" title="Place names cost a second map layer to draw; turn them off if panning feels heavy">
                <input
                  type="checkbox"
                  checked={showLabels}
                  onChange={(e) => {
                    setShowLabels(e.target.checked);
                    try {
                      localStorage.setItem("macscout.labels", e.target.checked ? "on" : "off");
                    } catch {
                      // A browser refusing storage is not worth failing over.
                    }
                  }}
                />
                <span>Place names</span>
              </label>
            </div>
            <Legend
              summary={liveSummary}
              active={visible}
              onToggle={(status) =>
                setVisible((prev) => {
                  const next = new Set(prev);
                  if (next.has(status)) next.delete(status);
                  else next.add(status);
                  return next;
                })
              }
            />
          </div>

          <StoreList stores={shown} selected={selected} onSelect={onStoreSelect} />
        </div>

        <footer className="panel-foot">
          {rotation && rotation.configurations > 0 ? (
            <span title={rotation.throttled ? "apple.com is rate limiting; the rotation is waiting it out" : undefined}>
              {rotation.synced} / {rotation.configurations} configurations
              {rotation.throttled ? " · waiting on apple.com" : rotation.syncing ? " · syncing" : ""}
            </span>
          ) : (
            <span>{scanZip ? `Near ${scanZip}` : "Nationwide"}</span>
          )}
          <span className="foot-note">Kept fresh from apple.com</span>
        </footer>
      </div>

      {selected && <StorePanel store={selected} variant={variant} onClose={() => setSelected(null)} />}
    </div>
  );
}
