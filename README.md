# MacScout

Look up any Apple product configuration and see, on one map, which Apple Stores
have it on the shelf, which will ship it in within a week, and which quote a
multi-week wait.

## What it does

- **Pick a configuration** — product line, size, colour, chip, storage. The
  options are read live from Apple's own buy pages, so the list is always the
  current lineup, and combinations Apple does not sell are greyed out.
- **Read the answer immediately** — the map is drawn from a local dataset that a
  background rotation keeps fresh, so clicking between configurations is
  instant. Every view says how old its data is; **Refresh** forces a sweep and
  **Check ZIP now** is a single request that answers in about a second.
- **Read the map** — each store is a dot coloured by wait. The heat layer
  colours by stock status too, with density driving only intensity and spread,
  so a dense cluster of back-ordered stores reads orange rather than "hot". Click a dot for the exact pickup quote, the ship-to-home date for
  that ZIP, and a link straight to that configuration on apple.com.
- **No WebGL required** — the map is Leaflet drawing to a 2D canvas, so it
  works with hardware acceleration off or Chrome's GPU process sandboxed away,
  where a WebGL map fails to initialise at all.
- **Or search one ZIP** — a single request returns the ~12 nearest stores,
  which is instant and much gentler on Apple.

## Running it

```sh
make build && ./macscout      # http://localhost:8080
```

The client bundle is embedded in the binary, so that is the whole deployment.
The dataset lives in the user cache directory by default; `-data-dir` moves it.

Useful flags:

| Flag | Default | What it does |
| --- | --- | --- |
| `-keep-fresh` | `30m` | how stale a configuration may get before it is refreshed |
| `-sync-gap` | `20s` | pause between background sweeps |
| `-background-sync` | `true` | set false to only sweep on demand |
| `-data-dir` | user cache dir | where the dataset and learned anchors live |

For development, run the API and the Vite dev server separately:

```sh
make run-api                  # :8080
cd web && npm run dev         # :5173, proxies /api to :8080
```

```sh
make test                     # go test ./... plus a TypeScript typecheck
```

## How it reads Apple

Everything comes from the same public endpoints apple.com's own buy pages call.
No key, no account, no scraping of rendered HTML beyond one embedded JSON blob.

| What | Endpoint |
| --- | --- |
| Configurations and part numbers | `/shop/buy-*/…` → the embedded `PRODUCT_SELECTION_BOOTSTRAP` blob |
| Store pickup availability | `/shop/retail/pickup-message?parts.0=…&location=…` |
| Ship-to-home estimate | `/shop/delivery-message?parts.0=…&location=…` |

Apple ships that configurator blob in three different shapes — Macs key
products by a dimension map, iPhone and iPad pages inline the dimensions on each
product, and Watch pages mix the two — so `internal/apple/catalog_parse.go`
handles each.

### Status colours

`pickupDisplay` plus the quoted date decide the bucket:

| Colour | Meaning | Source |
| --- | --- | --- |
| Green | In stock today | `available`, or a next-day ship-to-store |
| Amber | Short wait | ships to store within a week |
| Orange | Long wait | ships to store more than a week out |
| Grey | Unavailable | not offered for pickup at that store |

### Rate limiting

This matters more than it looks, and it shapes the whole design. Apple serves
short bursts happily but answers sustained parallel load with `503`/`541`, and
once you push through that it blocks the caller's IP for **10+ minutes**. The
budget is also cumulative, so a rate that survives one burst can still get you
blocked an hour later.

So the client:

- paces the whole process through one token bucket at ~2.5 req/s behind a
  concurrency gate of 2;
- trips a **circuit breaker** after four shed responses and abandons the rest
  of the sweep immediately. Without this, a queued sweep spends its retries on
  a server that is already refusing it, which deepens the block rather than
  riding it out. In practice this turns a 20-minute death spiral into a
  2-second stop;
- caches sweeps for 5 minutes and collapses concurrent requests for the same
  configuration into one, so a page refresh costs nothing.

A truncated sweep is reported honestly to the UI — and deliberately not cached
— rather than passed off as complete.

### Not tripping the limit in the first place

The safe request rate is not a constant. It depends on how much of apple.com's
budget this IP has already spent, which is invisible from outside — so the
client cannot be told the right rate, it has to find it.

Pacing is therefore an **AIMD controller**, the same shape as TCP congestion
control. It starts deliberately timid (~0.6 requests/second), shaves a little
off the interval after every request apple.com serves happily, and doubles the
interval the moment it starts shedding. Recovery is additive and backoff is
multiplicative, so giving up rate is cheap and regaining it is slow — the
controller settles just under the sustainable rate instead of repeatedly
discovering the limit by crossing it.

Anchor **order** matters as much as rate. Apple will sometimes cut a sweep short
regardless, so `SeedZips` is ordered by how many stores each anchor reaches:
the largest metros first, thinly served regions last. Thirty requests through a
density-ordered list is a fair picture of the country; thirty through a
geographically ordered one is a very detailed picture of California.

### Why there is a database

Sweeping at request time means every click waits on a rate-limited upstream —
minutes for a nationwide picture, and a wasted sweep whenever Apple says no.

So sweeping and reading are separated. A background rotation refreshes the
configurations people have actually looked at, one at a time, with a long pause
between them, and writes the results to a local SQLite file. Reads come from
that file and never touch apple.com, so they are instant and cannot be rate
limited. Each view reports its own age — "Synced 4 min ago" — and a refresh is
available when you want to pay for one.

The rotation turns on when each part was last **tried**, not last succeeded.
That distinction matters more than it sounds: ordering by last success let a
part apple.com would not answer for sort first forever, so it was retried — and
nothing else was — for hours, while its failures kept the address rate limited.
Parts that fail back off exponentially, and when nothing at all is getting
through the whole rotation slows down instead of hammering a closed door.

Two rules keep the dataset honest:

- **A sweep that reached nothing is never written.** An outage says nothing
  about availability, and recording it would overwrite good data with silence.
- **A part that has never been swept is not an empty one.** The UI says "Not
  synced yet" rather than drawing an empty map that implies the product is
  nowhere to be found.

The rotation stands down whenever someone is waiting on a foreground request,
so the person in front of the screen gets the request budget rather than
competing with the background for it.

### Sweeps that finish themselves

A sweep runs **on the server**, not in the browser. It works through the anchors,
pauses when apple.com starts shedding, waits out the cooldown, and carries on —
across as many rounds as it takes, backing off further each time a round covers
no new ground. The page just watches over SSE and shows a countdown.

That split matters: a sweep can take several minutes of mostly waiting, and a
tab that gets reloaded, backgrounded or closed must not throw away the anchors
already covered. Reconnecting re-attaches to the same sweep and replays what it
has. **Stop** ends it; nothing else needs to.

Partial results are never cached — that would pin an incomplete map in place and
stop the sweep ever finishing.

### Learning the anchor set

`SeedZips` is deliberately redundant: ~90 anchors whose 12-store neighbourhoods
overlap, so no store is missed. Most of that overlap is waste, but you cannot
know which anchors are redundant until you have asked.

So the first nationwide sweep records which stores each anchor reached, then
runs a greedy set cover over the result. Later sweeps query only the anchors
that survive — typically around a quarter as many — and the reduced set is
persisted to the user cache directory so a restart does not pay full price
again. It expires after a day, so stores that open later still get found.

A partial sweep is never used to recompute the cover: anchors that never
reported would look redundant when they are merely unanswered.

## Layout

```
cmd/server/           binary; embeds the built client
internal/apple/
  client.go          HTTP client: pacing, retries, circuit breaker
  catalog.go         buy-page scraping and catalog assembly
  catalog_parse.go   the three shapes of Apple's configurator blob
  pickup.go          store availability and delivery quotes
  scan.go            the sweep: worker pool, dedupe, early stop
  coverage.go        learns the minimal anchor set from a full sweep
  zips.go            the anchor grid
internal/api/         HTTP handlers, including the SSE scan stream
internal/store/       SQLite dataset: stores, availability, sync state
internal/syncer/      background rotation that keeps the dataset fresh
internal/cache/       TTL cache with request collapsing
web/                  React + Leaflet client
```

The nationwide sweep streams over Server-Sent Events, so stores appear on the
map as each anchor reports instead of after the whole sweep. The camera frames
the finished result set rather than each arriving batch, so it does not swing
across the country while the map fills in.

### On map performance

Two measurements, both worth keeping because they contradict the obvious guesses.

Leaflet's own work is negligible: 0.19 ms per pan, 2.31 ms per zoom with the
full store set on screen. And on a machine reporting *no WebGL — software
compositing*, the map still runs at **120 fps with a 9 ms worst frame**.

So neither JavaScript nor the absence of a GPU is a reason to strip the map
back, and an earlier version that disabled zoom animation to "help" made things
worse, not better: without the animation a zoom drops the old tiles and jumps,
so the map visibly goes black between levels. The animation is what holds the
previous tiles on screen, scaled, until their replacements load.

`?perf` in the URL shows a live frame-rate readout — fps, worst frame time,
marker count, and what the browser reports as its renderer. It exists because
"feels slow" and "is slow" are different claims, and only the browser doing the
drawing can settle it.

**Place names** can still be turned off to halve the tile layers, for genuinely
constrained machines.

The map deliberately avoids WebGL. An earlier MapLibre build threw
`Failed to initialize WebGL` outright on machines where Chrome's GPU process is
unavailable — no map at all, not a degraded one. Leaflet's 2D canvas has a
software path everywhere, and it took the client bundle from 1.27 MB to 363 KB
on the way.

## Limits

- US stores only; the anchor list and `LL/A` part numbers are US-specific.
- A nationwide sweep is the expensive path. Searching one ZIP is a single
  request and is the right tool when you only care about stores near you.
- Only preconfigured models are stocked in stores, so build-to-order
  configurations are intentionally absent.
- Apple quotes availability per part number, and quotes can change between a
  scan and a visit — always confirm on apple.com before travelling.
