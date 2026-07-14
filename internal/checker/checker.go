// Package checker runs the actual health checks against a site and translates
// the outcome into a storage.Tick.
//
// One tick performs a single HTTP GET and evaluates the HTTP-status, latency,
// content, and (on the slow cadence) SSL checks from that one response. The DNS
// check, when due, does an explicit hostname lookup. Doing one request per tick
// keeps load light and the stored data lean (one row per tick).
package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/stuart/uptime-monitor/internal/config"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// maxBodyBytes caps how much of the response body we read for content checks.
const maxBodyBytes = 1 << 20 // 1 MiB

// Runner executes checks. It is safe for concurrent use.
type Runner struct {
	client   *http.Client
	resolver *net.Resolver
}

// NewRunner builds a Runner with a bounded per-request timeout. When
// blockPrivate is true, the HTTP client refuses to connect to private,
// loopback, or link-local addresses — a defence against SSRF to internal
// services and the cloud metadata endpoint. Leave it false if you legitimately
// monitor internal hosts.
func NewRunner(requestTimeout time.Duration, blockPrivate bool) *Runner {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if blockPrivate {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			// Control runs after DNS resolution with the actual dial address,
			// so this also covers redirect targets and defeats DNS-rebinding
			// (TOCTOU) — we check the very IP the socket will connect to.
			Control: blockPrivateAddr,
		}
		transport.DialContext = dialer.DialContext
	}
	return &Runner{
		client: &http.Client{
			Timeout:   requestTimeout,
			Transport: transport,
			// Do not follow more than a sane number of redirects; a redirect
			// loop should surface as an error, not hang.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return nil
			},
		},
		resolver: net.DefaultResolver,
	}
}

// blockPrivateAddr rejects dials to non-public IP ranges (SSRF guard).
func blockPrivateAddr(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("cannot parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unexpected non-IP dial address %q", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("blocked connection to non-public address %s", ip)
	}
	return nil
}

// RunTick performs one check cycle. runSlow enables the slow-cadence checks
// (SSL, DNS) for this tick.
func (r *Runner) RunTick(ctx context.Context, site config.SiteConfig, runSlow bool) storage.Tick {
	t := storage.Tick{TS: storage.Now(), Up: true, SSLChecked: runSlow && site.Checks.SSL.Enabled}

	// DNS check (slow cadence): an explicit resolve of the hostname.
	if runSlow && site.Checks.DNS.Enabled {
		if host := site.Host(); host != "" {
			if _, err := r.resolver.LookupHost(ctx, host); err != nil {
				t.Up = false
				t.AddFail("dns", fmt.Sprintf("dns resolution failed: %v", err))
			}
		}
	}

	// Single HTTP request; all fast checks read from its result.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.URL, nil)
	if err != nil {
		t.Up = false
		t.AddFail("http", fmt.Sprintf("bad request: %v", err))
		return t
	}
	req.Header.Set("User-Agent", "uptime-monitor/1.0")
	// If the URL embeds basic-auth credentials, move them into the Authorization
	// header and strip them from the URL. Otherwise net/http keeps them on
	// req.URL, where any request error would echo the password into logs and the
	// stored error message.
	if req.URL.User != nil {
		user := req.URL.User.Username()
		pass, _ := req.URL.User.Password()
		req.SetBasicAuth(user, pass)
		req.URL.User = nil
	}

	start := time.Now()
	resp, err := r.client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	t.ResponseMs = elapsed

	if err != nil {
		// Transport-level failure: unreachable, timeout, TLS handshake, etc.
		t.Up = false
		t.AddFail("http", fmt.Sprintf("request failed: %v", err))
		return t
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	t.StatusCode = resp.StatusCode

	// HTTP status check.
	if site.Checks.HTTP.Enabled && !statusAcceptable(resp.StatusCode, site.Checks.HTTP.ExpectStatus) {
		t.Up = false
		t.AddFail("http", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}

	// Latency check.
	if site.Checks.Latency.Enabled && site.Checks.Latency.MaxMs > 0 &&
		elapsed > int64(site.Checks.Latency.MaxMs) {
		t.Up = false
		t.AddFail("latency", fmt.Sprintf("response %dms exceeded %dms", elapsed, site.Checks.Latency.MaxMs))
	}

	// Content check.
	if site.Checks.Content.Enabled {
		if msg := evalContent(string(body), site.Checks.Content); msg != "" {
			t.Up = false
			t.AddFail("content", msg)
		}
	}

	// SSL check (slow cadence), evaluated from the TLS state of this response.
	if runSlow && site.Checks.SSL.Enabled {
		evalSSL(&t, resp.TLS, site.Checks.SSL)
	}

	return t
}

// statusAcceptable reports whether code is allowed. If expect is empty, any
// 2xx/3xx is considered OK.
func statusAcceptable(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 400
	}
	for _, e := range expect {
		if code == e {
			return true
		}
	}
	return false
}

func evalContent(body string, c config.ContentCheck) string {
	for _, want := range c.MustContain {
		if want != "" && !strings.Contains(body, want) {
			return fmt.Sprintf("body missing expected text %q", want)
		}
	}
	for _, bad := range c.MustNotContain {
		if bad != "" && strings.Contains(body, bad) {
			return fmt.Sprintf("body contained forbidden text %q", bad)
		}
	}
	return ""
}

// evalSSL inspects the TLS certificate. An expired cert marks the site down; a
// cert nearing expiry is recorded as a warning only.
func evalSSL(t *storage.Tick, cs *tls.ConnectionState, c config.SSLCheck) {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		// SSL check enabled but the request wasn't over TLS (or no cert).
		t.Errors = append(t.Errors, storage.CheckError{
			CheckType: "ssl", Message: "no TLS certificate presented",
		})
		return
	}
	leaf := cs.PeerCertificates[0]
	t.SSLExpiresAt = leaf.NotAfter.Unix()

	now := time.Now()
	if now.After(leaf.NotAfter) {
		t.Up = false
		t.AddFail("ssl", "certificate expired")
		return
	}
	if c.WarnDays > 0 {
		warnBefore := leaf.NotAfter.Add(-time.Duration(c.WarnDays) * 24 * time.Hour)
		if now.After(warnBefore) {
			days := int(time.Until(leaf.NotAfter).Hours() / 24)
			t.Errors = append(t.Errors, storage.CheckError{
				CheckType: "ssl",
				Message:   fmt.Sprintf("certificate expires in %d days", days),
			})
		}
	}
}
