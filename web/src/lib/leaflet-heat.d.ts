// leaflet.heat ships no types. Importing leaflet first makes this file a
// module augmentation rather than an ambient redeclaration that would shadow
// leaflet's own types.
import "leaflet";

declare module "leaflet.heat";

declare module "leaflet" {
  interface HeatLayerOptions {
    minOpacity?: number;
    maxZoom?: number;
    max?: number;
    radius?: number;
    blur?: number;
    gradient?: Record<number, string>;
    pane?: string;
  }

  interface HeatLayer extends Layer {
    setLatLngs(points: [number, number, number][]): this;
    setOptions(options: HeatLayerOptions): this;
    redraw(): this;
  }

  function heatLayer(points: [number, number, number][], options?: HeatLayerOptions): HeatLayer;
}
