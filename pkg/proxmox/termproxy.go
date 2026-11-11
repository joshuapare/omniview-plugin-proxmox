package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TerminalKeepalive is how often an open terminal pings the termproxy, as the
// Proxmox web UI does; termproxy drops idle connections.
const TerminalKeepalive = 30 * time.Second

// TermTarget selects the console a terminal attaches to: the node shell when
// GuestType is empty, otherwise the console of guest VMID ("qemu" or "lxc").
type TermTarget struct {
	Node      string
	GuestType string
	VMID      int
	// Serial is the qemu serial interface (serial0..serial3); qemu only.
	Serial string
}

func (t TermTarget) path() string {
	p := "/nodes/" + url.PathEscape(t.Node)
	if t.GuestType != "" {
		p += "/" + t.GuestType + "/" + strconv.Itoa(t.VMID)
	}
	return p
}

// kind is the closed console kind: node, qemu or lxc.
func (t TermTarget) kind() string {
	switch t.GuestType {
	case "":
		return "node"
	case "qemu", "lxc":
		return t.GuestType
	default:
		return "other"
	}
}

func (t TermTarget) spanAttrs() []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("proxmox.node", t.Node),
		attribute.String("proxmox.kind", t.kind()),
	}
	if t.GuestType != "" {
		attrs = append(attrs, attribute.Int("proxmox.vmid", t.VMID))
	}
	return attrs
}

func (t TermTarget) logFields() []any {
	fields := []any{"node", t.Node, "kind", t.kind()}
	if t.GuestType != "" {
		fields = append(fields, "vmid", t.VMID)
	}
	return fields
}

// TermProxy is the POST .../termproxy payload: a one-shot ticket for the
// console process listening on Port.
type TermProxy struct {
	Port   int
	Ticket string
	User   string
	UPID   string
}

func (tp *TermProxy) UnmarshalJSON(b []byte) error {
	var raw struct {
		Port   json.RawMessage `json:"port"`
		Ticket string          `json:"ticket"`
		User   string          `json:"user"`
		UPID   string          `json:"upid"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	// PVE serializes the port as a number or a numeric string.
	port, err := strconv.Atoi(strings.Trim(string(raw.Port), `"`))
	if err != nil {
		return fmt.Errorf("termproxy port %s: %w", raw.Port, err)
	}
	*tp = TermProxy{Port: port, Ticket: raw.Ticket, User: raw.User, UPID: raw.UPID}
	return nil
}

// termProxy starts a console process for target.
func (c *Client) termProxy(ctx context.Context, authorize func(*http.Request), target TermTarget) (tp TermProxy, err error) {
	ctx, span := Tracer().Start(ctx, "proxmox.termproxy")
	span.SetAttributes(target.spanAttrs()...)
	defer func() { EndSpan(span, err) }()
	form := url.Values{}
	if target.GuestType == "qemu" && target.Serial != "" {
		form.Set("serial", target.Serial)
	}
	err = c.send(ctx, authorize, http.MethodPost, target.path()+"/termproxy", nil, form, &tp)
	return tp, err
}

// Terminal is an authenticated pve-xtermjs websocket. Read yields the raw
// console output; Write sends input. Close ends the console.
type Terminal struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	// pending holds output received with the handshake reply.
	pending []byte
	reader  io.Reader

	closeOnce sync.Once
	done      chan struct{}

	// Session accounting for the close log line: bytesIn is console output
	// received, bytesOut is input and control frames sent.
	started   time.Time
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	clusterID string
	target    TermTarget
	logOnce   sync.Once
}

// terminalDialError reports a failed websocket dial. The underlying error
// text embeds the vncwebsocket URL, including the one-shot vncticket, so it
// is dropped; only the timeout bit and a context error survive.
type terminalDialError struct {
	timeout bool
	ctxErr  error
}

func (e *terminalDialError) Error() string   { return "connect terminal websocket failed" }
func (e *terminalDialError) Timeout() bool   { return e.timeout }
func (e *terminalDialError) Temporary() bool { return false }
func (e *terminalDialError) Unwrap() error   { return e.ctxErr }

func newTerminalDialError(ctx context.Context, err error) *terminalDialError {
	out := &terminalDialError{}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		out.timeout = true
	}
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		out.ctxErr = context.Canceled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		out.ctxErr = context.DeadlineExceeded
	}
	return out
}

// OpenTerminal starts a console for target and connects to it: termproxy,
// vncwebsocket, then the `user:ticket` handshake. With a console user
// configured, it first logs that user in and authenticates both requests
// with the login ticket (PVE consoles reject API tokens); otherwise it
// tries the token. ctx bounds only the setup; the terminal lives until
// Close or the server ends it.
func (c *Client) OpenTerminal(ctx context.Context, target TermTarget) (term *Terminal, err error) {
	ctx, span := Tracer().Start(ctx, "proxmox.terminal.open")
	authMode := "token"
	if c.console.user != "" {
		authMode = "ticket"
	}
	span.SetAttributes(append(target.spanAttrs(),
		attribute.String("proxmox.cluster_id", c.ClusterID),
		attribute.String("auth.mode", authMode))...)
	start := time.Now()
	defer func() {
		EndSpan(span, err)
		if err != nil {
			TerminalSession("failed")
			pkgLogger().Warnw(ctx, "proxmox terminal open failed",
				append(append([]any{"cluster_id", c.ClusterID, "duration_ms", durationMs(time.Since(start))}, target.logFields()...), classFields(err)...)...)
			return
		}
		TerminalSession("opened")
		pkgLogger().Infow(ctx, "proxmox terminal opened",
			append([]any{"cluster_id", c.ClusterID, "duration_ms", durationMs(time.Since(start))}, target.logFields()...)...)
	}()

	authorize := c.authorizeToken
	if c.console.user != "" {
		session, err := c.loginConsole(ctx)
		if err != nil {
			return nil, err
		}
		authorize = session.authorize
	}
	tp, err := c.termProxy(ctx, authorize, target)
	if err != nil {
		return nil, err
	}
	return c.dialTerminal(ctx, authorize, target, tp, TerminalKeepalive)
}

func (c *Client) dialTerminal(
	ctx context.Context,
	authorize func(*http.Request),
	target TermTarget,
	tp TermProxy,
	keepalive time.Duration,
) (*Terminal, error) {
	u, err := c.endpoint(target.path() + "/vncwebsocket")
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.RawQuery = url.Values{"port": {strconv.Itoa(tp.Port)}, "vncticket": {tp.Ticket}}.Encode()

	// The handshake request is a GET: authorize sets the token header or
	// the ticket cookie, never a CSRF token.
	headers, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	authorize(headers)

	dialCtx, dialSpan := Tracer().Start(ctx, "proxmox.terminal.dial", trace.WithAttributes(target.spanAttrs()...))
	conn, resp, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{
		HTTPClient:   c.websocketHTTPClient(),
		HTTPHeader:   headers.Header,
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
			apiErr := &APIError{StatusCode: resp.StatusCode, Reason: reason}
			EndSpan(dialSpan, apiErr)
			pkgLogger().Debugw(ctx, "proxmox terminal dial failed",
				append(target.logFields(), classFields(apiErr)...)...)
			return nil, apiErr
		}
		dialErr := newTerminalDialError(ctx, err)
		EndSpan(dialSpan, dialErr)
		pkgLogger().Debugw(ctx, "proxmox terminal dial failed",
			append(target.logFields(), classFields(dialErr)...)...)
		return nil, dialErr
	}
	EndSpan(dialSpan, nil)
	conn.SetReadLimit(-1)

	hsCtx, hsSpan := Tracer().Start(ctx, "proxmox.terminal.handshake", trace.WithAttributes(target.spanAttrs()...))
	pending, err := terminalHandshake(hsCtx, conn, tp)
	EndSpan(hsSpan, err)
	if err != nil {
		pkgLogger().Debugw(ctx, "proxmox terminal handshake failed",
			append(target.logFields(), classFields(err)...)...)
		conn.CloseNow()
		return nil, err
	}

	lifetime, cancel := context.WithCancel(context.Background())
	t := &Terminal{
		conn: conn, ctx: lifetime, cancel: cancel, pending: pending, done: make(chan struct{}),
		started: time.Now(), clusterID: c.ClusterID, target: target,
	}
	t.bytesIn.Add(int64(len(pending)))
	go t.keepalive(keepalive)
	return t, nil
}

// websocketHTTPClient shares the API client's TLS settings but speaks
// HTTP/1.1 only: the websocket upgrade does not exist in HTTP/2.
func (c *Client) websocketHTTPClient() *http.Client {
	base, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return c.http
	}
	transport := base.Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &http.Client{Transport: transport}
}

// ErrTerminalHandshake reports that termproxy did not accept the console
// ticket: it answered something other than "OK" or closed the connection.
var ErrTerminalHandshake = errors.New("terminal handshake failed")

// terminalHandshake authenticates to termproxy and returns any output that
// arrived with its "OK".
func terminalHandshake(ctx context.Context, conn *websocket.Conn, tp TermProxy) ([]byte, error) {
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(tp.User+":"+tp.Ticket+"\n")); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTerminalHandshake, err)
	}
	_, reply, err := conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTerminalHandshake, err)
	}
	if !bytes.HasPrefix(reply, []byte("OK")) {
		return nil, fmt.Errorf("%w: termproxy did not answer OK", ErrTerminalHandshake)
	}
	return reply[2:], nil
}

// Read returns console output. It returns io.EOF once the server closes the
// console.
func (t *Terminal) Read(p []byte) (int, error) {
	if len(t.pending) > 0 {
		n := copy(p, t.pending)
		t.pending = t.pending[n:]
		return n, nil
	}
	for {
		if t.reader == nil {
			_, r, err := t.conn.Reader(t.ctx)
			if err != nil {
				return 0, t.readEnded(err)
			}
			t.reader = r
		}
		n, err := t.reader.Read(p)
		if errors.Is(err, io.EOF) {
			t.reader = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		t.bytesIn.Add(int64(n))
		if err != nil {
			return n, t.readEnded(err)
		}
		return n, nil
	}
}

// readEnded logs the end of the session and returns the error Read reports.
func (t *Terminal) readEnded(err error) error {
	out := terminalReadError(err)
	switch {
	case errors.Is(out, io.EOF):
		t.logClosed("peer", nil)
	case errors.Is(err, context.Canceled):
		t.logClosed("canceled", err)
	default:
		t.logClosed("error", err)
	}
	return out
}

// logClosed records the session end once: Info with accounting, never the
// transport error text.
func (t *Terminal) logClosed(reason string, err error) {
	t.logOnce.Do(func() {
		TerminalSession("closed")
		fields := append([]any{
			"cluster_id", t.clusterID,
			"close_reason", reason,
			"bytes_in", t.bytesIn.Load(),
			"bytes_out", t.bytesOut.Load(),
			"duration_ms", durationMs(time.Since(t.started)),
		}, t.target.logFields()...)
		if err != nil {
			fields = append(fields, classFields(err)...)
		}
		pkgLogger().Infow(context.Background(), "proxmox terminal closed", fields...)
	})
}

func terminalReadError(err error) error {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return io.EOF
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return err
}

// Write sends p as one input frame: `0:<bytelen>:<data>`.
func (t *Terminal) Write(p []byte) (int, error) {
	frame := make([]byte, 0, len(p)+16)
	frame = append(frame, "0:"...)
	frame = strconv.AppendInt(frame, int64(len(p)), 10)
	frame = append(frame, ':')
	frame = append(frame, p...)
	if err := t.conn.Write(t.ctx, websocket.MessageBinary, frame); err != nil {
		return 0, err
	}
	t.bytesOut.Add(int64(len(frame)))
	return len(p), nil
}

// Resize sends the terminal size: `1:<cols>:<rows>:`.
func (t *Terminal) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid terminal size %dx%d", cols, rows)
	}
	frame := fmt.Appendf(nil, "1:%d:%d:", cols, rows)
	err := t.conn.Write(t.ctx, websocket.MessageBinary, frame)
	if err == nil {
		t.bytesOut.Add(int64(len(frame)))
	}
	return err
}

func (t *Terminal) keepalive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			if err := t.conn.Write(t.ctx, websocket.MessageBinary, []byte("2")); err != nil {
				pkgLogger().Debugw(context.Background(), "proxmox terminal keepalive failed",
					append(t.target.logFields(), classFields(err)...)...)
				return
			}
			t.bytesOut.Add(1)
		}
	}
}

// Close sends a normal closure and releases the connection. It is safe to
// call more than once.
func (t *Terminal) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.done)
		t.logClosed("normal", nil)
		err = t.conn.Close(websocket.StatusNormalClosure, "")
		t.cancel()
	})
	if errors.Is(err, net.ErrClosed) || websocket.CloseStatus(err) != -1 {
		// The peer closed first; the closure is already complete.
		return nil
	}
	return err
}
