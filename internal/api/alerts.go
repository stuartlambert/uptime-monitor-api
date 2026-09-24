package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/stuart/uptime-monitor/internal/alerts"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// pathID parses an {id} path value as an integer.
func pathID(r *http.Request) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return v, err == nil && v > 0
}

// --- channels ---

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	list, err := s.reg.ListAlertChannels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// channelInput is the create/update body. Enabled is a pointer so an omitted
// field keeps the stored value instead of silently disabling the channel.
type channelInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Target  string `json:"target"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) decodeChannel(w http.ResponseWriter, r *http.Request) (channelInput, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return channelInput{}, false
	}
	var in channelInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return channelInput{}, false
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Target = strings.TrimSpace(in.Target)
	in.Type = strings.TrimSpace(in.Type)
	if in.Type == "" {
		in.Type = "email"
	}
	if in.Type != "email" {
		writeError(w, http.StatusBadRequest, `type must be "email"`)
		return channelInput{}, false
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return channelInput{}, false
	}
	// Validate the address here rather than discovering it at send time, when
	// the failure would be a row in the delivery log nobody is watching.
	if _, err := mail.ParseAddress(in.Target); err != nil {
		writeError(w, http.StatusBadRequest, "target must be a valid email address")
		return channelInput{}, false
	}
	return in, true
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	in, ok := s.decodeChannel(w, r)
	if !ok {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ch, err := s.reg.CreateAlertChannel(storage.AlertChannel{
		Name: in.Name, Type: in.Type, Target: in.Target, Enabled: enabled,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	existing, err := s.reg.GetAlertChannel(id)
	if s.handleAlertErr(w, err) {
		return
	}
	in, ok := s.decodeChannel(w, r)
	if !ok {
		return
	}
	existing.Name, existing.Type, existing.Target = in.Name, in.Type, in.Target
	if in.Enabled != nil {
		existing.Enabled = *in.Enabled
	}
	if err := s.reg.UpdateAlertChannel(existing); s.handleAlertErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	if err := s.reg.DeleteAlertChannel(id); s.handleAlertErr(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel sends a message to a channel immediately, so configuration
// can be proved from the UI rather than by waiting for a real outage.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	ch, err := s.reg.GetAlertChannel(id)
	if s.handleAlertErr(w, err) {
		return
	}
	if s.sender == nil {
		writeError(w, http.StatusServiceUnavailable,
			"no mail transport configured; set UPTIME_SMTP_HOST and UPTIME_SMTP_FROM")
		return
	}
	// Synchronous on purpose: the caller is a human waiting to learn whether the
	// settings work, so the SMTP error is the useful part of the response.
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	body := "This is a test alert from uptime-monitor.\n\n" +
		"If you received this, alert delivery is configured correctly.\n"
	if err := s.sender.Send(ctx, ch.Target, "[TEST] uptime-monitor alert", body); err != nil {
		writeError(w, http.StatusBadGateway, "send failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "sent", "target": ch.Target})
}

// --- rules ---

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	list, err := s.reg.ListAlertRules()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type ruleInput struct {
	SiteID       *string `json:"site_id"`
	ChannelID    int64   `json:"channel_id"`
	Kind         string  `json:"kind"`
	Enabled      *bool   `json:"enabled"`
	ConfirmAfter *int    `json:"confirm_after"`
}

func (s *Server) decodeRule(w http.ResponseWriter, r *http.Request) (ruleInput, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return ruleInput{}, false
	}
	var in ruleInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return ruleInput{}, false
	}
	if !storage.ValidAlertKind(in.Kind) {
		writeError(w, http.StatusBadRequest,
			"kind must be one of site_down, site_recovered, ssl_expiring")
		return ruleInput{}, false
	}
	if _, err := s.reg.GetAlertChannel(in.ChannelID); err != nil {
		writeError(w, http.StatusBadRequest, "channel_id does not exist")
		return ruleInput{}, false
	}
	// A rule naming a site that does not exist would never fire; refuse it now
	// rather than leaving a rule that looks configured but is inert.
	if in.SiteID != nil && *in.SiteID != "" {
		exists, err := s.reg.Exists(*in.SiteID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return ruleInput{}, false
		}
		if !exists {
			writeError(w, http.StatusBadRequest, "site_id does not exist")
			return ruleInput{}, false
		}
	} else {
		in.SiteID = nil // "" and null both mean every site
	}
	if in.ConfirmAfter != nil && (*in.ConfirmAfter < 1 || *in.ConfirmAfter > 100) {
		writeError(w, http.StatusBadRequest, "confirm_after must be between 1 and 100")
		return ruleInput{}, false
	}
	return in, true
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	in, ok := s.decodeRule(w, r)
	if !ok {
		return
	}
	rule := storage.AlertRule{
		SiteID: in.SiteID, ChannelID: in.ChannelID, Kind: in.Kind,
		Enabled: true, ConfirmAfter: 2,
	}
	if in.Enabled != nil {
		rule.Enabled = *in.Enabled
	}
	if in.ConfirmAfter != nil {
		rule.ConfirmAfter = *in.ConfirmAfter
	}
	created, err := s.reg.CreateAlertRule(rule)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	existing, err := s.reg.GetAlertRule(id)
	if s.handleAlertErr(w, err) {
		return
	}
	in, ok := s.decodeRule(w, r)
	if !ok {
		return
	}
	existing.SiteID, existing.ChannelID, existing.Kind = in.SiteID, in.ChannelID, in.Kind
	if in.Enabled != nil {
		existing.Enabled = *in.Enabled
	}
	if in.ConfirmAfter != nil {
		existing.ConfirmAfter = *in.ConfirmAfter
	}
	if err := s.reg.UpdateAlertRule(existing); s.handleAlertErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	if err := s.reg.DeleteAlertRule(id); s.handleAlertErr(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- deliveries ---

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	list, err := s.reg.ListAlertDeliveries(r.URL.Query().Get("site_id"), limitParam(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleAlertErr maps storage errors to responses, returning true when handled.
func (s *Server) handleAlertErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, storage.ErrAlertNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// senderFor exposes the configured transport, used by the test-send endpoint.
var _ alerts.Sender = (*alerts.SMTPSender)(nil)

// contextWithTimeout bounds a synchronous outbound call, inheriting the
// request's cancellation so a disconnecting client does not leave it running.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
