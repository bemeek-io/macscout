// Command server hosts the MacScout API and the built React client.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/benmeeker/macscout/internal/api"
	"github.com/benmeeker/macscout/internal/apple"
	"github.com/benmeeker/macscout/internal/store"
	"github.com/benmeeker/macscout/internal/syncer"
)

// The client bundle is embedded so the whole app ships as one binary. The
// placeholder keeps `go build` working before the first `npm run build`.
//
//go:embed all:dist
var dist embed.FS

func main() {
	addr := flag.String("addr", envOr("MACSCOUT_ADDR", ":8080"), "listen address")
	concurrency := flag.Int("concurrency", 0, "max concurrent requests to apple.com (0 uses the measured-safe default)")
	origin := flag.String("apple-origin", "", "override the apple.com origin (for a local stub or another storefront)")
	cacheDir := flag.String("data-dir", "", "where to keep the local dataset (default: user cache dir)")
	background := flag.Bool("background-sync", true, "keep viewed configurations refreshed in the background")
	keepFresh := flag.Duration("keep-fresh", 30*time.Minute, "how stale a configuration may get before it is refreshed")
	betweenParts := flag.Duration("sync-gap", 8*time.Second, "pause between background sweeps")
	flag.Parse()

	var opts []apple.ClientOption
	if *origin != "" {
		log.Printf("using Apple origin %s", *origin)
		opts = append(opts, apple.WithBaseURL(*origin))
	}

	static, err := fs.Sub(dist, "dist")
	if err != nil {
		log.Fatalf("embed dist: %v", err)
	}
	if _, err := fs.Stat(static, "index.html"); err != nil {
		log.Printf("no client bundle embedded (run `npm run build` in web/); serving API only")
		static = nil
	}

	coverage := apple.NewCoverageIndex(dataPath(*cacheDir, "anchors.json"))

	db, err := store.Open(dataPath(*cacheDir, "macscout.db"))
	if err != nil {
		log.Fatalf("open dataset: %v", err)
	}
	defer db.Close()

	client := apple.NewClient(*concurrency, opts...)
	handler := api.NewServer(client, coverage, db, static)

	// The rotation refreshes whatever configurations people have looked at, so
	// the UI reads from the local dataset instead of waiting on apple.com.
	rotation := syncer.New(db, handler.SweepBatch, syncer.Options{
		KeepFresh:    *keepFresh,
		BetweenParts: *betweenParts,
	})
	rotation.WithCatalogs(handler.FamilyIDs, handler.FetchCatalog, handler.RefreshFamilies).
		WithProbe(handler.Probe)

	handler.AttachSyncer(rotation)

	syncCtx, stopSync := context.WithCancel(context.Background())
	defer stopSync()
	if *background {
		go rotation.Run(syncCtx)
	} else {
		log.Printf("background sync disabled")
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Sweeps stream for a while, so no blanket write timeout.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Printf("macscout listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// dataPath resolves a file in the data directory, which holds the local
// dataset and the learned anchor set so a restart does not start from nothing.
func dataPath(dir, name string) string {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return name
		}
		dir = filepath.Join(base, "macscout")
	}
	return filepath.Join(dir, name)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
