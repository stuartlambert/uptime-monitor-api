package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/stuart/uptime-monitor/internal/auth"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// Session cookie parameters.
const (
	sessionCookie = "uptime_session"
	sessionTTL    = 7 * 24 * time.Hour

	// sessionRenewAfter is how much of the TTL must elapse before a request
	// slides the expiry forward. Without it every request writes to the sessions
	// table; with it, at most one write per hour per session.
	sessionRenewAfter = time.Hour
)

// loginRequest is the POST /api/auth/login body.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin verifies credentials and issues a session cookie.
//
// Every failure path returns the same 401 and the same message. Distinguishing
// "no such user" from "wrong password" would turn this endpoint into a username
// oracle.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return
	}
	var in loginRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.Username == "" || in.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	ip := s.clientIP(r)
	keys := []string{"user:" + in.Username, "ip:" + ip}
	if !s.loginLimiter.Allowed(keys...) {
		retry := s.loginLimiter.RetryAfter(keys...)
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		log.Printf("auth: login throttled for %q from %s", in.Username, ip)
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}

	user, err := s.reg.AdminUserByName(in.Username)
	if err != nil && !errors.Is(err, storage.ErrUserNotFound) {
		writeError(w, http.StatusInternalServerError, "login unavailable")
		return
	}
	// Hash-compare even when the user does not exist, against a dummy digest of
	// the same cost, so the response time cannot distinguish the two cases.
	hash := auth.DummyHash
	if err == nil {
		hash = user.PasswordHash
	}
	if !auth.CheckPassword(hash, in.Password) || err != nil {
		s.loginLimiter.RecordFailure(keys...)
		log.Printf("auth: failed login for %q from %s", in.Username, ip)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, id, err := auth.NewSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	expires := time.Now().Add(sessionTTL)
	if err := s.reg.CreateSession(id, user.ID, expires.Unix()); err != nil {
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}

	s.loginLimiter.Reset(keys...)
	http.SetCookie(w, s.sessionCookieFor(token, expires))
	log.Printf("auth: login for %q from %s", user.Username, ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"username":   user.Username,
		"expires_at": expires.Unix(),
	})
}

// handleLogout revokes the current session server-side and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.reg.DeleteSession(auth.HashToken(c.Value)); err != nil {
			log.Printf("auth: revoke session: %v", err)
		}
	}
	http.SetCookie(w, s.expiredSessionCookie())
	w.WriteHeader(http.StatusNoContent)
}

// handleMe reports who the caller is. The SPA uses it on load to decide between
// the dashboard and the login screen.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.sessionUser(r)
	if !ok {
		// Reached with an API key rather than a cookie: authenticated, but not
		// as a person.
		writeJSON(w, http.StatusOK, map[string]any{"username": nil, "auth": "api_key"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "auth": "session"})
}

// passwordRequest is the POST /api/auth/password body.
type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword rotates the signed-in user's password. It requires the
// current password even though the caller is authenticated, so a session left
// open on an unattended machine cannot be used to seize the account.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := s.sessionUser(r)
	if !ok {
		writeError(w, http.StatusForbidden, "password changes require a logged-in session, not an API key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return
	}
	var in passwordRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !auth.CheckPassword(user.PasswordHash, in.CurrentPassword) {
		log.Printf("auth: failed password change for %q from %s", user.Username, s.clientIP(r))
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	hash, err := auth.HashPassword(in.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Revokes every session for this user, including the caller's.
	if err := s.reg.SetAdminPassword(user.ID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "could not update password")
		return
	}
	http.SetCookie(w, s.expiredSessionCookie())
	log.Printf("auth: password changed for %q; all sessions revoked", user.Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "password updated; all sessions signed out",
	})
}

// sessionUser resolves the caller's session cookie to an account, sliding the
// expiry when it is far enough along to be worth a write. Returns false when the
// request carries no valid session (an API-key caller, or an unauthenticated one).
func (s *Server) sessionUser(r *http.Request) (storage.AdminUser, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return storage.AdminUser{}, false
	}
	sess, err := s.reg.LookupSession(auth.HashToken(c.Value))
	if err != nil {
		return storage.AdminUser{}, false
	}
	user, err := s.reg.AdminUserByID(sess.UserID)
	if err != nil {
		return storage.AdminUser{}, false
	}
	if time.Since(time.Unix(sess.LastSeenAt, 0)) > sessionRenewAfter {
		if err := s.reg.TouchSession(sess.ID, time.Now().Add(sessionTTL).Unix()); err != nil {
			log.Printf("auth: slide session expiry: %v", err)
		}
	}
	return user, true
}

// sessionOK reports whether the request carries a valid session.
func (s *Server) sessionOK(r *http.Request) bool {
	_, ok := s.sessionUser(r)
	return ok
}

// sessionCookieFor builds the login cookie.
//
// Secure is always set: the service is reached over HTTPS through the reverse
// proxy, and a cookie that would travel in clear is worse than no login at all.
// SameSite=Lax is sufficient because the UI is served from the same origin as
// the API; a cross-origin deployment would need None plus an explicit CSRF token.
func (s *Server) sessionCookieFor(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

// expiredSessionCookie clears the cookie in the browser.
func (s *Server) expiredSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}
