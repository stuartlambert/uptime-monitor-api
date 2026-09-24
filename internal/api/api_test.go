package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stuart/uptime-monitor/internal/auth"

	"github.com/stuart/uptime-monitor/internal/checker"
	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/scheduler"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// testEnv wires a full API server over temp databases.
type testEnv struct {
	srv    *httptest.Server
	stores *storage.Manager
	reg    *storage.Registry
}

func newTestEnv(t *testing.T, apiKey string, corsOrigins ...string) *testEnv {
	t.Helper()
	return newTestEnvOpts(t, apiKey, true, corsOrigins...)
}

// newTestEnvNoLegacy builds an env with the pre-/api/ paths unmounted, i.e. the
// state after the migration completes.
func newTestEnvNoLegacy(t *testing.T, apiKey string) *testEnv {
	t.Helper()
	return newTestEnvOpts(t, apiKey, false)
}

func newTestEnvOpts(t *testing.T, apiKey string, legacy bool, corsOrigins ...string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	stores, err := storage.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := storage.OpenRegistry(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.NewManager(stores, checker.NewRunner(2*time.Second, false))
	// Secure cookies off: httptest serves plain HTTP, so a Secure cookie would
	// never be stored by the client and every session test would fail opaquely.
	srv := httptest.NewServer(NewServer(reg, stores, sched, apiKey, corsOrigins...).
		WithLegacyRoutes(legacy).WithSecureCookies(false).Handler())
	t.Cleanup(func() {
		srv.Close()
		sched.Shutdown()
		reg.Close()
		stores.Close()
	})
	return &testEnv{srv: srv, stores: stores, reg: reg}
}

// do issues a request and returns the status code and body.
func (e *testEnv) do(t *testing.T, method, path, key, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestHealthIsUnauthenticated(t *testing.T) {
	e := newTestEnv(t, "secret")
	if code, _ := e.do(t, "GET", "/health", "", ""); code != 200 {
		t.Errorf("GET /health = %d, want 200", code)
	}
}

// --- /api/ prefix (phase 1) ---

// TestAPIPrefixMirrorsLegacy checks the same handler answers on both mounts.
func TestAPIPrefixMirrorsLegacy(t *testing.T) {
	e := newTestEnv(t, "")
	for _, path := range []string{"/health", "/api/health"} {
		if code, _ := e.do(t, "GET", path, "", ""); code != 200 {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
	}
}

// TestAPIPrefixFullLifecycle exercises CRUD and a data endpoint through /api/,
// covering the path-parameter case: {id} must survive the prefix strip.
func TestAPIPrefixFullLifecycle(t *testing.T) {
	e := newTestEnv(t, "secret")

	if code, body := e.do(t, "POST", "/api/sites", "secret", disabledSite); code != 201 {
		t.Fatalf("create via /api: %d %s", code, body)
	}
	if code, _ := e.do(t, "GET", "/api/sites/s1", "secret", ""); code != 200 {
		t.Errorf("get via /api: want 200")
	}
	// A nested path param through StripPrefix — the case a wrong strip breaks.
	if code, _ := e.do(t, "GET", "/api/sites/s1/uptime", "secret", ""); code != 200 {
		t.Errorf("nested data endpoint via /api: want 200")
	}
	// Auth still applies under the prefix.
	if code, _ := e.do(t, "GET", "/api/sites", "", ""); code != 401 {
		t.Errorf("unauthenticated /api/sites: want 401")
	}
	if code, _ := e.do(t, "DELETE", "/api/sites/s1", "secret", ""); code != 204 {
		t.Errorf("delete via /api: want 204")
	}
}

// TestAPIPrefixWritesAreVisibleOnLegacy proves both mounts share one backend
// rather than being parallel copies.
func TestAPIPrefixWritesAreVisibleOnLegacy(t *testing.T) {
	e := newTestEnv(t, testKey)
	if code, body := e.do(t, "POST", "/api/sites", testKey, disabledSite); code != 201 {
		t.Fatalf("create via /api: %d %s", code, body)
	}
	if code, _ := e.do(t, "GET", "/sites/s1", testKey, ""); code != 200 {
		t.Errorf("site created via /api not visible on legacy path")
	}
}

// TestLegacyRoutesDisabled is the post-migration state: /api/ only.
func TestLegacyRoutesDisabled(t *testing.T) {
	e := newTestEnvNoLegacy(t, "")

	if code, _ := e.do(t, "GET", "/api/health", "", ""); code != 200 {
		t.Errorf("GET /api/health = want 200")
	}
	if code, _ := e.do(t, "GET", "/health", "", ""); code != 404 {
		t.Errorf("legacy /health should 404 once disabled")
	}
	if code, _ := e.do(t, "GET", "/sites", "", ""); code != 404 {
		t.Errorf("legacy /sites should 404 once disabled")
	}
}

// TestDeprecationWarningRateLimited: first hit on a path warns, the next does
// not, and a different path warns again.
func TestDeprecationWarningRateLimited(t *testing.T) {
	s := &Server{deprecationSeen: map[string]time.Time{}}
	if !s.shouldLogDeprecation("/sites") {
		t.Error("first hit should warn")
	}
	if s.shouldLogDeprecation("/sites") {
		t.Error("second hit within the window should stay quiet")
	}
	if !s.shouldLogDeprecation("/sites/s1") {
		t.Error("a different path should warn on its first hit")
	}
}

// TestCallerIPIgnoresHeadersByDefault pins the fail-safe default: with no
// trusted header configured, nothing a client sends can change the identity
// used for logging and rate limiting.
func TestCallerIPIgnoresHeadersByDefault(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/sites", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("CF-Connecting-IP", "5.6.7.8")
	if got := s.callerIP(r); got != "10.0.0.9:5555" {
		t.Errorf("callerIP = %q, want the connecting address", got)
	}
}

func TestCallerIPUsesConfiguredHeader(t *testing.T) {
	s := &Server{realIPHeader: "CF-Connecting-IP"}
	for _, tc := range []struct{ hdr, remote, want string }{
		{"", "10.0.0.9:5555", "10.0.0.9:5555"}, // header absent -> connecting address
		{"203.0.113.7", "127.0.0.1:1", "203.0.113.7"},
		{"  203.0.113.7  ", "127.0.0.1:1", "203.0.113.7"},
		{"203.0.113.7, 70.41.3.18", "127.0.0.1:1", "203.0.113.7"},
	} {
		r := httptest.NewRequest("GET", "/sites", nil)
		r.RemoteAddr = tc.remote
		if tc.hdr != "" {
			r.Header.Set("CF-Connecting-IP", tc.hdr)
		}
		if got := s.callerIP(r); got != tc.want {
			t.Errorf("callerIP(hdr=%q) = %q, want %q", tc.hdr, got, tc.want)
		}
	}
}

// TestCallerIPIgnoresUnconfiguredHeaders is the security property: configuring
// CF-Connecting-IP must not also make X-Forwarded-For authoritative, or a client
// could forge the value that keys the login rate limiter.
func TestCallerIPIgnoresUnconfiguredHeaders(t *testing.T) {
	s := &Server{realIPHeader: "CF-Connecting-IP"}
	r := httptest.NewRequest("GET", "/sites", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := s.callerIP(r); got != "10.0.0.9:5555" {
		t.Errorf("callerIP = %q — X-Forwarded-For must be ignored when not configured", got)
	}
}

// TestClientIPStripsPort: 1.2.3.4:5678 and 1.2.3.4:9012 must share one
// rate-limit bucket, or a client trivially evades the per-IP throttle.
func TestClientIPStripsPort(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct{ remote, want string }{
		{"10.0.0.9:5555", "10.0.0.9"},
		{"10.0.0.9:9999", "10.0.0.9"},
		{"[2001:db8::1]:443", "2001:db8::1"},
	} {
		r := httptest.NewRequest("GET", "/sites", nil)
		r.RemoteAddr = tc.remote
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("clientIP(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

func TestAuthEnforced(t *testing.T) {
	e := newTestEnv(t, "secret")
	if code, _ := e.do(t, "GET", "/sites", "", ""); code != 401 {
		t.Errorf("no key: got %d, want 401", code)
	}
	if code, _ := e.do(t, "GET", "/sites", "wrong", ""); code != 401 {
		t.Errorf("wrong key: got %d, want 401", code)
	}
	if code, _ := e.do(t, "GET", "/sites", "secret", ""); code != 200 {
		t.Errorf("right key: got %d, want 200", code)
	}
}

// testKey is the admin key for tests that just need to be authenticated.
// Auth now fails closed, so there is no longer an "unconfigured = open" mode
// for these to rely on.
const testKey = "test-admin-key"

// disabledSite avoids launching a live check loop during CRUD tests.
const disabledSite = `{"id":"s1","name":"S1","url":"https://example.com","interval_seconds":60,"enabled":false,"checks":{"http":{"enabled":true}}}`

func TestCreateGetListDelete(t *testing.T) {
	e := newTestEnv(t, testKey)

	code, body := e.do(t, "POST", "/sites", testKey, disabledSite)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}

	// Duplicate id -> 409.
	if code, _ := e.do(t, "POST", "/sites", testKey, disabledSite); code != 409 {
		t.Errorf("duplicate: got %d, want 409", code)
	}

	// Get.
	if code, _ := e.do(t, "GET", "/sites/s1", testKey, ""); code != 200 {
		t.Errorf("get: got %d", code)
	}

	// List has exactly one.
	code, body = e.do(t, "GET", "/sites", testKey, "")
	var list []map[string]any
	json.Unmarshal(body, &list)
	if code != 200 || len(list) != 1 {
		t.Errorf("list: code=%d n=%d", code, len(list))
	}

	// Unknown -> 404.
	if code, _ := e.do(t, "GET", "/sites/nope", testKey, ""); code != 404 {
		t.Errorf("unknown: got %d, want 404", code)
	}

	// Delete -> 204, then gone.
	if code, _ := e.do(t, "DELETE", "/sites/s1", testKey, ""); code != 204 {
		t.Errorf("delete: got %d, want 204", code)
	}
	if code, _ := e.do(t, "GET", "/sites/s1", testKey, ""); code != 404 {
		t.Errorf("get after delete: got %d, want 404", code)
	}
}

func TestCreateDefaultsEnabledTrue(t *testing.T) {
	e := newTestEnv(t, testKey)
	// enabled omitted -> should default true. Point at a local backend so the
	// launched check loop stays hermetic.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	payload := `{"id":"auto","name":"Auto","url":"` + backend.URL + `","interval_seconds":60,"checks":{"http":{"enabled":true}}}`
	code, body := e.do(t, "POST", "/sites", testKey, payload)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	if got["enabled"] != true {
		t.Errorf("enabled = %v, want true", got["enabled"])
	}
}

func TestCreateValidation(t *testing.T) {
	e := newTestEnv(t, testKey)
	bad := `{"name":"X","url":"https://x.com","interval_seconds":1,"enabled":false}`
	if code, _ := e.do(t, "POST", "/sites", testKey, bad); code != 400 {
		t.Errorf("invalid interval: got %d, want 400", code)
	}
	if code, _ := e.do(t, "POST", "/sites", testKey, `{not json`); code != 400 {
		t.Errorf("bad json: got %d, want 400", code)
	}
}

// seedUp records one successful check so data endpoints have something to read.
func (e *testEnv) seedUp(t *testing.T, id string) {
	t.Helper()
	store, err := e.stores.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordTick(storage.Tick{TS: 1, Up: true, StatusCode: 200, ResponseMs: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestPerSiteKeyGenerateAndScope(t *testing.T) {
	e := newTestEnv(t, "admin")

	// Create site "a" with a server-generated key (admin required for CRUD).
	code, body := e.do(t, "POST", "/sites", "admin",
		`{"id":"a","name":"A","url":"https://example.com","enabled":false,"interval_seconds":60,"generate_api_key":true}`)
	if code != 201 {
		t.Fatalf("create a: %d %s", code, body)
	}
	var created map[string]any
	json.Unmarshal(body, &created)
	aKey, _ := created["api_key"].(string)
	if aKey == "" {
		t.Fatal("expected a one-time api_key in create response")
	}
	if created["has_api_key"] != true {
		t.Errorf("has_api_key = %v, want true", created["has_api_key"])
	}
	e.seedUp(t, "a")

	// Create a second site "b" with its own generated key.
	code, body = e.do(t, "POST", "/sites", "admin",
		`{"id":"b","name":"B","url":"https://example.com","enabled":false,"interval_seconds":60,"generate_api_key":true}`)
	if code != 201 {
		t.Fatalf("create b: %d %s", code, body)
	}
	var createdB map[string]any
	json.Unmarshal(body, &createdB)
	bKey, _ := createdB["api_key"].(string)

	cases := []struct {
		name string
		key  string
		want int
	}{
		{"site key reads own data", aKey, 200},
		{"admin reads any site", "admin", 200},
		{"wrong key rejected", "nope", 401},
		{"missing key rejected", "", 401},
		{"other site's key rejected", bKey, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, _ := e.do(t, "GET", "/sites/a/uptime", c.key, ""); code != c.want {
				t.Errorf("got %d, want %d", code, c.want)
			}
		})
	}
}

func TestPerSiteKeyNeverLeaked(t *testing.T) {
	e := newTestEnv(t, "admin")
	e.do(t, "POST", "/sites", "admin",
		`{"id":"a","name":"A","url":"https://example.com","enabled":false,"interval_seconds":60,"api_key":"provided-secret-value"}`)

	// GET config and LIST must expose has_api_key but never the hash or key.
	for _, path := range []string{"/sites/a", "/sites"} {
		_, body := e.do(t, "GET", path, "admin", "")
		s := string(body)
		if strings.Contains(s, "api_key_hash") || strings.Contains(s, "provided-secret-value") ||
			strings.Contains(s, config.HashAPIKey("provided-secret-value")) {
			t.Errorf("%s leaked key material: %s", path, s)
		}
		if !strings.Contains(s, "has_api_key") {
			t.Errorf("%s missing has_api_key flag", path)
		}
	}

	// The provided key authorizes reads.
	e.seedUp(t, "a")
	if code, _ := e.do(t, "GET", "/sites/a/uptime", "provided-secret-value", ""); code != 200 {
		t.Errorf("provided key should read: got %d", code)
	}
}

func TestPerSiteKeyRotation(t *testing.T) {
	e := newTestEnv(t, "admin")
	code, body := e.do(t, "POST", "/sites", "admin",
		`{"id":"a","name":"A","url":"https://example.com","enabled":false,"interval_seconds":60,"generate_api_key":true}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	json.Unmarshal(body, &created)
	oldKey := created["api_key"].(string)
	e.seedUp(t, "a")

	// Rotate via PUT with generate_api_key.
	_, body = e.do(t, "PUT", "/sites/a", "admin",
		`{"name":"A","url":"https://example.com","enabled":false,"interval_seconds":60,"generate_api_key":true}`)
	var rotated map[string]any
	json.Unmarshal(body, &rotated)
	newKey := rotated["api_key"].(string)
	if newKey == "" || newKey == oldKey {
		t.Fatalf("expected a new key, old=%s new=%s", oldKey, newKey)
	}

	if code, _ := e.do(t, "GET", "/sites/a/uptime", oldKey, ""); code != 401 {
		t.Errorf("old key should be revoked: got %d", code)
	}
	if code, _ := e.do(t, "GET", "/sites/a/uptime", newKey, ""); code != 200 {
		t.Errorf("new key should work: got %d", code)
	}
}

func TestUpdatePreservesKeyWhenUntouched(t *testing.T) {
	e := newTestEnv(t, "admin")
	code, body := e.do(t, "POST", "/sites", "admin",
		`{"id":"a","name":"A","url":"https://example.com","enabled":false,"interval_seconds":60,"generate_api_key":true}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	json.Unmarshal(body, &created)
	key := created["api_key"].(string)
	e.seedUp(t, "a")

	// Update other fields without mentioning the key.
	e.do(t, "PUT", "/sites/a", "admin",
		`{"name":"A renamed","url":"https://example.com","enabled":false,"interval_seconds":120}`)

	if code, _ := e.do(t, "GET", "/sites/a/uptime", key, ""); code != 200 {
		t.Errorf("key should survive an unrelated update: got %d", code)
	}
}

func TestDataEndpoints(t *testing.T) {
	e := newTestEnv(t, testKey)
	if code, body := e.do(t, "POST", "/sites", testKey, disabledSite); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}

	// Seed deterministic data directly in the site store.
	store, err := e.stores.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	down := storage.Tick{TS: 1000, StatusCode: 500, ResponseMs: 20}
	down.AddFail("http", "boom")
	for _, tk := range []storage.Tick{
		{TS: 1001, Up: true, StatusCode: 200, ResponseMs: 100},
		{TS: 1002, Up: true, StatusCode: 200, ResponseMs: 200},
		down,
	} {
		if _, err := store.RecordTick(tk); err != nil {
			t.Fatal(err)
		}
	}

	// uptime: 2/3 up.
	code, body := e.do(t, "GET", "/sites/s1/uptime", testKey, "")
	var u storage.Uptime
	json.Unmarshal(body, &u)
	if code != 200 || u.Checks != 3 || u.Successful != 2 {
		t.Errorf("uptime: code=%d %+v", code, u)
	}

	// metrics present.
	if code, _ := e.do(t, "GET", "/sites/s1/metrics?window=all", testKey, ""); code != 200 {
		t.Errorf("metrics: %d", code)
	}

	// errors: one logged.
	code, body = e.do(t, "GET", "/sites/s1/errors", testKey, "")
	var errs []storage.ErrorRow
	json.Unmarshal(body, &errs)
	if code != 200 || len(errs) != 1 {
		t.Errorf("errors: code=%d n=%d", code, len(errs))
	}

	// incidents: one opened by the failure.
	code, body = e.do(t, "GET", "/sites/s1/incidents", testKey, "")
	var incs []storage.Incident
	json.Unmarshal(body, &incs)
	if code != 200 || len(incs) != 1 {
		t.Errorf("incidents: code=%d n=%d", code, len(incs))
	}

	// results: three rows.
	code, body = e.do(t, "GET", "/sites/s1/results", testKey, "")
	var res []storage.ResultRow
	json.Unmarshal(body, &res)
	if code != 200 || len(res) != 3 {
		t.Errorf("results: code=%d n=%d", code, len(res))
	}

	// status: currently down.
	code, body = e.do(t, "GET", "/sites/s1/status", testKey, "")
	var st storage.Status
	json.Unmarshal(body, &st)
	if code != 200 || st.Up == nil || *st.Up {
		t.Errorf("status: code=%d up=%v", code, st.Up)
	}
}

// doOrigin issues a request with an Origin header and returns the response so
// CORS headers can be inspected.
func (e *testEnv) doOrigin(t *testing.T, method, path, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestCORS(t *testing.T) {
	const allowed = "https://portal.pinkcrab.co.uk"
	e := newTestEnv(t, "", allowed)

	// Allowed origin on a normal GET: the origin is echoed back.
	resp := e.doOrigin(t, http.MethodGet, "/health", allowed)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != allowed {
		t.Errorf("allowed GET: Allow-Origin = %q, want %q", got, allowed)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("allowed GET: Vary = %q, want to contain Origin", got)
	}

	// Preflight for the allowed origin: 204 with the CORS headers, no routing to a handler.
	pre := e.doOrigin(t, http.MethodOptions, "/sites", allowed)
	if pre.StatusCode != http.StatusNoContent {
		t.Errorf("preflight: status = %d, want 204", pre.StatusCode)
	}
	if got := pre.Header.Get("Access-Control-Allow-Origin"); got != allowed {
		t.Errorf("preflight: Allow-Origin = %q, want %q", got, allowed)
	}
	if got := pre.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-API-Key") {
		t.Errorf("preflight: Allow-Headers = %q, want to contain X-API-Key", got)
	}

	// Disallowed origin: no CORS headers leak out.
	other := e.doOrigin(t, http.MethodGet, "/health", "https://evil.example.com")
	if got := other.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin: Allow-Origin = %q, want empty", got)
	}
}

func TestCORSDisabledByDefault(t *testing.T) {
	e := newTestEnv(t, "") // no origins configured
	resp := e.doOrigin(t, http.MethodGet, "/health", "https://portal.pinkcrab.co.uk")
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS off: Allow-Origin = %q, want empty", got)
	}
}

// --- admin login (phase 2) ---

const (
	testUser = "stuart"
	testPass = "correct-horse-battery-staple"
)

// addAdmin creates an account in the env's registry and returns an HTTP client
// with a cookie jar, so a login persists across requests like a browser's would.
func (e *testEnv) addAdmin(t *testing.T) *http.Client {
	t.Helper()
	hash, err := auth.HashPassword(testPass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.reg.CreateAdminUser(testUser, hash); err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// login posts credentials and returns the status code.
func (e *testEnv) login(t *testing.T, c *http.Client, user, pass string) int {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)
	resp, err := c.Post(e.srv.URL+"/api/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// getWith issues an authenticated GET using the client's cookie jar.
func (e *testEnv) getWith(t *testing.T, c *http.Client, path string) (int, []byte) {
	t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestLoginIssuesSessionAndGrantsAccess(t *testing.T) {
	e := newTestEnv(t, "")
	c := e.addAdmin(t)

	// No session yet: admin routes are refused.
	if code, _ := e.getWith(t, c, "/api/sites"); code != 401 {
		t.Errorf("before login: got %d, want 401", code)
	}
	if code := e.login(t, c, testUser, testPass); code != 200 {
		t.Fatalf("login: got %d, want 200", code)
	}
	// The cookie alone now authenticates — no API key involved.
	if code, _ := e.getWith(t, c, "/api/sites"); code != 200 {
		t.Errorf("after login: got %d, want 200", code)
	}
	code, body := e.getWith(t, c, "/api/auth/me")
	if code != 200 || !strings.Contains(string(body), testUser) {
		t.Errorf("me: %d %s", code, body)
	}
}

func TestLoginCookieAttributes(t *testing.T) {
	e := newTestEnv(t, "")
	e.addAdmin(t)

	body := fmt.Sprintf(`{"username":%q,"password":%q}`, testUser, testPass)
	resp, err := http.Post(e.srv.URL+"/api/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no session cookie issued")
	}
	if !got.HttpOnly {
		t.Error("session cookie must be HttpOnly so scripts cannot read it")
	}
	if got.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", got.SameSite)
	}
	if got.Path != "/" {
		t.Errorf("Path = %q, want /", got.Path)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	e := newTestEnv(t, "")
	c := e.addAdmin(t)

	if code := e.login(t, c, testUser, "wrong-password-entirely"); code != 401 {
		t.Errorf("wrong password: got %d, want 401", code)
	}
	if code := e.login(t, c, "no-such-user", testPass); code != 401 {
		t.Errorf("unknown user: got %d, want 401", code)
	}
	if code, _ := e.getWith(t, c, "/api/sites"); code != 401 {
		t.Error("failed logins must not grant access")
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newTestEnv(t, "")
	c := e.addAdmin(t)

	for i := 0; i < 5; i++ {
		if code := e.login(t, c, testUser, "wrong"); code != 401 {
			t.Fatalf("attempt %d: got %d, want 401", i, code)
		}
	}
	if code := e.login(t, c, testUser, "wrong"); code != 429 {
		t.Errorf("6th attempt: got %d, want 429", code)
	}
	// Throttling must hold even once the password is right, or it would be
	// trivially bypassed by an attacker who lands on the correct one.
	if code := e.login(t, c, testUser, testPass); code != 429 {
		t.Errorf("correct password while throttled: got %d, want 429", code)
	}
}

func TestLogoutRevokesServerSide(t *testing.T) {
	e := newTestEnv(t, "")
	c := e.addAdmin(t)
	if code := e.login(t, c, testUser, testPass); code != 200 {
		t.Fatal("login failed")
	}

	// Capture the cookie, then log out, then replay it: a client-side-only
	// logout would still accept this.
	u, _ := url.Parse(e.srv.URL)
	var stolen *http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == sessionCookie {
			stolen = ck
		}
	}
	if stolen == nil {
		t.Fatal("no session cookie in jar")
	}

	resp, err := c.Post(e.srv.URL+"/api/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/sites", nil)
	req.AddCookie(stolen)
	replay, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Body.Close()
	if replay.StatusCode != 401 {
		t.Errorf("replayed cookie after logout: got %d, want 401", replay.StatusCode)
	}
}

func TestPasswordChangeRevokesAllSessions(t *testing.T) {
	e := newTestEnv(t, "")
	c1 := e.addAdmin(t)
	if code := e.login(t, c1, testUser, testPass); code != 200 {
		t.Fatal("login 1 failed")
	}
	// A second browser for the same account.
	jar, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar}
	if code := e.login(t, c2, testUser, testPass); code != 200 {
		t.Fatal("login 2 failed")
	}

	const newPass = "an-entirely-different-passphrase"
	body := fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, testPass, newPass)
	resp, err := c1.Post(e.srv.URL+"/api/auth/password", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("password change: got %d, want 200", resp.StatusCode)
	}

	// The other session must be dead too — that is the point of revocation.
	if code, _ := e.getWith(t, c2, "/api/sites"); code != 401 {
		t.Errorf("second session survived a password change: got %d, want 401", code)
	}
	// And the new password works.
	jar3, _ := cookiejar.New(nil)
	c3 := &http.Client{Jar: jar3}
	if code := e.login(t, c3, testUser, newPass); code != 200 {
		t.Errorf("login with new password: got %d, want 200", code)
	}
	if code := e.login(t, c3, testUser, testPass); code != 401 {
		t.Errorf("old password still works: got %d, want 401", code)
	}
}

func TestPasswordChangeRequiresCurrentPassword(t *testing.T) {
	e := newTestEnv(t, "")
	c := e.addAdmin(t)
	if code := e.login(t, c, testUser, testPass); code != 200 {
		t.Fatal("login failed")
	}
	body := `{"current_password":"wrong","new_password":"a-brand-new-passphrase"}`
	resp, err := c.Post(e.srv.URL+"/api/auth/password", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("got %d, want 401", resp.StatusCode)
	}
}

// TestAPIKeyCannotChangePassword: a leaked machine key must not be escalatable
// into control of the human account.
func TestAPIKeyCannotChangePassword(t *testing.T) {
	e := newTestEnv(t, testKey)
	e.addAdmin(t)

	body := fmt.Sprintf(`{"current_password":%q,"new_password":"a-brand-new-passphrase"}`, testPass)
	code, _ := e.do(t, "POST", "/api/auth/password", testKey, body)
	if code != 403 {
		t.Errorf("got %d, want 403", code)
	}
}

// TestSiteReadFailsClosedWithoutCredentials pins the behaviour change: a site
// with no per-site key used to be readable by anyone when no admin key was set.
func TestSiteReadFailsClosedWithoutCredentials(t *testing.T) {
	e := newTestEnv(t, testKey)
	if code, body := e.do(t, "POST", "/api/sites", testKey, disabledSite); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := e.do(t, "GET", "/api/sites/s1/uptime", "", ""); code != 401 {
		t.Errorf("anonymous site read: got %d, want 401", code)
	}
	if code, _ := e.do(t, "GET", "/api/sites/s1/uptime", testKey, ""); code != 200 {
		t.Errorf("admin site read: got %d, want 200", code)
	}
}

// --- dashboard endpoints (phase 4) ---

// TestOverviewRouteBeatsSiteIDPattern: /sites/overview must resolve to the
// rollup, not be read as a site whose id happens to be "overview".
func TestOverviewRouteBeatsSiteIDPattern(t *testing.T) {
	e := newTestEnv(t, testKey)
	if code, body := e.do(t, "POST", "/api/sites", testKey, disabledSite); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	code, body := e.do(t, "GET", "/api/sites/overview", testKey, "")
	if code != 200 {
		t.Fatalf("overview: %d %s", code, body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("overview did not return an array: %s", body)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if _, ok := rows[0]["site"]; !ok {
		t.Errorf("row is missing the site object: %s", body)
	}
}

func TestOverviewIsAdminOnly(t *testing.T) {
	e := newTestEnv(t, testKey)
	// A per-site key must not be able to enumerate the whole estate.
	code, body := e.do(t, "POST", "/api/sites", testKey,
		`{"id":"s1","name":"S1","url":"https://example.com","enabled":false,`+
			`"generate_api_key":true,"checks":{"http":{"enabled":true}}}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	json.Unmarshal(body, &created)
	siteKey, _ := created["api_key"].(string)
	if siteKey == "" {
		t.Fatal("no per-site key returned")
	}
	if code, _ := e.do(t, "GET", "/api/sites/overview", siteKey, ""); code != 401 {
		t.Errorf("per-site key on overview: got %d, want 401", code)
	}
	if code, _ := e.do(t, "GET", "/api/sites/overview", "", ""); code != 401 {
		t.Errorf("anonymous on overview: got %d, want 401", code)
	}
}

func TestSeriesEndpoint(t *testing.T) {
	e := newTestEnv(t, testKey)
	if code, body := e.do(t, "POST", "/api/sites", testKey, disabledSite); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	store, err := e.stores.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 30; i++ {
		if _, err := store.RecordTick(storage.Tick{
			TS: now - int64(30-i)*60, Up: true, StatusCode: 200, ResponseMs: 120,
		}); err != nil {
			t.Fatal(err)
		}
	}

	code, body := e.do(t, "GET", "/api/sites/s1/series?window=24h&buckets=12", testKey, "")
	if code != 200 {
		t.Fatalf("series: %d %s", code, body)
	}
	var got storage.Series
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 12 {
		t.Errorf("got %d points, want 12", len(got.Points))
	}
	if got.Window != "24h" {
		t.Errorf("window = %q", got.Window)
	}
	if got.BucketSeconds <= 0 {
		t.Errorf("bucket_seconds = %d", got.BucketSeconds)
	}
	var total int64
	for _, p := range got.Points {
		total += p.Checks
	}
	if total != 30 {
		t.Errorf("checks across buckets = %d, want 30", total)
	}
}

func TestSeriesRespectsPerSiteKey(t *testing.T) {
	e := newTestEnv(t, testKey)
	code, body := e.do(t, "POST", "/api/sites", testKey,
		`{"id":"s1","name":"S1","url":"https://example.com","enabled":false,`+
			`"generate_api_key":true,"checks":{"http":{"enabled":true}}}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	json.Unmarshal(body, &created)
	siteKey, _ := created["api_key"].(string)

	// The site's own key reads its own series...
	if code, _ := e.do(t, "GET", "/api/sites/s1/series", siteKey, ""); code != 200 {
		t.Errorf("site key on its own series: want 200")
	}
	// ...but nothing reads it anonymously.
	if code, _ := e.do(t, "GET", "/api/sites/s1/series", "", ""); code != 401 {
		t.Errorf("anonymous series read: want 401")
	}
}
