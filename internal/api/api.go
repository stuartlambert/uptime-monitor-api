// Package api exposes the read/write HTTP interface: site config CRUD plus
// pull-only data endpoints for other servers to consume.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stuart/uptime-monitor/internal/alerts"
	"github.com/stuart/uptime-monitor/internal/auth"
	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/scheduler"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// Server wires the HTTP handlers to the registry, per-site stores, and scheduler.
type Server struct {
	reg         *storage.Registry
	stores      *storage.Manager
	sched       *scheduler.Manager
	apiKey      string   // if non-empty, required via X-API-Key
	corsOrigins []string // exact-match allowlist of browser origins; empty = CORS off

	// legacyRoutes keeps the pre-/api/ paths mounted alongside the new prefix
	// so existing consumers keep working during the migration.
	legacyRoutes bool

	// secureCookies sets the Secure attribute on the session cookie. Always on
	// in production; tests turn it off because httptest serves plain HTTP.
	secureCookies bool

	// realIPHeader names the header carrying the true client address, set by a
	// trusted reverse proxy. Empty (the default) means trust nothing and use
	// RemoteAddr. See callerIP for why the default is not X-Forwarded-For.
	realIPHeader string

	loginLimiter *auth.Limiter

	// sender is the alert transport, used by the test-send endpoint. Nil when no
	// mail server is configured, which that endpoint reports as 503.
	sender alerts.Sender

	// deprecationSeen rate-limits the legacy-path warning to one line per path
	// per deprecationLogEvery, so a consumer polling every minute cannot drown
	// the journal. Reading it is how you tell when the cutover is safe.
	deprecationMu   sync.Mutex
	deprecationSeen map[string]time.Time
}

// deprecationLogEvery is the per-path quiet period for legacy-route warnings.
const deprecationLogEvery = time.Hour

// NewServer constructs the API server. apiKey may be empty to disable auth.
// corsOrigins is an optional exact-match allowlist of browser Origins (e.g.
// "https://portal.pinkcrab.co.uk"); omit it to leave CORS disabled.
func NewServer(reg *storage.Registry, stores *storage.Manager, sched *scheduler.Manager, apiKey string, corsOrigins ...string) *Server {
	return &Server{
		reg: reg, stores: stores, sched: sched, apiKey: apiKey, corsOrigins: corsOrigins,
		// Default on: a server built without an explicit choice keeps answering
		// the paths consumers already use. main.go overrides from -legacy-routes.
		legacyRoutes:    true,
		deprecationSeen: map[string]time.Time{},
		secureCookies:   true,
		loginLimiter:    auth.NewLimiter(0, 0),
	}
}

// WithSecureCookies toggles the Secure attribute on the session cookie. Only
// tests, which run over plain HTTP, should turn it off.
func (s *Server) WithSecureCookies(on bool) *Server {
	s.secureCookies = on
	return s
}

// WithSender supplies the alert transport used by POST /alerts/channels/{id}/test.
func (s *Server) WithSender(sender alerts.Sender) *Server {
	s.sender = sender
	return s
}

// WithRealIPHeader names the header a trusted proxy uses to carry the client's
// address — "CF-Connecting-IP" behind Cloudflare. Leave unset unless a proxy in
// front is known to overwrite the header on every request; see callerIP.
func (s *Server) WithRealIPHeader(h string) *Server {
	s.realIPHeader = h
	return s
}

// WithLegacyRoutes controls whether the pre-/api/ paths stay mounted next to
// /api/. Turn it off once no consumer is hitting them (see the deprecation
// warnings in the log). Returns s so it can be chained onto NewServer.
func (s *Server) WithLegacyRoutes(on bool) *Server {
	s.legacyRoutes = on
	return s
}

// Handler builds the routed http.Handler.
//
// The API is mounted under /api/. When legacyRoutes is set it is also mounted
// bare, so consumers written against the original paths keep working until they
// migrate; those requests log a rate-limited deprecation warning naming the
// caller. Both mounts share one mux, so the two paths can never drift apart.
//
// Authorization has two tiers:
//   - Admin: the global API key (-api-key). Required for all config CRUD and
//     grants read access to every site's data.
//   - Site read: a per-site key. Grants read access to that one site's data
//     endpoints only. The admin key also satisfies these.
//
// Both tiers fail closed. A site with no per-site key is readable only by an
// admin; there is no configuration in which these endpoints answer anonymously.
func (s *Server) Handler() http.Handler {
	api := s.apiMux()

	root := http.NewServeMux()
	// StripPrefix takes "/api" without a trailing slash: the pattern below keeps
	// the slash, so /api/sites must arrive at the inner mux as /sites.
	root.Handle("/api/", http.StripPrefix("/api", api))
	if s.legacyRoutes {
		root.Handle("/", s.warnDeprecated(api))
	}
	return s.withCORS(root)
}

// apiMux routes the API itself, with no prefix. Handler mounts it.
func (s *Server) apiMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness of the monitor itself — intentionally unauthenticated.
	mux.HandleFunc("GET /health", s.handleHealth)

	// Session auth for the admin UI. login is necessarily unauthenticated and is
	// rate-limited instead; the rest require an existing session.
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/me", s.requireAdmin(s.handleMe))
	mux.HandleFunc("POST /auth/password", s.requireAdmin(s.handleChangePassword))

	// Config CRUD — admin only.
	mux.HandleFunc("GET /sites", s.requireAdmin(s.handleListSites))
	mux.HandleFunc("POST /sites", s.requireAdmin(s.handleCreateSite))
	mux.HandleFunc("GET /sites/{id}", s.requireAdmin(s.handleGetSite))
	mux.HandleFunc("PUT /sites/{id}", s.requireAdmin(s.handleUpdateSite))
	mux.HandleFunc("DELETE /sites/{id}", s.requireAdmin(s.handleDeleteSite))

	// Alerting configuration — admin only.
	mux.HandleFunc("GET /alerts/channels", s.requireAdmin(s.handleListChannels))
	mux.HandleFunc("POST /alerts/channels", s.requireAdmin(s.handleCreateChannel))
	mux.HandleFunc("PUT /alerts/channels/{id}", s.requireAdmin(s.handleUpdateChannel))
	mux.HandleFunc("DELETE /alerts/channels/{id}", s.requireAdmin(s.handleDeleteChannel))
	mux.HandleFunc("POST /alerts/channels/{id}/test", s.requireAdmin(s.handleTestChannel))

	mux.HandleFunc("GET /alerts/rules", s.requireAdmin(s.handleListRules))
	mux.HandleFunc("POST /alerts/rules", s.requireAdmin(s.handleCreateRule))
	mux.HandleFunc("PUT /alerts/rules/{id}", s.requireAdmin(s.handleUpdateRule))
	mux.HandleFunc("DELETE /alerts/rules/{id}", s.requireAdmin(s.handleDeleteRule))

	mux.HandleFunc("GET /alerts/deliveries", s.requireAdmin(s.handleListDeliveries))

	// Dashboard rollup — admin only, since it spans every site.
	mux.HandleFunc("GET /sites/overview", s.requireAdmin(s.handleOverview))

	// Data endpoints (pull-only) — this site's key or the admin key.
	mux.HandleFunc("GET /sites/{id}/series", s.requireSiteRead(s.handleSeries))
	mux.HandleFunc("GET /sites/{id}/status", s.requireSiteRead(s.handleStatus))
	mux.HandleFunc("GET /sites/{id}/uptime", s.requireSiteRead(s.handleUptime))
	mux.HandleFunc("GET /sites/{id}/metrics", s.requireSiteRead(s.handleMetrics))
	mux.HandleFunc("GET /sites/{id}/errors", s.requireSiteRead(s.handleErrors))
	mux.HandleFunc("GET /sites/{id}/incidents", s.requireSiteRead(s.handleIncidents))
	mux.HandleFunc("GET /sites/{id}/results", s.requireSiteRead(s.handleResults))

	return mux
}

// warnDeprecated logs requests arriving on the legacy (unprefixed) paths before
// passing them through unchanged. The warning names the caller so you can tell
// which consumer still needs migrating; it is rate-limited per path so a polling
// client leaves one line an hour rather than one per request.
func (s *Server) warnDeprecated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.shouldLogDeprecation(r.URL.Path) {
			log.Printf("api: DEPRECATED legacy path %s %s from %s — migrate to /api%s",
				r.Method, r.URL.Path, s.callerIP(r), r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// shouldLogDeprecation reports whether path is due another warning, recording
// the time when it is.
func (s *Server) shouldLogDeprecation(path string) bool {
	s.deprecationMu.Lock()
	defer s.deprecationMu.Unlock()
	now := time.Now()
	if last, ok := s.deprecationSeen[path]; ok && now.Sub(last) < deprecationLogEvery {
		return false
	}
	s.deprecationSeen[path] = now
	return true
}

// callerIP identifies the client, for deprecation warnings and as the
// rate-limiting key.
//
// Behind a reverse proxy every RemoteAddr is loopback, so the real address has
// to come from a header — but only one the proxy is known to set itself. That
// is what realIPHeader names, and it defaults to empty: with nothing
// configured this returns RemoteAddr and no header can influence it.
//
// The default is deliberately fail-safe. An earlier version always read the
// first X-Forwarded-For entry, which a client can forge: Cloudflare (like most
// proxies) *appends* to any inbound X-Forwarded-For rather than replacing it,
// so the leftmost value is whatever the caller sent. Since this value keys the
// login rate limiter, trusting it lets an attacker rotate fabricated addresses
// and walk past the per-IP throttle.
//
// Behind Cloudflare, set -real-ip-header=CF-Connecting-IP: Cloudflare
// overwrites that header on every proxied request, so a client cannot supply
// it. Note that this only holds for traffic that actually passes through
// Cloudflare — an origin reachable directly on 443 can still be sent a forged
// header, so restricting the origin to Cloudflare's ranges is the other half of
// the job.
func (s *Server) callerIP(r *http.Request) string {
	if s.realIPHeader != "" {
		if v := r.Header.Get(s.realIPHeader); v != "" {
			// Single-value headers such as CF-Connecting-IP carry one address.
			// A list-style header is split for tolerance, but see the warning
			// above before configuring one of those.
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return r.RemoteAddr
}

// clientIP is callerIP reduced to a bare address, dropping any port. Used as a
// rate-limiting key, where 1.2.3.4:5678 and 1.2.3.4:9012 must be one bucket.
func (s *Server) clientIP(r *http.Request) string {
	host := s.callerIP(r)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// withCORS wraps the mux, adding CORS response headers when the request carries
// an Origin on the allowlist. It also answers preflight OPTIONS requests (which
// the method-specific mux would otherwise 405) directly with 204.
//
// Origins are matched exactly and echoed back individually — never "*" — because
// the API authenticates with the X-API-Key header, and a fixed origin keeps that
// header usable from the browser. Requests with no/unlisted Origin pass through
// unchanged and simply receive no CORS headers.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin") // response varies per-origin; keep caches honest
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			// Preflight: the headers above are the whole response.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin is in the configured CORS allowlist.
func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.corsOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// keyMatches is a constant-time comparison of the presented header against want.
func keyMatches(presented, want string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// adminOK reports whether the request carries an admin credential: the API key
// (machine consumers) or a valid session cookie (the web UI).
func (s *Server) adminOK(r *http.Request) bool {
	if s.apiKey != "" && keyMatches(r.Header.Get("X-API-Key"), s.apiKey) {
		return true
	}
	return s.sessionOK(r)
}

// requireAdmin gates admin-only routes.
//
// This fails closed. Earlier versions treated "no -api-key configured" as "auth
// disabled" and served config CRUD, including DELETE, to anyone who could reach
// the port. That was survivable only while the service was loopback-only with no
// browser surface; it is not now. cmd/monitor refuses to start without a
// credential, so reaching here always means one is configured.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminOK(r) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "admin credentials required")
	}
}

// requireSiteRead gates per-site data routes: an admin credential, or the site's
// own read key. Unknown sites 404 (the id is already the resource being
// addressed). Like requireAdmin this fails closed — a site without its own key
// is not public, it is admin-only.
func (s *Server) requireSiteRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site, err := s.reg.Get(r.PathValue("id"))
		if s.handleRegistryErr(w, err) {
			return
		}
		presented := r.Header.Get("X-API-Key")
		switch {
		case s.adminOK(r):
			// Admin can read any site.
		case site.APIKeyHash != "" && keyMatches(config.HashAPIKey(presented), site.APIKeyHash):
			// Valid per-site key.
		default:
			writeError(w, http.StatusUnauthorized, "valid API key required for this site")
			return
		}
		next(w, r)
	}
}

// --- config CRUD ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": storage.Now()})
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	sites, err := s.reg.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sites == nil {
		sites = []config.SiteConfig{}
	}
	writeJSON(w, http.StatusOK, sites)
}

// maxBodyBytes bounds request bodies to protect against memory-exhaustion.
const maxBodyBytes = 1 << 20 // 1 MiB

// keyInput carries the optional per-site key controls in a create/update body.
type keyInput struct {
	APIKey      *string `json:"api_key"`          // set to a value, or "" to clear
	GenerateKey bool    `json:"generate_api_key"` // server generates a random key
}

// resolveSiteKey decides the site's api_key_hash from the request body and
// returns the hash to persist plus a one-time plaintext to echo back (only when
// the server generated the key). existingHash is preserved when the request
// doesn't mention the key at all.
func resolveSiteKey(body []byte, existingHash string) (hash, oncePlaintext string, err error) {
	var in keyInput
	if e := json.Unmarshal(body, &in); e != nil {
		return "", "", e
	}
	switch {
	case in.GenerateKey:
		key, e := randomKey()
		if e != nil {
			return "", "", e
		}
		return config.HashAPIKey(key), key, nil
	case in.APIKey != nil && *in.APIKey != "":
		return config.HashAPIKey(*in.APIKey), "", nil
	case in.APIKey != nil: // explicit empty string clears the key
		return "", "", nil
	default:
		return existingHash, "", nil
	}
}

// randomKey returns a 256-bit random token as hex.
func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeSite serializes a site, splicing in a one-time plaintext api_key when the
// key was just generated (it is never stored or returned again).
func writeSite(w http.ResponseWriter, status int, site config.SiteConfig, oneTimeKey string) {
	if oneTimeKey == "" {
		writeJSON(w, status, site)
		return
	}
	data, err := json.Marshal(site)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	obj["api_key"], _ = json.Marshal(oneTimeKey)
	writeJSON(w, status, obj)
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return
	}
	var site config.SiteConfig
	if err := json.Unmarshal(body, &site); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// A new site starts monitoring by default; only an explicit "enabled": false
	// disables it. (A plain bool can't distinguish omitted from false, so probe.)
	var probe struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Enabled == nil {
		site.Enabled = true
	}
	site.ApplyDefaults()
	if err := site.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Optional per-site read key: provided value, or server-generated.
	hash, oneTimeKey, err := resolveSiteKey(body, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	site.APIKeyHash = hash

	exists, err := s.reg.Exists(site.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists {
		writeError(w, http.StatusConflict, "site id already exists: "+site.ID)
		return
	}
	now := storage.Now()
	site.CreatedAt, site.UpdatedAt = now, now
	if err := s.reg.Create(site); err != nil {
		// Guard the Exists->Create gap: a concurrent create trips the primary
		// key and should read as a conflict, not a 500.
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "site id already exists: "+site.ID)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Materialise the per-site store now so monitoring_started_at is stamped.
	if _, err := s.stores.Get(site.ID); err != nil {
		log.Printf("api: could not open store for new site %s: %v", site.ID, err)
	}
	if site.Enabled {
		s.sched.Start(site)
	}
	writeSite(w, http.StatusCreated, site, oneTimeKey)
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	site, err := s.reg.Get(r.PathValue("id"))
	if s.handleRegistryErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) handleUpdateSite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.reg.Get(id)
	if s.handleRegistryErr(w, err) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return
	}
	var site config.SiteConfig
	if err := json.Unmarshal(body, &site); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// id and created_at are immutable; carry them from the existing record.
	site.ID = id
	site.CreatedAt = existing.CreatedAt
	site.ApplyDefaults()
	if err := site.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Key is preserved unless the body sets/clears/regenerates it.
	hash, oneTimeKey, err := resolveSiteKey(body, existing.APIKeyHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	site.APIKeyHash = hash

	site.UpdatedAt = storage.Now()
	if err := s.reg.Update(site); s.handleRegistryErr(w, err) {
		return
	}
	// Apply the new schedule/config immediately.
	s.sched.Restart(site)
	writeSite(w, http.StatusOK, site, oneTimeKey)
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	purge := r.URL.Query().Get("purge") == "true"

	if err := s.reg.Delete(id); s.handleRegistryErr(w, err) {
		return
	}
	s.sched.Stop(id)

	// By default the historical data file is preserved. ?purge=true removes it.
	if purge {
		if err := s.stores.Delete(id); err != nil {
			log.Printf("api: purge data for %s: %v", id, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- data endpoints ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	st, err := store.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleUptime(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	window, since := parseWindow(r)
	u, err := store.Uptime(window, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	window, since := parseWindow(r)
	m, err := store.Metrics(window, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleErrors(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	rows, err := store.Errors(sinceParam(r), limitParam(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	rows, err := store.Incidents(limitParam(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	rows, err := store.Results(sinceParam(r), limitParam(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// --- helpers ---

// storeFor resolves the site store for {id}, verifying the site is registered.
func (s *Server) storeFor(w http.ResponseWriter, r *http.Request) (*storage.SiteStore, bool) {
	id := r.PathValue("id")
	if _, err := s.reg.Get(id); s.handleRegistryErr(w, err) {
		return nil, false
	}
	store, err := s.stores.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return store, true
}

// handleRegistryErr writes the appropriate response for a registry error and
// reports whether the request should stop. Returns false when err is nil.
func (s *Server) handleRegistryErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "site not found")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// windowSeconds maps friendly window names to a duration in seconds. "all" (0)
// means since monitoring began.
var windowSeconds = map[string]int64{
	"1h":  3600,
	"24h": 86400,
	"7d":  604800,
	"30d": 2592000,
	"all": 0,
}

// parseWindow reads ?window= and returns the canonical name plus the cutoff ts.
func parseWindow(r *http.Request) (string, int64) {
	w := r.URL.Query().Get("window")
	if w == "" {
		w = "all"
	}
	secs, ok := windowSeconds[w]
	if !ok {
		w, secs = "all", 0
	}
	if secs == 0 {
		return w, 0
	}
	return w, time.Now().Unix() - secs
}

// sinceParam reads ?since= (unix seconds), defaulting to 0 (all time).
func sinceParam(r *http.Request) int64 {
	v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// limitParam reads ?limit=, clamped to [1, 10000] with the given default.
func limitParam(r *http.Request, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || v <= 0 {
		return def
	}
	if v > 10000 {
		return 10000
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleOverview returns one row per site: config, live state, and uptime over
// the requested window. Admin-only, because it spans every site — a per-site
// read key must not become a way to enumerate the estate.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	window, since := parseWindow(r)
	rows, err := storage.Overview(s.reg, s.stores, window, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleSeries returns bucketed uptime and latency for charting.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	store, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	window, since := parseWindow(r)
	buckets := storage.DefaultSeriesBuckets
	if v, err := strconv.Atoi(r.URL.Query().Get("buckets")); err == nil && v > 0 {
		buckets = v
	}
	series, err := store.Series(window, since, buckets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, series)
}
