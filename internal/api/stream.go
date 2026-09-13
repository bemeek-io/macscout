package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/benmeeker/macscout/internal/apple"
)

// handleScanStream attaches to a sweep and streams its progress as Server-Sent
// Events. The sweep itself runs in the background, so reloading or closing the
// page does not lose the anchors already covered — reconnecting picks the
// stream back up where it is.
func (s *Server) handleScanStream(w http.ResponseWriter, r *http.Request) {
	part := strings.TrimSpace(r.URL.Query().Get("part"))
	if part == "" {
		writeErr(w, http.StatusBadRequest, "part is required")
		return
	}
	zips, scope, national, err := s.zipsFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	key := scope + "|" + part

	// A finished sweep replays from cache without touching apple.com.
	if cached, ok := s.scans.Get(key); ok {
		send("batch", apple.ScanEvent{
			Zip:    "cache",
			Stores: cached.Stores,
			Done:   len(zips),
			Total:  len(zips),
		})
		send("done", cached)
		return
	}

	defer s.holdSyncer()()

	runner := s.runnerFor(key, part, scope, zips)
	events := runner.watch()
	defer runner.unwatch(events)

	// Everything found so far, before subscribing to what comes next.
	snapshot, wait, finished := runner.snapshot()
	send("start", map[string]any{
		"part": part, "scope": scope,
		"total": len(zips), "covered": snapshot.Covered,
	})
	if len(snapshot.Stores) > 0 {
		send("batch", apple.ScanEvent{
			Zip: "resume", Stores: snapshot.Stores,
			Done: snapshot.Covered, Total: len(zips),
		})
	}
	if finished {
		s.cacheIfComplete(key, snapshot)
		send("done", snapshot)
		return
	}
	if wait > 0 {
		send("waiting", map[string]any{
			"retryAfter": int(wait.Seconds()),
			"remaining":  snapshot.Remaining,
		})
	}

	runner.start(s.client, s.coverage, national)

	// Keep the connection alive through the long waits between attempts;
	// without this, proxies and browsers drop an idle event stream.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case "batch":
				if !send("batch", ev.Batch) {
					return
				}
			case "waiting":
				if !send("waiting", map[string]any{
					"retryAfter": ev.RetryAfter,
					"remaining":  ev.Remaining,
				}) {
					return
				}
			case "done":
				s.cacheIfComplete(key, ev.Result)
				if ev.Error != "" {
					send("note", map[string]any{"message": ev.Error})
				}
				send("done", ev.Result)
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			// The sweep keeps running; only this watcher goes away.
			return
		}
	}
}

// cacheIfComplete stores only finished sweeps. Caching a partial one would pin
// an incomplete map in place and stop it ever being finished.
func (s *Server) cacheIfComplete(key string, res *ScanResult) {
	if res != nil && !res.Partial && len(res.Stores) > 0 {
		s.scans.Set(key, res)
	}
}
