package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/stuart/uptime-monitor/internal/auth"
	"github.com/stuart/uptime-monitor/internal/storage"
)

// seedAdminUser creates the first admin account from the environment, if one is
// configured and no account exists yet. It returns whether an account exists
// afterwards, which the caller needs to decide if it may start at all.
//
// Seeding is deliberately once-only: after the first boot the hash in the
// database is authoritative, and editing UPTIME_ADMIN_PASSWORD does nothing.
// Anything else would mean a plaintext password in the env file permanently
// overriding a password changed through the UI.
func seedAdminUser(reg *storage.Registry) (bool, error) {
	n, err := reg.AdminUserCount()
	if err != nil {
		return false, fmt.Errorf("count admin users: %w", err)
	}
	if n > 0 {
		return true, nil
	}

	user := strings.TrimSpace(os.Getenv("UPTIME_ADMIN_USER"))
	pass := os.Getenv("UPTIME_ADMIN_PASSWORD")
	if user == "" || pass == "" {
		return false, nil
	}

	hash, err := auth.HashPassword(pass)
	if err != nil {
		return false, fmt.Errorf("UPTIME_ADMIN_PASSWORD rejected: %w", err)
	}
	if _, err := reg.CreateAdminUser(user, hash); err != nil {
		return false, fmt.Errorf("create admin user: %w", err)
	}
	log.Printf("auth: seeded initial admin user %q from the environment; "+
		"remove UPTIME_ADMIN_USER/UPTIME_ADMIN_PASSWORD from the env file now", user)
	return true, nil
}

// setPassword implements -set-password: the lockout escape hatch. It reads the
// new password from the terminal rather than a flag so it never reaches shell
// history or the process table, then exits without starting the service.
func setPassword(reg *storage.Registry, username string) error {
	fmt.Fprintf(os.Stderr, "New password for %q: ", username)
	pw1, err := readSecret()
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "Repeat: ")
	pw2, err := readSecret()
	if err != nil {
		return err
	}
	if pw1 != pw2 {
		return errors.New("passwords do not match")
	}

	hash, err := auth.HashPassword(pw1)
	if err != nil {
		return err
	}

	user, err := reg.AdminUserByName(username)
	switch {
	case errors.Is(err, storage.ErrUserNotFound):
		if _, err := reg.CreateAdminUser(username, hash); err != nil {
			return fmt.Errorf("create admin user: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Created admin user %q.\n", username)
	case err != nil:
		return err
	default:
		if err := reg.SetAdminPassword(user.ID, hash); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Password updated for %q; all sessions signed out.\n", username)
	}
	return nil
}

// readSecret reads one line without echoing it. When stdin is not a terminal
// (a pipe, or a provisioning script) it falls back to a plain read so the
// command stays scriptable.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	return line, nil
}
