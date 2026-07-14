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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/scheduler"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// Server wires the HTTP handlers to the registry, per-site stores, and scheduler.
type Server struct {
	reg    *storage.Registry
	stores *storage.Manager
	sched  *scheduler.Manager
	apiKey string // if non-empty, required via X-API-Key
}

// NewServer constructs the API server. apiKey may be empty to disable auth.
func NewServer(reg *storage.Registry, stores *storage.Manager, sched *scheduler.Manager, apiKey string) *Server {
	return &Server{reg: reg, stores: stores, sched: sched, apiKey: apiKey}
}

// Handler builds the routed http.Handler.
//
// Authorization has two tiers:
//   - Admin: the global API key (-api-key). Required for all config CRUD and
//     grants read access to every site's data.
//   - Site read: a per-site key. Grants read access to that one site's data
//     endpoints only. The admin key also satisfies these.
//
// If no admin key is configured and a site has no key, that site's data
// endpoints are open (matches the "no -api-key = no auth" default).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness of the monitor itself — intentionally unauthenticated.
	mux.HandleFunc("GET /health", s.handleHealth)

	// Config CRUD — admin only.
	mux.HandleFunc("GET /sites", s.requireAdmin(s.handleListSites))
	mux.HandleFunc("POST /sites", s.requireAdmin(s.handleCreateSite))
	mux.HandleFunc("GET /sites/{id}", s.requireAdmin(s.handleGetSite))
	mux.HandleFunc("PUT /sites/{id}", s.requireAdmin(s.handleUpdateSite))
	mux.HandleFunc("DELETE /sites/{id}", s.requireAdmin(s.handleDeleteSite))

	// Data endpoints (pull-only) — this site's key or the admin key.
	mux.HandleFunc("GET /sites/{id}/status", s.requireSiteRead(s.handleStatus))
	mux.HandleFunc("GET /sites/{id}/uptime", s.requireSiteRead(s.handleUptime))
	mux.HandleFunc("GET /sites/{id}/metrics", s.requireSiteRead(s.handleMetrics))
	mux.HandleFunc("GET /sites/{id}/errors", s.requireSiteRead(s.handleErrors))
	mux.HandleFunc("GET /sites/{id}/incidents", s.requireSiteRead(s.handleIncidents))
	mux.HandleFunc("GET /sites/{id}/results", s.requireSiteRead(s.handleResults))

	return mux
}

// keyMatches is a constant-time comparison of the presented header against want.
func keyMatches(presented, want string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// adminOK reports whether the request carries the admin key.
func (s *Server) adminOK(r *http.Request) bool {
	return s.apiKey != "" && keyMatches(r.Header.Get("X-API-Key"), s.apiKey)
}

// requireAdmin gates admin-only routes. When no admin key is configured, auth
// is disabled (preserves the default single-node "just run it" behavior).
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" || s.adminOK(r) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "admin API key required")
	}
}

// requireSiteRead gates per-site data routes: the admin key, or the site's own
// key. Unknown sites 404 (the id is already the resource being addressed).
func (s *Server) requireSiteRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site, err := s.reg.Get(r.PathValue("id"))
		if s.handleRegistryErr(w, err) {
			return
		}
		presented := r.Header.Get("X-API-Key")
		switch {
		case s.apiKey == "" && site.APIKeyHash == "":
			// No auth configured for this site: open.
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
