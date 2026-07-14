// Package config defines the site configuration model shared across the monitor.
//
// A SiteConfig is the source-of-truth record for one monitored website. It is
// stored in the registry database and drives what the scheduler and checkers do.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Defaults applied when a field is left zero on create.
const (
	DefaultIntervalSeconds     = 60    // per-minute checks
	DefaultSlowIntervalSeconds = 3600  // SSL/DNS cadence: hourly
	MinIntervalSeconds         = 5     // guard against hammering
	MaxIntervalSeconds         = 86400 // 1 day; also prevents duration overflow
	DefaultLatencyMaxMs        = 0     // 0 = no latency threshold
	DefaultSSLWarnDays         = 14    // warn when cert expires within N days
)

// SiteConfig is one monitored website.
type SiteConfig struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	URL                 string       `json:"url"`
	IntervalSeconds     int          `json:"interval_seconds"`
	SlowIntervalSeconds int          `json:"slow_interval_seconds"`
	Enabled             bool         `json:"enabled"`
	Checks              ChecksConfig `json:"checks"`
	CreatedAt           int64        `json:"created_at"`
	UpdatedAt           int64        `json:"updated_at"`

	// APIKeyHash is the SHA-256 (hex) of this site's read key, or "" if the site
	// has no dedicated key. It is never serialized to clients (json:"-") and is
	// never accepted from client input; MarshalJSON exposes only has_api_key.
	APIKeyHash string `json:"-"`
}

// MarshalJSON emits the site config with a derived has_api_key flag and without
// ever leaking the key hash.
func (s SiteConfig) MarshalJSON() ([]byte, error) {
	type alias SiteConfig // avoid infinite recursion
	return json.Marshal(struct {
		alias
		HasAPIKey bool `json:"has_api_key"`
	}{alias(s), s.APIKeyHash != ""})
}

// HashAPIKey returns the hex SHA-256 of a key. Keys are high-entropy random
// tokens, so a fast hash is appropriate — there is no low-entropy secret to
// brute-force, and this keeps request-time verification cheap.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ChecksConfig toggles each check type on/off and carries per-check parameters.
type ChecksConfig struct {
	HTTP    HTTPCheck    `json:"http"`
	Latency LatencyCheck `json:"latency"`
	Content ContentCheck `json:"content"`
	SSL     SSLCheck     `json:"ssl"`
	DNS     DNSCheck     `json:"dns"`
}

// HTTPCheck verifies the site responds with an acceptable status code.
type HTTPCheck struct {
	Enabled      bool  `json:"enabled"`
	ExpectStatus []int `json:"expect_status"` // empty => any 2xx/3xx is OK
}

// LatencyCheck flags responses slower than MaxMs. Runs every tick (free: reuses
// the HTTP request timing).
type LatencyCheck struct {
	Enabled bool `json:"enabled"`
	MaxMs   int  `json:"max_ms"` // 0 => record latency but never fail on it
}

// ContentCheck scans the response body. "Error free" is expressed via
// MustNotContain (e.g. "Exception", "stack trace").
type ContentCheck struct {
	Enabled        bool     `json:"enabled"`
	MustContain    []string `json:"must_contain"`
	MustNotContain []string `json:"must_not_contain"`
}

// SSLCheck validates the TLS certificate. Runs on the slow cadence.
type SSLCheck struct {
	Enabled  bool `json:"enabled"`
	WarnDays int  `json:"warn_days"` // warn when the cert expires within N days
}

// DNSCheck verifies the hostname resolves. Runs on the slow cadence (the HTTP
// request already resolves DNS every tick; this is an explicit periodic probe).
type DNSCheck struct {
	Enabled bool `json:"enabled"`
}

// ApplyDefaults fills zero-valued fields with sensible defaults. It does not
// overwrite values the caller supplied.
func (s *SiteConfig) ApplyDefaults() {
	if s.IntervalSeconds == 0 {
		s.IntervalSeconds = DefaultIntervalSeconds
	}
	if s.SlowIntervalSeconds == 0 {
		s.SlowIntervalSeconds = DefaultSlowIntervalSeconds
	}
	if s.Checks.SSL.Enabled && s.Checks.SSL.WarnDays == 0 {
		s.Checks.SSL.WarnDays = DefaultSSLWarnDays
	}
	if s.ID == "" {
		s.ID = Slugify(s.Name)
		if s.ID == "" {
			s.ID = Slugify(s.URL)
		}
	}
}

// Validate returns an error if the config is not usable.
func (s *SiteConfig) Validate() error {
	if strings.TrimSpace(s.URL) == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("url must include a host")
	}
	if s.IntervalSeconds < MinIntervalSeconds {
		return fmt.Errorf("interval_seconds must be >= %d", MinIntervalSeconds)
	}
	if s.IntervalSeconds > MaxIntervalSeconds {
		return fmt.Errorf("interval_seconds must be <= %d", MaxIntervalSeconds)
	}
	if s.SlowIntervalSeconds < s.IntervalSeconds {
		return errors.New("slow_interval_seconds must be >= interval_seconds")
	}
	if s.SlowIntervalSeconds > MaxIntervalSeconds {
		return fmt.Errorf("slow_interval_seconds must be <= %d", MaxIntervalSeconds)
	}
	if s.Checks.Latency.MaxMs < 0 {
		return errors.New("latency.max_ms must be >= 0")
	}
	if s.Checks.SSL.WarnDays < 0 {
		return errors.New("ssl.warn_days must be >= 0")
	}
	if s.ID == "" {
		return errors.New("id could not be derived; supply id or name")
	}
	if !slugRe.MatchString(s.ID) {
		return errors.New("id must be a slug: lowercase letters, digits, hyphens")
	}
	return nil
}

// Host returns the hostname portion of the URL (used by the DNS/SSL checks).
func (s *SiteConfig) Host() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// IsHTTPS reports whether the URL uses TLS.
func (s *SiteConfig) IsHTTPS() bool {
	return strings.HasPrefix(strings.ToLower(s.URL), "https://")
}

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	nonSlugChar = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slugify converts a name into a filesystem- and URL-safe id.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlugChar.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}
