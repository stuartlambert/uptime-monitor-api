package alerts

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTPConfig is the mail server configuration, read from the environment in
// cmd/monitor. Credentials never reach the database or an API response.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// InsecureSkipVerify disables certificate verification. Present for hosts
	// with a self-signed mail certificate; leaving it off is strongly preferred.
	InsecureSkipVerify bool
}

// Configured reports whether enough is set to attempt delivery.
func (c SMTPConfig) Configured() bool {
	return c.Host != "" && c.From != ""
}

// Addr is the host:port dial target.
func (c SMTPConfig) Addr() string {
	port := c.Port
	if port == 0 {
		port = 587
	}
	return net.JoinHostPort(c.Host, fmt.Sprint(port))
}

// SMTPSender delivers mail over SMTP.
//
// Port 465 is treated as implicit TLS (SMTPS); every other port is dialled in
// clear and upgraded with STARTTLS. Upgrading is mandatory, not best-effort:
// the alert names hosts and error text, and the session carries a password.
type SMTPSender struct {
	cfg SMTPConfig
}

// NewSMTPSender builds a sender.
func NewSMTPSender(cfg SMTPConfig) *SMTPSender { return &SMTPSender{cfg: cfg} }

// Send delivers one message. The context bounds the whole exchange.
func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	if !s.cfg.Configured() {
		return errors.New("smtp is not configured (set UPTIME_SMTP_HOST and UPTIME_SMTP_FROM)")
	}
	if to == "" {
		return errors.New("no recipient")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	d := net.Dialer{Deadline: deadline}

	var (
		conn net.Conn
		err  error
	)
	tlsCfg := &tls.Config{
		ServerName:         s.cfg.Host,
		InsecureSkipVerify: s.cfg.InsecureSkipVerify,
	}
	if s.cfg.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", s.cfg.Addr(), tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.cfg.Addr())
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", s.cfg.Addr(), err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()

	if s.cfg.Port != 465 {
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			return errors.New("server does not offer STARTTLS; refusing to send credentials in clear")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to %s: %w", to, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write([]byte(buildMessage(s.cfg.From, to, subject, body))); err != nil {
		w.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return c.Quit()
}

// buildMessage renders RFC 5322 headers plus the body, with CRLF line endings.
func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitiseHeader(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return b.String()
}

// sanitiseHeader strips CR and LF from a header value.
//
// Subjects contain a site name that an admin typed. Without this, a name
// containing a newline could inject extra headers — a Bcc, say — into every
// alert the site generates.
func sanitiseHeader(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	// Collapse the runs the replacements leave behind, so a stripped CRLF does
	// not show up as a gap in the subject line.
	return strings.Join(strings.Fields(v), " ")
}
