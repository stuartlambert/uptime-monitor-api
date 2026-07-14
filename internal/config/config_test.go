package config

import "testing"

func TestApplyDefaults(t *testing.T) {
	s := SiteConfig{Name: "My Cool Site", URL: "https://example.com"}
	s.Checks.SSL.Enabled = true
	s.ApplyDefaults()

	if s.IntervalSeconds != DefaultIntervalSeconds {
		t.Errorf("interval = %d, want %d", s.IntervalSeconds, DefaultIntervalSeconds)
	}
	if s.SlowIntervalSeconds != DefaultSlowIntervalSeconds {
		t.Errorf("slow interval = %d, want %d", s.SlowIntervalSeconds, DefaultSlowIntervalSeconds)
	}
	if s.Checks.SSL.WarnDays != DefaultSSLWarnDays {
		t.Errorf("ssl warn days = %d, want %d", s.Checks.SSL.WarnDays, DefaultSSLWarnDays)
	}
	if s.ID != "my-cool-site" {
		t.Errorf("id = %q, want my-cool-site", s.ID)
	}
}

func TestApplyDefaultsIDFromURLWhenNoName(t *testing.T) {
	s := SiteConfig{URL: "https://api.example.com/health"}
	s.ApplyDefaults()
	if s.ID == "" {
		t.Fatal("expected a derived id from URL")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     SiteConfig
		wantErr bool
	}{
		{"ok", SiteConfig{ID: "a", URL: "https://x.com", IntervalSeconds: 60, SlowIntervalSeconds: 3600}, false},
		{"no url", SiteConfig{ID: "a", IntervalSeconds: 60, SlowIntervalSeconds: 3600}, true},
		{"bad scheme", SiteConfig{ID: "a", URL: "ftp://x.com", IntervalSeconds: 60, SlowIntervalSeconds: 3600}, true},
		{"interval too small", SiteConfig{ID: "a", URL: "https://x.com", IntervalSeconds: 1, SlowIntervalSeconds: 3600}, true},
		{"slow < interval", SiteConfig{ID: "a", URL: "https://x.com", IntervalSeconds: 60, SlowIntervalSeconds: 30}, true},
		{"bad id slug", SiteConfig{ID: "Bad ID", URL: "https://x.com", IntervalSeconds: 60, SlowIntervalSeconds: 3600}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello World":     "hello-world",
		"  Trim  Me  ":    "trim-me",
		"api.example.com": "api-example-com",
		"UPPER_case-123":  "upper-case-123",
		"!!!":             "",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
