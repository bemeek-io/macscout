import { useEffect, useRef } from "react";
import L from "leaflet";
import "leaflet.heat";
import type { Status, Store } from "../lib/api";
import { STATUS_COLOR } from "../lib/status";

interface Props {
  stores: Store[];
  heatmap: boolean;
  selected: Store | null;
  onSelect: (store: Store | null) => void;
  flyTo: { lng: number; lat: number; zoom: number } | null;
  /** Bumped when a sweep finishes, to frame the finished result set once. */
  fitVersion: number;
  /** Place names are a second full-screen layer to composite; some machines
   *  are better off without them. */
  showLabels: boolean;
}

/**
 * Basemap is Esri's Dark Gray Canvas, which needs no API key and ships labels
 * as a separate overlay.
 *
 * Leaflet draws to a 2D canvas rather than WebGL. That is deliberate: WebGL is
 * unavailable whenever Chrome's GPU process is sandboxed off or hardware
 * acceleration is disabled, and a map that cannot initialise at all is worse
 * than one that pans a little less smoothly.
 */
const ESRI = "https://server.arcgisonline.com/ArcGIS/rest/services/Canvas";

/**
 * LowDpiCanvas draws the store markers at 1x instead of the device pixel ratio.
 *
 * Leaflet sizes canvas backing stores by devicePixelRatio, so on a retina
 * display the marker layer is a ~7 megapixel, 28 MB buffer that is reallocated
 * and cleared on every zoom. The markers are flat colour discs, so 1x costs
 * almost nothing visually and takes that to a quarter. Scoped to this renderer
 * rather than set globally, so tile handling is untouched.
 */
const LowDpiCanvas = L.Canvas.extend({
  _update(this: L.Canvas) {
    const browser = L.Browser as unknown as { retina: boolean };
    const wasRetina = browser.retina;
    browser.retina = false;
    try {
      (L.Canvas.prototype as unknown as { _update(): void })._update.call(this);
    } finally {
      browser.retina = wasRetina;
    }
  },
});

function lowDpiRenderer(pane: string): L.Canvas {
  return new (LowDpiCanvas as unknown as new (o: L.RendererOptions) => L.Canvas)({ pane });
}

// Each pane gets its own full-size canvas, so any pane stacked above the dots
// would swallow clicks meant for them. Only the dot pane takes pointer events.
//
// One heat pane per status. Leaflet's heat layer colours purely by density, so
// a single layer cannot say "green means in stock" — a dense cluster of
// long-wait stores would come out green simply for being dense. Splitting by
// status gives each its own colour, with density driving only intensity and
// spread within that colour.
const HEAT_ORDER: Status[] = ["unavailable", "long_wait", "short_wait", "in_stock"];

const PANES = {
  heat: { name: "macscout-heat", z: 405, interactive: false },
  dot: { name: "macscout-dot", z: 420, interactive: true },
  ring: { name: "macscout-ring", z: 430, interactive: false },
};

function heatPane(status: Status): string {
  return `macscout-heat-${status}`;
}

/** rgba builds a gradient stop from one of the status colours. */
function rgba(hex: string, alpha: number): string {
  const n = parseInt(hex.slice(1), 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

/** Dot radius in screen pixels, widening as you zoom in. */
function dotRadius(zoom: number): number {
  if (zoom <= 3) return 4;
  if (zoom >= 12) return 11;
  return 4 + ((zoom - 3) / 9) * 7;
}

/**
 * Each store is one canvas layer, not two. The halo that used to be a second,
 * larger circle is now a wide translucent stroke on the same marker — the same
 * look for half the per-frame drawing.
 */
function dotStyle(color: string, faded: boolean): L.CircleMarkerOptions {
  return {
    fillColor: color,
    fillOpacity: faded ? 0.5 : 0.95,
    color,
    weight: 5,
    opacity: faded ? 0.08 : 0.2,
  };
}

/** Matches .panel in styles.css; results are framed clear of it. */
const PANEL_WIDTH = 372;

/**
 * addLocateControl re-creates the "find me" button Leaflet does not ship,
 * centring the map on the browser's reported position.
 */
function addLocateControl(map: L.Map) {
  const control = new (L.Control.extend({
    options: { position: "bottomright" },
    onAdd() {
      const wrap = L.DomUtil.create("div", "leaflet-bar locate-control");
      const link = L.DomUtil.create("a", "", wrap);
      link.href = "#";
      link.title = "Centre on my location";
      link.setAttribute("role", "button");
      link.setAttribute("aria-label", "Centre the map on my location");
      link.textContent = "◎";

      L.DomEvent.on(link, "click", (e) => {
        L.DomEvent.stop(e);
        wrap.classList.add("is-busy");
        map.locate({ setView: true, maxZoom: 9 });
      });
      L.DomEvent.disableClickPropagation(wrap);

      const done = () => wrap.classList.remove("is-busy");
      map.on("locationfound", done);
      map.on("locationerror", done);
      return wrap;
    },
  }))();
  control.addTo(map);
}

export default function MapView({ stores, heatmap, selected, onSelect, flyTo, fitVersion, showLabels }: Props) {
  const container = useRef<HTMLDivElement>(null);
  const map = useRef<L.Map | null>(null);
  const marks = useRef(new Map<string, L.CircleMarker>());
  const radius = useRef(0);
  const ring = useRef<L.CircleMarker | null>(null);
  const heat = useRef<L.HeatLayer[]>([]);
  const labels = useRef<L.TileLayer | null>(null);
  const dotRenderer = useRef<L.Canvas | null>(null);
  const ringRenderer = useRef<L.Canvas | null>(null);
  const showLabelsRef = useRef(showLabels);

  showLabelsRef.current = showLabels;
  const storesRef = useRef<Store[]>(stores);
  const onSelectRef = useRef(onSelect);

  storesRef.current = stores;
  onSelectRef.current = onSelect;

  useEffect(() => {
    if (!container.current || map.current) return;

    const m = L.map(container.current, {
      center: [39.2, -96],
      zoom: 4,
      minZoom: 3,
      maxZoom: 16,
      zoomControl: false,
      attributionControl: false,
      // Canvas keeps a few hundred markers cheap and avoids one DOM node each.
      preferCanvas: true,
      worldCopyJump: false,
      // Animations stay on. Turning them off makes a zoom drop the old tiles
      // and jump straight to the new level, so you watch the map go black
      // between steps; the animation is what holds the previous tiles on
      // screen, scaled, until the replacements arrive. Measured on a machine
      // compositing in software: 120 fps with a 9 ms worst frame, so the cost
      // this was avoiding was not real.
    });
    map.current = m;

    // updateWhenIdle and a small buffer keep the tile layer from thrashing
    // requests and redraws through every frame of a pan.
    // The base layer is fully opaque so the compositor can blit it rather than
    // blend it — one less full-screen blended surface on every frame.
    // Tiles load while the map is moving, not only once it stops. Deferring
    // them saves work but leaves blank ground under the cursor mid-drag, which
    // reads as far more broken than a few extra requests.
    // updateWhenZooming:false keeps tile loading out of the animation's frames.
    // The old tiles stay on screen, scaled, for the length of the zoom and the
    // replacements arrive once it settles — so the transition stays continuous
    // without compositing two zoom levels' worth of images on every frame.
    // This is not updateWhenIdle, which would also strand panning.
    L.tileLayer(`${ESRI}/World_Dark_Gray_Base/MapServer/tile/{z}/{y}/{x}`, {
      maxZoom: 16,
      updateWhenZooming: false,
      attribution: "Esri, HERE, Garmin, © OpenStreetMap contributors",
    }).addTo(m);

    labels.current = L.tileLayer(`${ESRI}/World_Dark_Gray_Reference/MapServer/tile/{z}/{y}/{x}`, {
      maxZoom: 16,
      opacity: 0.55,
      updateWhenZooming: false,
    });
    if (showLabelsRef.current) labels.current.addTo(m);

    for (const pane of Object.values(PANES)) {
      const el = m.createPane(pane.name);
      el.style.zIndex = String(pane.z);
      if (!pane.interactive) el.style.pointerEvents = "none";
    }
    // Availability sits on top, so where statuses overlap the map answers the
    // question people are actually asking.
    dotRenderer.current = lowDpiRenderer(PANES.dot.name).addTo(m);

    HEAT_ORDER.forEach((status, i) => {
      const el = m.createPane(heatPane(status));
      el.style.zIndex = String(PANES.heat.z + i);
      el.style.pointerEvents = "none";
    });

    L.control.zoom({ position: "bottomright" }).addTo(m);
    L.control.attribution({ position: "bottomright", prefix: false }).addTo(m);
    addLocateControl(m);

    // Clicking bare map clears the selection; marker clicks stop propagation.
    m.on("click", () => onSelectRef.current(null));

    m.on("zoomend", () => {
      // Resizing every marker is the expensive part, so only do it when the
      // radius actually changes by a visible amount.
      const r = Math.round(dotRadius(m.getZoom()));
      if (r === radius.current) return;
      radius.current = r;
      for (const dot of marks.current.values()) {
        dot.setRadius(r);
      }
      ring.current?.setRadius(r + 6);
    });

    return () => {
      m.remove();
      map.current = null;
      marks.current.clear();
      ring.current = null;
      dotRenderer.current = null;
      ringRenderer.current = null;
      heat.current = [];
    };
  }, []);

  // Markers are reconciled by store number rather than torn down and rebuilt,
  // so a streaming sweep does not flicker as each batch lands.
  useEffect(() => {
    const m = map.current;
    if (!m) return;

    const r = Math.round(dotRadius(m.getZoom()));
    radius.current = r;
    const live = new Set<string>();

    for (const s of stores) {
      live.add(s.storeNumber);
      const style = dotStyle(STATUS_COLOR[s.status], s.status === "unavailable");
      const existing = marks.current.get(s.storeNumber);

      if (existing) {
        existing.setStyle(style);
        continue;
      }

      const dot = L.circleMarker([s.lat, s.lng], {
        pane: PANES.dot.name,
        renderer: dotRenderer.current ?? undefined,
        radius: r,
        ...style,
      }).addTo(m);

      dot.on("click", (e) => {
        L.DomEvent.stopPropagation(e);
        onSelectRef.current(s);
      });
      dot.on("mouseover", () => (m.getContainer().style.cursor = "pointer"));
      dot.on("mouseout", () => (m.getContainer().style.cursor = ""));

      marks.current.set(s.storeNumber, dot);
    }

    for (const [num, dot] of marks.current) {
      if (live.has(num)) continue;
      dot.remove();
      marks.current.delete(num);
    }
  }, [stores]);

  useEffect(() => {
    const m = map.current;
    if (!m) return;

    ring.current?.remove();
    ring.current = null;
    if (!selected) return;

    // Created on first use: an always-present ring canvas is another
    // full-viewport surface to clear on every move, for something that is
    // empty most of the time.
    if (!ringRenderer.current) {
      ringRenderer.current = lowDpiRenderer(PANES.ring.name).addTo(m);
    }

    ring.current = L.circleMarker([selected.lat, selected.lng], {
      pane: PANES.ring.name,
      renderer: ringRenderer.current ?? undefined,
      radius: dotRadius(m.getZoom()) + 6,
      color: "#eaf2ff",
      weight: 2,
      fill: false,
      interactive: false,
    }).addTo(m);
  }, [selected]);

  useEffect(() => {
    const m = map.current;
    if (!m) return;

    for (const layer of heat.current) layer.remove();
    heat.current = [];

    // The heat layer replaces the pins rather than sitting under them: hiding
    // the pane also takes several hundred canvas layers out of every frame.
    const pins = m.getPane(PANES.dot.name);
    const rings = m.getPane(PANES.ring.name);
    if (pins) pins.style.display = heatmap ? "none" : "";
    if (rings) rings.style.display = heatmap ? "none" : "";

    if (!heatmap || !stores.length) return;

    for (const status of HEAT_ORDER) {
      const points = stores
        .filter((s) => s.status === status)
        .map((s) => [s.lat, s.lng, 1] as [number, number, number]);
      if (!points.length) continue;

      const color = STATUS_COLOR[status];
      // Stores that cannot supply the product should barely register.
      const ceiling = status === "unavailable" ? 0.35 : 0.85;

      heat.current.push(
        L.heatLayer(points, {
          pane: heatPane(status),
          radius: 32,
          blur: 24,
          minOpacity: status === "unavailable" ? 0.06 : 0.18,
          // It takes several nearby stores of the same status to saturate, so
          // the brightness genuinely reads as density.
          max: 4,
          gradient: {
            0.2: rgba(color, ceiling * 0.35),
            0.55: rgba(color, ceiling * 0.7),
            1: rgba(color, ceiling),
          },
        }).addTo(m),
      );
    }
  }, [heatmap, stores]);

  useEffect(() => {
    const m = map.current;
    const layer = labels.current;
    if (!m || !layer) return;
    if (showLabels) layer.addTo(m);
    else layer.remove();
  }, [showLabels]);

  useEffect(() => {
    if (!flyTo) return;
    map.current?.flyTo([flyTo.lat, flyTo.lng], flyTo.zoom, { duration: 0.8 });
  }, [flyTo]);

  // Frame the finished sweep, not each streaming batch — an early batch is
  // one metro, and fitting to it would swing the camera across the country.
  useEffect(() => {
    const m = map.current;
    const found = storesRef.current;
    if (!m || !fitVersion || !found.length) return;

    m.fitBounds(L.latLngBounds(found.map((s) => [s.lat, s.lng] as [number, number])), {
      // Leave room for the panel so results are not framed underneath it.
      paddingTopLeft: [PANEL_WIDTH + 32, 48],
      paddingBottomRight: [48, 48],
      maxZoom: 9,
      animate: true,
    });
  }, [fitVersion]);

  return <div className="map" ref={container} />;
}
