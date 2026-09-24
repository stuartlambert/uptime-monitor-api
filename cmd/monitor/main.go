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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stuart/uptime-monitor/internal/alerts"
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
		legacyRts  = flag.Bool("legacy-routes", true, "also serve the API on the pre-/api/ paths (deprecated; set false once all consumers use /api/)")
		setPwUser  = flag.String("set-password", "", "set (or create) this admin user's password, prompting on the terminal, then exit")
		realIPHdr  = flag.String("real-ip-header", "", "header carrying the client's real address, set by a trusted proxy (behind Cloudflare: CF-Connecting-IP). Empty = trust none and use the connecting address")
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
		legacyRts:   *legacyRts,
		setPwUser:   *setPwUser,
		realIPHdr:   *realIPHdr,
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
	legacyRts   bool
	setPwUser   string
	realIPHdr   string
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

	// Administrative one-shot: set a password and exit without starting checks
	// or listening. Placed before any other setup so it works on a broken host.
	if o.setPwUser != "" {
		return setPassword(reg, o.setPwUser)
	}

	haveAdminUser, err := seedAdminUser(reg)
	if err != nil {
		return err
	}
	// Fail closed. Previously an instance with no -api-key served config CRUD,
	// including DELETE, to anyone who could reach the port. Refusing to start is
	// the only safe reading now that a browser-facing login exists: a silently
	// open admin API is worse than an outage, because nothing signals it.
	if o.apiKey == "" && !haveAdminUser {
		return errors.New("refusing to start: no admin credential configured — " +
			"set UPTIME_API_KEY, or seed an account with UPTIME_ADMIN_USER and " +
			"UPTIME_ADMIN_PASSWORD, or run with -set-password <username>")
	}
	if n, err := reg.PurgeExpiredSessions(); err != nil {
		log.Printf("auth: purge expired sessions: %v", err)
	} else if n > 0 {
		log.Printf("auth: purged %d expired session(s)", n)
	}

	if o.seedPath != "" {
		if err := seed(reg, stores, o.seedPath); err != nil {
			return err
		}
	}

	runner := checker.NewRunner(o.reqTimeout, o.blockPriv)
	sched := scheduler.NewManager(stores, runner)

	// Alerting. Without a mail server configured the dispatcher still runs and
	// still records deliveries, so the delivery log shows what would have been
	// sent — but the send itself reports the missing configuration.
	smtpCfg := smtpConfigFromEnv()
	var sender alerts.Sender
	if smtpCfg.Configured() {
		sender = alerts.NewSMTPSender(smtpCfg)
		log.Printf("alerts: smtp configured (%s, from %s)", smtpCfg.Addr(), smtpCfg.From)
	} else {
		log.Print("alerts: smtp not configured; alerts will be recorded but not delivered " +
			"(set UPTIME_SMTP_HOST and UPTIME_SMTP_FROM)")
		sender = alerts.NewSMTPSender(smtpCfg) // returns a clear error on send
	}
	dispatcher := alerts.NewDispatcher(reg, sender, alerts.Options{})
	defer dispatcher.Shutdown()

	// The hook runs on the site's check goroutine, so it only queues.
	sched.OnTransition(func(site config.SiteConfig, tr storage.Transition) {
		dispatcher.Enqueue(alerts.Event{Site: site, Transition: tr, At: time.Now()})
	})

	sites, err := reg.List()
	if err != nil {
		return err
	}
	sched.StartAll(sites)
	log.Printf("monitor: started, %d site(s) configured (block-private-targets=%v)", len(sites), o.blockPriv)

	srv := &http.Server{
		Addr: o.addr,
		Handler: api.NewServer(reg, stores, sched, o.apiKey, o.corsOrigins...).
			WithLegacyRoutes(o.legacyRts).WithSender(sender).
			WithRealIPHeader(o.realIPHdr).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Serve until an interrupt/terminate signal arrives.
	errCh := make(chan error, 1)
	go func() {
		realIP := o.realIPHdr
		if realIP == "" {
			realIP = "none (using connecting address)"
		}
		log.Printf("monitor: REST API listening on %s (prefix /api/, legacy-routes=%v, admin-user=%v, api-key=%v, real-ip-header=%s)",
			o.addr, o.legacyRts, haveAdminUser, o.apiKey != "", realIP)
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

// smtpConfigFromEnv reads mail settings from the environment.
//
// Environment rather than flags or the database: credentials on a command line
// are visible in `ps`, and in the database they would be readable through any
// future config-export endpoint. /etc/uptime-monitor.env is already root-only.
func smtpConfigFromEnv() alerts.SMTPConfig {
	port, err := strconv.Atoi(os.Getenv("UPTIME_SMTP_PORT"))
	if err != nil || port <= 0 {
		port = 587 // submission with STARTTLS
	}
	return alerts.SMTPConfig{
		Host:               strings.TrimSpace(os.Getenv("UPTIME_SMTP_HOST")),
		Port:               port,
		Username:           os.Getenv("UPTIME_SMTP_USER"),
		Password:           os.Getenv("UPTIME_SMTP_PASS"),
		From:               strings.TrimSpace(os.Getenv("UPTIME_SMTP_FROM")),
		InsecureSkipVerify: os.Getenv("UPTIME_SMTP_INSECURE") == "true",
	}
}
