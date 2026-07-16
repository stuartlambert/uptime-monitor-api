package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	srv := httptest.NewServer(NewServer(reg, stores, sched, apiKey, corsOrigins...).Handler())
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

// disabledSite avoids launching a live check loop during CRUD tests.
const disabledSite = `{"id":"s1","name":"S1","url":"https://example.com","interval_seconds":60,"enabled":false,"checks":{"http":{"enabled":true}}}`

func TestCreateGetListDelete(t *testing.T) {
	e := newTestEnv(t, "")

	code, body := e.do(t, "POST", "/sites", "", disabledSite)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}

	// Duplicate id -> 409.
	if code, _ := e.do(t, "POST", "/sites", "", disabledSite); code != 409 {
		t.Errorf("duplicate: got %d, want 409", code)
	}

	// Get.
	if code, _ := e.do(t, "GET", "/sites/s1", "", ""); code != 200 {
		t.Errorf("get: got %d", code)
	}

	// List has exactly one.
	code, body = e.do(t, "GET", "/sites", "", "")
	var list []map[string]any
	json.Unmarshal(body, &list)
	if code != 200 || len(list) != 1 {
		t.Errorf("list: code=%d n=%d", code, len(list))
	}

	// Unknown -> 404.
	if code, _ := e.do(t, "GET", "/sites/nope", "", ""); code != 404 {
		t.Errorf("unknown: got %d, want 404", code)
	}

	// Delete -> 204, then gone.
	if code, _ := e.do(t, "DELETE", "/sites/s1", "", ""); code != 204 {
		t.Errorf("delete: got %d, want 204", code)
	}
	if code, _ := e.do(t, "GET", "/sites/s1", "", ""); code != 404 {
		t.Errorf("get after delete: got %d, want 404", code)
	}
}

func TestCreateDefaultsEnabledTrue(t *testing.T) {
	e := newTestEnv(t, "")
	// enabled omitted -> should default true. Point at a local backend so the
	// launched check loop stays hermetic.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	payload := `{"id":"auto","name":"Auto","url":"` + backend.URL + `","interval_seconds":60,"checks":{"http":{"enabled":true}}}`
	code, body := e.do(t, "POST", "/sites", "", payload)
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
	e := newTestEnv(t, "")
	bad := `{"name":"X","url":"https://x.com","interval_seconds":1,"enabled":false}`
	if code, _ := e.do(t, "POST", "/sites", "", bad); code != 400 {
		t.Errorf("invalid interval: got %d, want 400", code)
	}
	if code, _ := e.do(t, "POST", "/sites", "", `{not json`); code != 400 {
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
	if err := store.RecordTick(storage.Tick{TS: 1, Up: true, StatusCode: 200, ResponseMs: 10}); err != nil {
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
	e := newTestEnv(t, "")
	if code, body := e.do(t, "POST", "/sites", "", disabledSite); code != 201 {
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
		if err := store.RecordTick(tk); err != nil {
			t.Fatal(err)
		}
	}

	// uptime: 2/3 up.
	code, body := e.do(t, "GET", "/sites/s1/uptime", "", "")
	var u storage.Uptime
	json.Unmarshal(body, &u)
	if code != 200 || u.Checks != 3 || u.Successful != 2 {
		t.Errorf("uptime: code=%d %+v", code, u)
	}

	// metrics present.
	if code, _ := e.do(t, "GET", "/sites/s1/metrics?window=all", "", ""); code != 200 {
		t.Errorf("metrics: %d", code)
	}

	// errors: one logged.
	code, body = e.do(t, "GET", "/sites/s1/errors", "", "")
	var errs []storage.ErrorRow
	json.Unmarshal(body, &errs)
	if code != 200 || len(errs) != 1 {
		t.Errorf("errors: code=%d n=%d", code, len(errs))
	}

	// incidents: one opened by the failure.
	code, body = e.do(t, "GET", "/sites/s1/incidents", "", "")
	var incs []storage.Incident
	json.Unmarshal(body, &incs)
	if code != 200 || len(incs) != 1 {
		t.Errorf("incidents: code=%d n=%d", code, len(incs))
	}

	// results: three rows.
	code, body = e.do(t, "GET", "/sites/s1/results", "", "")
	var res []storage.ResultRow
	json.Unmarshal(body, &res)
	if code != 200 || len(res) != 3 {
		t.Errorf("results: code=%d n=%d", code, len(res))
	}

	// status: currently down.
	code, body = e.do(t, "GET", "/sites/s1/status", "", "")
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
