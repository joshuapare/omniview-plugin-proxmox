package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// ErrConsoleLogin wraps every failure to log the console user in.
var ErrConsoleLogin = errors.New("console login failed")

// ErrConsoleTFA reports a console user with two-factor authentication,
// which a non-interactive login cannot complete.
var ErrConsoleTFA = errors.New("console user has two-factor authentication")

// consoleLogin is the optional user login for console sessions; the zero
// value means consoles use the API token.
type consoleLogin struct {
	user         string
	password     string
	passwordFile string
}

func consoleLoginFor(c Cluster) consoleLogin {
	return consoleLogin{user: c.ConsoleUser, password: c.ConsolePassword, passwordFile: c.ConsolePasswordFile}
}

// ConsoleUser returns the user that opens consoles, or "" when consoles use
// the API token.
func (c *Client) ConsoleUser() string {
	return c.console.user
}

// consoleSession is a PVE login ticket. Tickets last two hours; each
// console fetches its own, so none is cached.
type consoleSession struct {
	ticket string
	csrf   string
}

// authorize sets the ticket cookie, plus the CSRF token on writes.
func (s consoleSession) authorize(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: s.ticket})
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.Header.Set("CSRFPreventionToken", s.csrf)
	}
}

// loginConsole exchanges the console user's password for a ticket via POST
// /access/ticket. The password never appears in errors.
func (c *Client) loginConsole(ctx context.Context) (session consoleSession, err error) {
	ctx, span := Tracer().Start(ctx, "proxmox.console.login")
	span.SetAttributes(attribute.String("proxmox.cluster_id", c.ClusterID))
	defer func() {
		EndSpan(span, err)
		if err != nil {
			pkgLogger().Warnw(ctx, "proxmox console login failed",
				append([]any{"cluster_id", c.ClusterID}, classFields(err)...)...)
		}
	}()
	password := c.console.password
	if c.console.passwordFile != "" {
		path, err := ExpandPath(c.console.passwordFile)
		if err != nil {
			return consoleSession{}, fmt.Errorf("%w: %w", ErrConsoleLogin, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return consoleSession{}, fmt.Errorf("%w: read console_password_file: %w", ErrConsoleLogin, err)
		}
		password = strings.TrimSpace(string(raw))
		if password == "" {
			return consoleSession{}, fmt.Errorf("%w: console_password_file %s is empty", ErrConsoleLogin, c.console.passwordFile)
		}
	}

	var resp struct {
		Ticket  string          `json:"ticket"`
		CSRF    string          `json:"CSRFPreventionToken"`
		NeedTFA json.RawMessage `json:"NeedTFA"`
	}
	form := url.Values{"username": {c.console.user}, "password": {password}}
	noAuth := func(*http.Request) {}
	if err := c.send(ctx, noAuth, http.MethodPost, "/access/ticket", nil, form, &resp); err != nil {
		return consoleSession{}, fmt.Errorf("%w for %s: %w", ErrConsoleLogin, c.console.user, err)
	}
	switch strings.Trim(string(resp.NeedTFA), `"`) {
	case "", "0", "false", "null":
	default:
		return consoleSession{}, fmt.Errorf("%w for %s: %w", ErrConsoleLogin, c.console.user, ErrConsoleTFA)
	}
	if resp.Ticket == "" || resp.CSRF == "" {
		return consoleSession{}, fmt.Errorf("%w for %s: no ticket in the reply", ErrConsoleLogin, c.console.user)
	}
	return consoleSession{ticket: resp.Ticket, csrf: resp.CSRF}, nil
}
