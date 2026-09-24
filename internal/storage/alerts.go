package storage

import (
	"database/sql"
	"errors"
	"time"
)

// Alert kinds. A latency breach is deliberately absent: the checker marks a
// slow response as a failed tick, so it already opens an incident and fires
// AlertSiteDown. A separate kind would double-notify for one event.
const (
	AlertSiteDown      = "site_down"
	AlertSiteRecovered = "site_recovered"
	AlertSSLExpiring   = "ssl_expiring"
)

// ValidAlertKind reports whether kind is one this build can deliver.
func ValidAlertKind(kind string) bool {
	switch kind {
	case AlertSiteDown, AlertSiteRecovered, AlertSSLExpiring:
		return true
	}
	return false
}

// Delivery statuses.
const (
	DeliveryPending = "pending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
)

// ErrAlertNotFound is returned when a channel or rule id does not exist.
var ErrAlertNotFound = errors.New("alert record not found")

// ErrDeliveryClaimed is returned by ClaimDelivery when this alert has already
// been claimed by someone else — the signal to skip sending, not an error.
var ErrDeliveryClaimed = errors.New("alert already claimed")

// AlertChannel is a destination for alerts.
type AlertChannel struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`   // currently only "email"
	Target    string `json:"target"` // email address
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// AlertRule binds a trigger to a channel. SiteID nil means every site, so one
// rule keeps covering sites added later.
type AlertRule struct {
	ID           int64   `json:"id"`
	SiteID       *string `json:"site_id"`
	ChannelID    int64   `json:"channel_id"`
	Kind         string  `json:"kind"`
	Enabled      bool    `json:"enabled"`
	ConfirmAfter int     `json:"confirm_after"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
}

// AlertDelivery is one attempted notification.
type AlertDelivery struct {
	ID         int64   `json:"id"`
	DedupeKey  string  `json:"dedupe_key"`
	SiteID     string  `json:"site_id"`
	Kind       string  `json:"kind"`
	ChannelID  int64   `json:"channel_id"`
	IncidentID *int64  `json:"incident_id"`
	Subject    string  `json:"subject"`
	Status     string  `json:"status"`
	Attempts   int     `json:"attempts"`
	Error      *string `json:"error"`
	CreatedAt  int64   `json:"created_at"`
	SentAt     *int64  `json:"sent_at"`
}

// --- channels ---

// ListAlertChannels returns every channel, oldest first.
func (r *Registry) ListAlertChannels() ([]AlertChannel, error) {
	rows, err := r.db.Query(`SELECT id, name, type, target, enabled, created_at, updated_at
		FROM alert_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertChannel{}
	for rows.Next() {
		var c AlertChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.Target, &c.Enabled,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetAlertChannel returns one channel.
func (r *Registry) GetAlertChannel(id int64) (AlertChannel, error) {
	var c AlertChannel
	err := r.db.QueryRow(`SELECT id, name, type, target, enabled, created_at, updated_at
		FROM alert_channels WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.Type, &c.Target, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrAlertNotFound
	}
	return c, err
}

// CreateAlertChannel inserts a channel and returns it with its id.
func (r *Registry) CreateAlertChannel(c AlertChannel) (AlertChannel, error) {
	now := time.Now().Unix()
	res, err := r.db.Exec(`INSERT INTO alert_channels
		(name, type, target, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, c.Target, c.Enabled, now, now)
	if err != nil {
		return c, err
	}
	c.ID, _ = res.LastInsertId()
	c.CreatedAt, c.UpdatedAt = now, now
	return c, nil
}

// UpdateAlertChannel replaces a channel's mutable fields.
func (r *Registry) UpdateAlertChannel(c AlertChannel) error {
	res, err := r.db.Exec(`UPDATE alert_channels
		SET name = ?, type = ?, target = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Type, c.Target, c.Enabled, time.Now().Unix(), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// DeleteAlertChannel removes a channel and, by cascade, its rules.
func (r *Registry) DeleteAlertChannel(id int64) error {
	res, err := r.db.Exec(`DELETE FROM alert_channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// --- rules ---

// ListAlertRules returns every rule, oldest first.
func (r *Registry) ListAlertRules() ([]AlertRule, error) {
	rows, err := r.db.Query(`SELECT id, site_id, channel_id, kind, enabled,
		confirm_after, created_at, updated_at FROM alert_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlertRules(rows)
}

// RulesFor returns the enabled rules of a kind that apply to siteID, including
// the site-agnostic ones (site_id IS NULL). Only rules whose channel is itself
// enabled are returned, so disabling a channel silences it without editing
// every rule that points at it.
func (r *Registry) RulesFor(siteID, kind string) ([]AlertRule, error) {
	rows, err := r.db.Query(`SELECT r.id, r.site_id, r.channel_id, r.kind, r.enabled,
		r.confirm_after, r.created_at, r.updated_at
		FROM alert_rules r
		JOIN alert_channels c ON c.id = r.channel_id
		WHERE r.kind = ? AND r.enabled = 1 AND c.enabled = 1
		  AND (r.site_id IS NULL OR r.site_id = ?)
		ORDER BY r.id`, kind, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlertRules(rows)
}

func scanAlertRules(rows *sql.Rows) ([]AlertRule, error) {
	out := []AlertRule{}
	for rows.Next() {
		var (
			r    AlertRule
			site sql.NullString
		)
		if err := rows.Scan(&r.ID, &site, &r.ChannelID, &r.Kind, &r.Enabled,
			&r.ConfirmAfter, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if site.Valid {
			v := site.String
			r.SiteID = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAlertRule returns one rule.
func (r *Registry) GetAlertRule(id int64) (AlertRule, error) {
	rows, err := r.db.Query(`SELECT id, site_id, channel_id, kind, enabled,
		confirm_after, created_at, updated_at FROM alert_rules WHERE id = ?`, id)
	if err != nil {
		return AlertRule{}, err
	}
	defer rows.Close()
	list, err := scanAlertRules(rows)
	if err != nil {
		return AlertRule{}, err
	}
	if len(list) == 0 {
		return AlertRule{}, ErrAlertNotFound
	}
	return list[0], nil
}

// CreateAlertRule inserts a rule and returns it with its id.
func (r *Registry) CreateAlertRule(a AlertRule) (AlertRule, error) {
	now := time.Now().Unix()
	res, err := r.db.Exec(`INSERT INTO alert_rules
		(site_id, channel_id, kind, enabled, confirm_after, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullableString(a.SiteID), a.ChannelID, a.Kind, a.Enabled, a.ConfirmAfter, now, now)
	if err != nil {
		return a, err
	}
	a.ID, _ = res.LastInsertId()
	a.CreatedAt, a.UpdatedAt = now, now
	return a, nil
}

// UpdateAlertRule replaces a rule's mutable fields.
func (r *Registry) UpdateAlertRule(a AlertRule) error {
	res, err := r.db.Exec(`UPDATE alert_rules SET site_id = ?, channel_id = ?, kind = ?,
		enabled = ?, confirm_after = ?, updated_at = ? WHERE id = ?`,
		nullableString(a.SiteID), a.ChannelID, a.Kind, a.Enabled, a.ConfirmAfter,
		time.Now().Unix(), a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// DeleteAlertRule removes a rule.
func (r *Registry) DeleteAlertRule(id int64) error {
	res, err := r.db.Exec(`DELETE FROM alert_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// --- deliveries ---

// ClaimDelivery reserves the right to send one alert, returning the new row id.
//
// The UNIQUE index on dedupe_key does the work: the insert either succeeds (this
// caller sends) or violates the constraint (someone already has, so skip). That
// makes duplicate suppression a property of the database rather than of process
// lifetime, which is what stops a restart mid-incident re-announcing an outage.
func (r *Registry) ClaimDelivery(d AlertDelivery) (int64, error) {
	res, err := r.db.Exec(`INSERT INTO alert_deliveries
		(dedupe_key, site_id, kind, channel_id, incident_id, subject, status, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		d.DedupeKey, d.SiteID, d.Kind, d.ChannelID, d.IncidentID, d.Subject,
		DeliveryPending, time.Now().Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDeliveryClaimed
		}
		return 0, err
	}
	return res.LastInsertId()
}

// MarkDeliverySent records a successful send.
func (r *Registry) MarkDeliverySent(id int64, attempts int) error {
	_, err := r.db.Exec(`UPDATE alert_deliveries
		SET status = ?, attempts = ?, sent_at = ?, error = NULL WHERE id = ?`,
		DeliverySent, attempts, time.Now().Unix(), id)
	return err
}

// MarkDeliveryFailed records a send that exhausted its retries. The row is kept
// (not deleted) so the dedupe key stays claimed — a permanently broken channel
// must not turn into a mail loop retrying every tick.
func (r *Registry) MarkDeliveryFailed(id int64, attempts int, cause string) error {
	_, err := r.db.Exec(`UPDATE alert_deliveries
		SET status = ?, attempts = ?, error = ? WHERE id = ?`,
		DeliveryFailed, attempts, cause, id)
	return err
}

// ListAlertDeliveries returns recent deliveries, newest first.
func (r *Registry) ListAlertDeliveries(siteID string, limit int) ([]AlertDelivery, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, dedupe_key, site_id, kind, channel_id, incident_id, subject,
		status, attempts, error, created_at, sent_at FROM alert_deliveries`
	args := []any{}
	if siteID != "" {
		q += ` WHERE site_id = ?`
		args = append(args, siteID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AlertDelivery{}
	for rows.Next() {
		var (
			d      AlertDelivery
			incID  sql.NullInt64
			errMsg sql.NullString
			sentAt sql.NullInt64
		)
		if err := rows.Scan(&d.ID, &d.DedupeKey, &d.SiteID, &d.Kind, &d.ChannelID,
			&incID, &d.Subject, &d.Status, &d.Attempts, &errMsg,
			&d.CreatedAt, &sentAt); err != nil {
			return nil, err
		}
		if incID.Valid {
			v := incID.Int64
			d.IncidentID = &v
		}
		if errMsg.Valid {
			v := errMsg.String
			d.Error = &v
		}
		if sentAt.Valid {
			v := sentAt.Int64
			d.SentAt = &v
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeliveryExists reports whether a dedupe key has already been claimed. Used to
// decide whether a recovery notice is warranted: announcing "back up" for an
// outage that was never announced is noise.
func (r *Registry) DeliveryExists(dedupeKey string) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM alert_deliveries WHERE dedupe_key = ?`,
		dedupeKey).Scan(&n)
	return n > 0, err
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
