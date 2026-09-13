import { useEffect, useState } from "react";

interface Props {
  /** ISO timestamp of the last sweep, or null if never swept. */
  syncedAt: string | null;
  complete: boolean;
  covered: number;
  anchors: number;
  syncing: boolean;
  onRefresh: () => void;
}

function ago(from: Date): string {
  const seconds = Math.max(0, Math.round((Date.now() - from.getTime()) / 1000));
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} hr ago`;
  return `${Math.round(hours / 24)} d ago`;
}

/**
 * The map is drawn from a locally stored dataset that a background rotation
 * keeps fresh, so this line is what tells you how much to trust it.
 */
export default function SyncStatus({ syncedAt, complete, covered, anchors, syncing, onRefresh }: Props) {
  const [, tick] = useState(0);

  // Re-render on a slow timer so "3 min ago" does not sit there going stale.
  useEffect(() => {
    const id = window.setInterval(() => tick((n) => n + 1), 20000);
    return () => window.clearInterval(id);
  }, []);

  const at = syncedAt ? new Date(syncedAt) : null;

  let label: string;
  if (syncing) label = at ? `Refreshing · synced ${ago(at)}` : "Syncing this configuration…";
  else if (!at) label = "Not synced yet · queued";
  else if (!complete) label = `Synced ${ago(at)} · ${covered} of ${anchors} regions`;
  else label = `Synced ${ago(at)}`;

  return (
    <div className={`sync-status${syncing ? " is-syncing" : ""}`}>
      <span className="sync-dot" />
      <span className="sync-label">{label}</span>
      <button type="button" onClick={onRefresh} disabled={syncing}>
        {at ? "Refresh" : "Sync now"}
      </button>
    </div>
  );
}
