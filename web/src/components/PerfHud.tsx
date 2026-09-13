import { useEffect, useRef, useState } from "react";

interface Sample {
  fps: number;
  worst: number;
}

/** describeRenderer reports what the browser is actually drawing with. */
function describeRenderer(): string {
  try {
    const canvas = document.createElement("canvas");
    const gl = (canvas.getContext("webgl2") ?? canvas.getContext("webgl")) as WebGLRenderingContext | null;
    if (!gl) return "no WebGL — software compositing";
    const info = gl.getExtension("WEBGL_debug_renderer_info");
    return info ? String(gl.getParameter(info.UNMASKED_RENDERER_WEBGL)) : "WebGL, renderer hidden";
  } catch {
    return "WebGL unavailable";
  }
}

/**
 * A frame-rate readout, shown with ?perf in the URL.
 *
 * It exists because "feels slow" and "is slow" are different claims, and the
 * numbers that would settle it — frame rate under real interaction — cannot be
 * gathered from outside the browser doing the drawing.
 */
export default function PerfHud({ markers }: { markers: number }) {
  const [sample, setSample] = useState<Sample>({ fps: 0, worst: 0 });
  const [tiles, setTiles] = useState(0);
  const renderer = useRef(describeRenderer());

  // Tile count is the other half of the picture: it is what the compositor is
  // actually pushing around, and it halves when place names are turned off.
  useEffect(() => {
    const id = window.setInterval(
      () => setTiles(document.querySelectorAll(".leaflet-tile").length),
      1000,
    );
    return () => window.clearInterval(id);
  }, []);

  useEffect(() => {
    let frames = 0;
    let worst = 0;
    let last = performance.now();
    let windowStart = last;
    let raf = 0;

    const tick = (now: number) => {
      const delta = now - last;
      last = now;
      frames++;
      if (delta > worst) worst = delta;

      if (now - windowStart >= 1000) {
        setSample({ fps: Math.round((frames * 1000) / (now - windowStart)), worst: Math.round(worst) });
        frames = 0;
        worst = 0;
        windowStart = now;
      }
      raf = requestAnimationFrame(tick);
    };

    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, []);

  const bad = sample.fps > 0 && sample.fps < 30;

  return (
    <div className={`perf-hud${bad ? " is-bad" : ""}`}>
      <b>{sample.fps} fps</b>
      <span>worst frame {sample.worst} ms</span>
      <span>
        {markers} markers · {tiles} tiles · dpr {window.devicePixelRatio}
      </span>
      <span className="perf-renderer">{renderer.current}</span>
    </div>
  );
}
