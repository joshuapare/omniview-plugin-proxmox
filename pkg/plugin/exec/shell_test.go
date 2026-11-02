package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	sdkexec "github.com/omniviewdev/plugin-sdk/pkg/v1/exec"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// body is a resource body as the host passes it.
func body(fields map[string]any) *resourcev2.Value {
	v, err := resourcev2.ValueFromNative(fields)
	if err != nil {
		panic(err)
	}
	return &v
}

// fakePVE serves termproxy and vncwebsocket; ws runs on each accepted
// websocket after the handler checks the termproxy handshake.
type fakePVE struct {
	t         *testing.T
	termproxy http.HandlerFunc // nil → a valid ticket
	ws        func(ctx context.Context, conn *websocket.Conn)
	paths     chan string
}

func (f *fakePVE) clients() clientFunc {
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	f.paths = make(chan string, 8)
	return func(*types.PluginContext) (*proxmox.Client, error) {
		return proxmox.NewClient(proxmox.Cluster{
			ID: "c", URL: srv.URL, TokenID: "root@pam!omni", TokenSecret: "secret", InsecureSkipVerify: true,
		})
	}
}

func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!omni=secret" {
		http.Error(w, "authentication failure", http.StatusUnauthorized)
		return
	}
	f.paths <- r.Method + " " + r.URL.Path
	switch {
	case strings.HasSuffix(r.URL.Path, "/termproxy"):
		if f.termproxy != nil {
			f.termproxy(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"port":5900,"ticket":"PVEVNC:tk","user":"root@pam!omni","upid":"UPID:x"}}`)
	case strings.HasSuffix(r.URL.Path, "/vncwebsocket"):
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			f.t.Errorf("accept: %v", err)
			return
		}
		ctx := r.Context()
		_, hello, err := conn.Read(ctx)
		if err != nil || string(hello) != "root@pam!omni:PVEVNC:tk\n" {
			f.t.Errorf("first message = %q, %v; want the user:ticket handshake", hello, err)
			conn.CloseNow()
			return
		}
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("OK"))
		f.ws(ctx, conn)
	default:
		http.NotFound(w, r)
	}
}

// session runs handler on a real raw-mode PTY like the SDK manager does and
// returns the PTY master (the user's side).
type session struct {
	master *os.File
	stopCh chan error
	resize chan sdkexec.SessionResizeInput
	cancel context.CancelFunc
}

func startSession(t *testing.T, handler sdkexec.TTYHandlerFunc, opts sdkexec.SessionOptions) *session {
	t.Helper()
	term, err := sdkexec.NewRealTerminalFactory()()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { term.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &session{
		master: term.MasterFd(),
		stopCh: make(chan error, 1),
		resize: make(chan sdkexec.SessionResizeInput),
		cancel: cancel,
	}
	// Setup failures must arrive on stopCh, where the host renders them.
	if err := handler(&types.PluginContext{Context: ctx}, opts, term.SlaveFd(), s.stopCh, s.resize); err != nil {
		t.Fatalf("handler returned %v; failures belong on stopCh", err)
	}
	return s
}

// stopped returns the session's stop value and checks it is the only one.
func (s *session) stopped(t *testing.T) error {
	t.Helper()
	var err error
	select {
	case err = <-s.stopCh:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not stop")
	}
	select {
	case again := <-s.stopCh:
		t.Fatalf("second stop value %v after %v", again, err)
	case <-time.After(100 * time.Millisecond):
	}
	return err
}

func stoppedWithExecError(t *testing.T, s *session) *sdkexec.ExecError {
	t.Helper()
	var execErr *sdkexec.ExecError
	if err := s.stopped(t); !errors.As(err, &execErr) {
		t.Fatalf("stop error = %v, want ExecError", err)
	}
	return execErr
}

func handlerFor(t *testing.T, key string, clients clientFunc) sdkexec.TTYHandlerFunc {
	t.Helper()
	for _, h := range handlers(clients) {
		if h.Plugin == "proxmox" && h.Resource == key {
			if !h.HandlesResize {
				t.Fatalf("%s handler does not handle resize", key)
			}
			return h.TTYHandler
		}
	}
	t.Fatalf("no handler for %s", key)
	return nil
}

func TestShellBridgesPTYAndTermproxy(t *testing.T) {
	frames := make(chan string, 4)
	f := &fakePVE{t: t}
	f.ws = func(ctx context.Context, conn *websocket.Conn) {
		for range 3 {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				t.Errorf("server read: %v", err)
				return
			}
			frames <- string(msg)
		}
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("hello\r\n"))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	handler := handlerFor(t, keyContainer, f.clients())

	s := startSession(t, handler, sdkexec.SessionOptions{
		ResourcePlugin: "proxmox", ResourceKey: keyContainer,
		ResourceData: body(map[string]any{"id": "lxc/101", "node": "pve2", "vmid": 101}),
		TTY:          true,
	})
	// The UI sizes the terminal right away, usually while still connecting.
	s.resize <- sdkexec.SessionResizeInput{Cols: 100, Rows: 30}
	for _, want := range []string{"POST /api2/json/nodes/pve2/lxc/101/termproxy", "GET /api2/json/nodes/pve2/lxc/101/vncwebsocket"} {
		if got := <-f.paths; got != want {
			t.Fatalf("request = %q, want %q", got, want)
		}
	}
	if got := <-frames; got != "1:100:30:" {
		t.Fatalf("early resize frame = %q", got)
	}

	if _, err := s.master.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if got := <-frames; got != "0:3:ls\n" {
		t.Fatalf("input frame = %q", got)
	}
	s.resize <- sdkexec.SessionResizeInput{Cols: 132, Rows: 43}
	if got := <-frames; got != "1:132:43:" {
		t.Fatalf("resize frame = %q", got)
	}

	out := make([]byte, 7)
	if _, err := io.ReadFull(s.master, out); err != nil || string(out) != "hello\r\n" {
		t.Fatalf("tty output = %q, %v", out, err)
	}
	if err := s.stopped(t); err != nil {
		t.Fatalf("stop error = %v, want nil on a normal close", err)
	}
}

func TestShellCancelClosesWebsocket(t *testing.T) {
	closed := make(chan websocket.StatusCode, 1)
	f := &fakePVE{t: t}
	f.ws = func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("ready"))
		_, _, err := conn.Read(ctx)
		closed <- websocket.CloseStatus(err)
	}
	s := startSession(t, handlerFor(t, keyNode, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "node/ares", "node": "ares"}),
	})
	// Output on the PTY means the session is connected and bridging.
	ready := make([]byte, 5)
	if _, err := io.ReadFull(s.master, ready); err != nil {
		t.Fatal(err)
	}
	s.cancel()
	select {
	case code := <-closed:
		if code != websocket.StatusNormalClosure {
			t.Fatalf("close status = %v, want normal closure", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("websocket not closed after cancel")
	}
	if err := s.stopped(t); err != nil {
		t.Fatalf("stop error = %v, want nil after cancel", err)
	}
}

func TestShellCancelWhileConnecting(t *testing.T) {
	requested := make(chan struct{})
	f := &fakePVE{t: t, termproxy: func(_ http.ResponseWriter, r *http.Request) {
		close(requested)
		<-r.Context().Done() // never answers; the client must give up
	}}
	s := startSession(t, handlerFor(t, keyNode, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "node/ares"}),
	})
	<-requested
	s.cancel()
	if err := s.stopped(t); err != nil {
		t.Fatalf("stop error = %v, want nil after cancel", err)
	}
}

// The watcher and a failed connect both finish the session. This forces the
// order the watcher loses: the session is already cancelled when the
// connect error arrives, and no watcher runs.
func TestShellConnectErrorAfterCancelIsCleanStop(t *testing.T) {
	f := &fakePVE{t: t}
	clients := f.clients()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopCh := make(chan error, 1)
	s := &shellSession{stopCh: stopCh, done: make(chan struct{})}
	s.connect(&types.PluginContext{Context: ctx}, sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "node/ares", "node": "ares"}),
	}, clients, nodeTarget)
	select {
	case err := <-stopCh:
		if err != nil {
			t.Fatalf("stop error = %v, want nil after cancel", err)
		}
	default:
		t.Fatal("connect did not finish the cancelled session")
	}
}

func TestShellInvalidResourceReportedOnStopCh(t *testing.T) {
	f := &fakePVE{t: t}
	s := startSession(t, handlerFor(t, keyVM, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "qemu/100"}),
	})
	if execErr := stoppedWithExecError(t, s); execErr.Title != "Invalid resource" || execErr.Retryable {
		t.Fatalf("ExecError = %+v", execErr)
	}
}

func TestShellPermissionDenied(t *testing.T) {
	f := &fakePVE{t: t, termproxy: func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission check failed (/nodes/ares, Sys.Console)", http.StatusForbidden)
	}}
	s := startSession(t, handlerFor(t, keyNode, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "node/ares"}),
	})
	execErr := stoppedWithExecError(t, s)
	// PVE puts "Permission check failed (...)" in the reason phrase, which a Go
	// test server cannot set; Message carries it live.
	if execErr.Title != "Permission denied" || execErr.Retryable ||
		!strings.Contains(execErr.Suggestion, "Sys.Console on /nodes/ares") {
		t.Fatalf("ExecError = %+v", execErr)
	}
}

func TestShellVMWithoutSerialPort(t *testing.T) {
	serial := make(chan string, 1)
	f := &fakePVE{t: t}
	f.termproxy = func(w http.ResponseWriter, r *http.Request) {
		serial <- r.PostFormValue("serial")
		_, _ = io.WriteString(w, `{"data":{"port":"5900","ticket":"PVEVNC:tk","user":"root@pam!omni","upid":"UPID:x"}}`)
	}
	f.ws = func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("unable to find a serial interface\r\n"))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	s := startSession(t, handlerFor(t, keyVM, f.clients()), sdkexec.SessionOptions{
		ResourceData: body(map[string]any{"id": "qemu/100", "node": "apollo", "vmid": 100}),
	})
	if got := <-serial; got != "serial0" {
		t.Fatalf("serial = %q, want serial0", got)
	}
	execErr := stoppedWithExecError(t, s)
	if execErr.Title != "No serial port" || !strings.Contains(execErr.Suggestion, "qm set 100 -serial0 socket") {
		t.Fatalf("ExecError = %+v", execErr)
	}
}

func TestTargets(t *testing.T) {
	cases := []struct {
		name    string
		resolve targetFunc
		opts    sdkexec.SessionOptions
		want    proxmox.TermTarget
		wantErr bool
	}{
		{"node from id", nodeTarget, sdkexec.SessionOptions{ResourceData: body(map[string]any{"id": "node/ares"})},
			proxmox.TermTarget{Node: "ares"}, false},
		{"node missing", nodeTarget, sdkexec.SessionOptions{ResourceData: body(map[string]any{})}, proxmox.TermTarget{}, true},
		{"lxc vmid from id", guestTarget(guestLXC),
			sdkexec.SessionOptions{ResourceData: body(map[string]any{"id": "lxc/1000", "omniview": map[string]any{"node": "apollo"}})},
			proxmox.TermTarget{Node: "apollo", GuestType: "lxc", VMID: 1000}, false},
		{"qemu serial param", guestTarget(guestQemu),
			sdkexec.SessionOptions{ResourceData: body(map[string]any{"node": "ares", "vmid": 100}), Params: map[string]string{"serial": "serial2"}},
			proxmox.TermTarget{Node: "ares", GuestType: "qemu", VMID: 100, Serial: "serial2"}, false},
		{"qemu bad serial", guestTarget(guestQemu),
			sdkexec.SessionOptions{ResourceData: body(map[string]any{"node": "ares", "vmid": 100}), Params: map[string]string{"serial": "ttyS0"}},
			proxmox.TermTarget{}, true},
		{"guest without vmid", guestTarget(guestQemu),
			sdkexec.SessionOptions{ResourceData: body(map[string]any{"node": "ares", "id": "lxc/5"})}, proxmox.TermTarget{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.resolve(tc.opts)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestClassifyOpenError(t *testing.T) {
	node := proxmox.TermTarget{Node: "ares"}
	vm := proxmox.TermTarget{Node: "ares", GuestType: guestQemu, VMID: 100, Serial: defaultSerial}
	token := consoleAuth{ClusterID: "homelab"}
	user := consoleAuth{ClusterID: "homelab", ConsoleUser: "omniview@pve"}
	loginErr := func(err error) error { return fmt.Errorf("%w for omniview@pve: %w", proxmox.ErrConsoleLogin, err) }
	cases := []struct {
		name       string
		err        error
		target     proxmox.TermTarget
		auth       consoleAuth
		title      string
		suggestion string
	}{
		{"node 403 with token", &proxmox.APIError{StatusCode: 403, Reason: "Permission check failed (/nodes/ares, Sys.Console)"},
			node, token, "Permission denied", "Sys.Console on /nodes/ares. Proxmox VE consoles reject API tokens"},
		{"guest 403 as console user", &proxmox.APIError{StatusCode: 403, Reason: "Permission check failed (/vms/100, VM.Console)"},
			vm, user, "Permission denied", "VM.Console on /vms/100: grant omniview@pve"},
		{"node shell with token", &proxmox.APIError{StatusCode: 500,
			Reason: "value 'omniview@pve!omniview' does not look like a valid user name"},
			node, token, "Console refused the API token", `Set console_user and console_password (or console_password_file) for cluster "homelab"`},
		{"guest handshake dropped with token", fmt.Errorf("%w: EOF", proxmox.ErrTerminalHandshake),
			vm, token, "Console refused the API token", "show_bug.cgi?id=6079"},
		{"handshake rejected as console user", fmt.Errorf("%w: EOF", proxmox.ErrTerminalHandshake),
			vm, user, "Terminal authentication failed", "Open the shell again"},
		{"console user has TFA", loginErr(proxmox.ErrConsoleTFA),
			node, user, "Console user has two-factor auth", "Use a user without TFA"},
		{"console password rejected", loginErr(&proxmox.APIError{StatusCode: 401, Reason: "authentication failure"}),
			node, user, "Console login failed", "rejected the password of omniview@pve"},
		{"guest stopped", &proxmox.APIError{StatusCode: 500, Reason: "CT 1000 not running"},
			proxmox.TermTarget{Node: "ares", GuestType: guestLXC, VMID: 1000}, user, "Guest not running", "Start the guest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var execErr *sdkexec.ExecError
			if !errors.As(classifyOpenError(tc.err, tc.target, tc.auth), &execErr) {
				t.Fatal("not an ExecError")
			}
			if execErr.Title != tc.title || !strings.Contains(execErr.Suggestion, tc.suggestion) {
				t.Fatalf("ExecError = %+v", execErr)
			}
		})
	}
}
