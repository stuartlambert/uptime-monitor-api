package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"
)

func TestStatusAcceptable(t *testing.T) {
	cases := []struct {
		code   int
		expect []int
		want   bool
	}{
		{200, nil, true},
		{301, nil, true},
		{404, nil, false},
		{500, nil, false},
		{201, []int{200, 201}, true},
		{200, []int{204}, false},
	}
	for _, c := range cases {
		if got := statusAcceptable(c.code, c.expect); got != c.want {
			t.Errorf("statusAcceptable(%d, %v) = %v, want %v", c.code, c.expect, got, c.want)
		}
	}
}

func TestEvalContent(t *testing.T) {
	c := config.ContentCheck{
		Enabled:        true,
		MustContain:    []string{"welcome"},
		MustNotContain: []string{"Exception"},
	}
	if msg := evalContent("welcome home", c); msg != "" {
		t.Errorf("expected pass, got %q", msg)
	}
	if msg := evalContent("goodbye", c); msg == "" {
		t.Error("expected failure for missing required text")
	}
	if msg := evalContent("welcome, but Exception occurred", c); msg == "" {
		t.Error("expected failure for forbidden text")
	}
}

func newSite(url string) config.SiteConfig {
	s := config.SiteConfig{ID: "test", Name: "test", URL: url}
	s.Checks.HTTP.Enabled = true
	s.ApplyDefaults()
	return s
}

func TestRunTickUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("all good"))
	}))
	defer srv.Close()

	r := NewRunner(5*time.Second, false)
	tick := r.RunTick(context.Background(), newSite(srv.URL), false)

	if !tick.Up {
		t.Errorf("expected up, got down: %s", tick.Error)
	}
	if tick.StatusCode != 200 {
		t.Errorf("status = %d, want 200", tick.StatusCode)
	}
}

func TestRunTickDownOnStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	r := NewRunner(5*time.Second, false)
	tick := r.RunTick(context.Background(), newSite(srv.URL), false)

	if tick.Up {
		t.Error("expected down on 503")
	}
	if len(tick.FailedChecks) == 0 || tick.Error == "" {
		t.Error("expected a failed check and error message")
	}
}

func TestRunTickContentFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Fatal Exception: boom"))
	}))
	defer srv.Close()

	s := newSite(srv.URL)
	s.Checks.Content = config.ContentCheck{Enabled: true, MustNotContain: []string{"Exception"}}

	r := NewRunner(5*time.Second, false)
	tick := r.RunTick(context.Background(), s, false)
	if tick.Up {
		t.Error("expected down due to forbidden content")
	}
}

func TestRunTickUnreachable(t *testing.T) {
	r := NewRunner(1*time.Second, false)
	// Reserved TEST-NET-1 address that should not accept connections.
	s := newSite("http://192.0.2.1:9/")
	tick := r.RunTick(context.Background(), s, false)
	if tick.Up {
		t.Error("expected down for unreachable host")
	}
}

func TestBasicAuthNotLeakedInErrors(t *testing.T) {
	r := NewRunner(1*time.Second, false)
	// Unreachable host with embedded credentials; the failure message must not
	// contain the password.
	s := newSite("http://user:sup3rsecret@192.0.2.1:9/")
	tick := r.RunTick(context.Background(), s, false)
	if tick.Up {
		t.Fatal("expected down")
	}
	if strings.Contains(tick.Error, "sup3rsecret") {
		t.Errorf("password leaked into error: %q", tick.Error)
	}
}

func TestBasicAuthSent(t *testing.T) {
	var gotUser, gotPass string
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok = r.BasicAuth()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Inject credentials into the URL host portion.
	u := srv.URL[len("http://"):]
	s := newSite("http://alice:s3cret@" + u + "/")
	if tick := NewRunner(5*time.Second, false).RunTick(context.Background(), s, false); !tick.Up {
		t.Fatalf("expected up, got: %s", tick.Error)
	}
	if !ok || gotUser != "alice" || gotPass != "s3cret" {
		t.Errorf("basic auth not forwarded: user=%q pass=%q ok=%v", gotUser, gotPass, ok)
	}
}

func TestBlockPrivateTargets(t *testing.T) {
	// A loopback backend that would succeed if not for the SSRF guard.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Guard off: reaching 127.0.0.1 succeeds.
	if tick := NewRunner(5*time.Second, false).RunTick(context.Background(), newSite(srv.URL), false); !tick.Up {
		t.Errorf("guard off: expected up, got down: %s", tick.Error)
	}
	// Guard on: the dial to a loopback address is refused.
	tick := NewRunner(5*time.Second, true).RunTick(context.Background(), newSite(srv.URL), false)
	if tick.Up {
		t.Error("guard on: expected down (blocked private address)")
	}
	if !strings.Contains(tick.Error, "non-public") {
		t.Errorf("guard on: error = %q, want it to mention a blocked address", tick.Error)
	}
}
