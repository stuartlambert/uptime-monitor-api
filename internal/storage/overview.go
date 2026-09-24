package storage

import (
	"log"

	"github.com/stuart/uptime-monitor/internal/config"
)

// SiteOverview is one row of the dashboard: a site's configuration alongside its
// current state and a recent uptime figure.
//
// SiteConfig is a named field rather than an embedded one on purpose. It defines
// MarshalJSON, and embedding would promote that method to this struct — so the
// whole overview would serialise as just the site config, silently dropping
// every field below.
type SiteOverview struct {
	Site              config.SiteConfig `json:"site"`
	Up                *bool             `json:"up"`
	LastCheckTS       *int64            `json:"last_check_ts"`
	OngoingIncidentID *int64            `json:"ongoing_incident_id"`
	SSLExpiresAt      *int64            `json:"ssl_expires_at"`
	Window            string            `json:"window"`
	Checks            int64             `json:"checks"`
	Failed            int64             `json:"failed"`
	UptimePercent     float64           `json:"uptime_percent"`
	AvgMs             float64           `json:"avg_ms"`

	// Error is set when this site's data could not be read. The rest of the
	// dashboard still renders; only this row is degraded.
	Error string `json:"error,omitempty"`
}

// Overview returns one row per configured site.
//
// The dashboard needs config, live state, and an uptime figure together. Without
// this a ten-site dashboard is twenty-one requests (list, then status and uptime
// per site), which is both slow and racy — the rows would come from different
// instants.
//
// A site whose database cannot be opened or read yields a row carrying Error
// rather than failing the whole request: one corrupt file must not blank the
// dashboard for every other site.
func Overview(reg *Registry, stores *Manager, window string, sinceTS int64) ([]SiteOverview, error) {
	sites, err := reg.List()
	if err != nil {
		return nil, err
	}

	out := make([]SiteOverview, 0, len(sites))
	for _, site := range sites {
		row := SiteOverview{Site: site, Window: window}

		store, err := stores.Get(site.ID)
		if err != nil {
			log.Printf("overview: open store for %s: %v", site.ID, err)
			row.Error = "site data unavailable"
			out = append(out, row)
			continue
		}

		st, err := store.Status()
		if err != nil {
			log.Printf("overview: status for %s: %v", site.ID, err)
			row.Error = "site status unavailable"
			out = append(out, row)
			continue
		}
		row.Up = st.Up
		row.LastCheckTS = st.LastCheckTS
		row.OngoingIncidentID = st.OngoingIncidentID
		row.SSLExpiresAt = st.SSLExpiresAt

		m, err := store.Metrics(window, sinceTS)
		if err != nil {
			log.Printf("overview: metrics for %s: %v", site.ID, err)
			row.Error = "site metrics unavailable"
			out = append(out, row)
			continue
		}
		row.Checks, row.Failed, row.AvgMs = m.Checks, m.Failed, m.AvgMs

		u, err := store.Uptime(window, sinceTS)
		if err != nil {
			log.Printf("overview: uptime for %s: %v", site.ID, err)
			row.Error = "site uptime unavailable"
			out = append(out, row)
			continue
		}
		row.UptimePercent = u.Percent

		out = append(out, row)
	}
	return out, nil
}
