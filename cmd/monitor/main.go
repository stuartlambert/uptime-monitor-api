// Command monitor is the uptime-monitor daemon: it schedules periodic checks
// for each configured site, stores results in per-site SQLite databases, and
// serves a pull-only REST API for other systems to query.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stuart/uptime-monitor/internal/api"
	"github.com/stuart/uptime-monitor/internal/checker"
	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/scheduler"
	"github.com/stuart/uptime-monitor/internal/storage"
)

func main() {
	var (
		dataDir    = flag.String("data", "./data", "directory for registry.db and per-site databases")
		addr       = flag.String("addr", ":8080", "REST API listen address")
		apiKey     = flag.String("api-key", envOr("UPTIME_API_KEY", ""), "require this key via X-API-Key (empty = no auth)")
		reqTimeout = flag.Duration("request-timeout", 15*time.Second, "per-check HTTP request timeout")
		seedPath   = flag.String("seed", "", "optional JSON file of site configs to import on startup")
		blockPriv  = flag.Bool("block-private-targets", false, "refuse to check private/loopback/link-local addresses (SSRF guard); leave off to monitor internal hosts")
		corsOrigin = flag.String("cors-origins", "https://portal.pinkcrab.co.uk", "comma-separated browser origins allowed via CORS (empty = disabled)")
	)
	flag.Parse()

	if err := run(runOpts{
		dataDir:     *dataDir,
		addr:        *addr,
		apiKey:      *apiKey,
		reqTimeout:  *reqTimeout,
		seedPath:    *seedPath,
		blockPriv:   *blockPriv,
		corsOrigins: splitOrigins(*corsOrigin),
	}); err != nil {
		log.Fatal(err)
	}
}

type runOpts struct {
	dataDir     string
	addr        string
	apiKey      string
	reqTimeout  time.Duration
	seedPath    string
	blockPriv   bool
	corsOrigins []string
}

func run(o runOpts) error {
	dataDir := o.dataDir
	stores, err := storage.NewManager(dataDir)
	if err != nil {
		return err
	}
	defer stores.Close()

	reg, err := storage.OpenRegistry(filepath.Join(dataDir, "registry.db"))
	if err != nil {
		return err
	}
	defer reg.Close()

	if o.seedPath != "" {
		if err := seed(reg, stores, o.seedPath); err != nil {
			return err
		}
	}

	runner := checker.NewRunner(o.reqTimeout, o.blockPriv)
	sched := scheduler.NewManager(stores, runner)

	sites, err := reg.List()
	if err != nil {
		return err
	}
	sched.StartAll(sites)
	log.Printf("monitor: started, %d site(s) configured (block-private-targets=%v)", len(sites), o.blockPriv)

	srv := &http.Server{
		Addr:              o.addr,
		Handler:           api.NewServer(reg, stores, sched, o.apiKey, o.corsOrigins...).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Serve until an interrupt/terminate signal arrives.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("monitor: REST API listening on %s", o.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Print("monitor: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("monitor: http shutdown: %v", err)
	}
	sched.Shutdown()
	return nil
}

// seed imports site configs from a JSON file. Existing site ids are skipped so
// the seed is idempotent across restarts.
func seed(reg *storage.Registry, stores *storage.Manager, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var sites []config.SiteConfig
	if err := json.Unmarshal(raw, &sites); err != nil {
		return err
	}
	for _, s := range sites {
		s.ApplyDefaults()
		if err := s.Validate(); err != nil {
			log.Printf("seed: skipping invalid site %q: %v", s.ID, err)
			continue
		}
		exists, err := reg.Exists(s.ID)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		now := storage.Now()
		s.CreatedAt, s.UpdatedAt = now, now
		if err := reg.Create(s); err != nil {
			return err
		}
		if _, err := stores.Get(s.ID); err != nil {
			log.Printf("seed: open store for %s: %v", s.ID, err)
		}
		log.Printf("seed: imported site %s", s.ID)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitOrigins parses a comma-separated origin list into a trimmed slice,
// dropping empty entries (so "" yields nil and CORS stays disabled).
func splitOrigins(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}
