import { useEffect, useState } from "react";

interface Props {
  /** Epoch milliseconds at which the sweep resumes on its own. */
  at: number;
  remaining: number;
  onCancel: () => void;
}

/**
 * A nationwide sweep pauses whenever apple.com starts shedding load, then
 * carries on by itself. The sweep runs on the server, so this is only a
 * progress report — closing the page does not stop it.
 */
export default function ResumeNotice({ at, remaining, onCancel }: Props) {
  const [left, setLeft] = useState(() => Math.max(0, Math.ceil((at - Date.now()) / 1000)));

  useEffect(() => {
    const tick = () => setLeft(Math.max(0, Math.ceil((at - Date.now()) / 1000)));
    tick();
    const id = window.setInterval(tick, 1000);
    return () => window.clearInterval(id);
  }, [at]);

  return (
    <div className="resume" role="status">
      <span className="resume-text">
        Waiting out apple.com's limit. {remaining} {remaining === 1 ? "region" : "regions"} left —
        continuing in <b>{left}s</b>
      </span>
      <span className="resume-actions">
        <button type="button" onClick={onCancel}>
          Stop
        </button>
      </span>
    </div>
  );
}
