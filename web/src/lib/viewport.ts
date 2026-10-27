import { useEffect, useState } from "react";

/** Matches the narrow-screen breakpoint in styles.css, where the panel becomes
 *  a bottom sheet. */
export const SHEET_QUERY = "(max-width: 900px)";

/** Roughly the collapsed sheet's height; results are framed clear of it. */
export const SHEET_PEEK = 176;

/** Roughly the store sheet's height; a chosen store is centred above it. */
export const STORE_SHEET = 300;

export function isSheetLayout(): boolean {
  return typeof window !== "undefined" && window.matchMedia(SHEET_QUERY).matches;
}

export function useSheetLayout(): boolean {
  const [sheet, setSheet] = useState(isSheetLayout);
  useEffect(() => {
    const mq = window.matchMedia(SHEET_QUERY);
    const update = () => setSheet(mq.matches);
    update();
    mq.addEventListener("change", update);
    return () => mq.removeEventListener("change", update);
  }, []);
  return sheet;
}
