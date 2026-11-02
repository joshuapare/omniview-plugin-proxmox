package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	logging "github.com/omniviewdev/plugin-sdk/log"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	sdkexec "github.com/omniviewdev/plugin-sdk/pkg/v1/exec"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const (
	guestQemu = "qemu"
	guestLXC  = "lxc"

	defaultSerial = "serial0"

	// setupTimeout bounds termproxy, the websocket dial and the handshake.
	setupTimeout = 30 * time.Second

	// A console that fails (no serial port, guest stopped) prints one line
	// and exits at once. Output within headBytes of a session that ended
	// within earlyExit is checked for those failures.
	headBytes = 1024
	earlyExit = 15 * time.Second
)

var serialIface = regexp.MustCompile(`^serial[0-3]$`)

// targetFunc resolves the console target from the session's resource data.
type targetFunc func(opts sdkexec.SessionOptions) (proxmox.TermTarget, error)

// nodeTarget resolves a node shell from a Node object (`node`, or `id`
// node/<name>).
func nodeTarget(opts sdkexec.SessionOptions) (proxmox.TermTarget, error) {
	data := proxmox.ResourceData(opts.ResourceData)
	node := nodeOf(data)
	if node == "" {
		if id, _ := data["id"].(string); strings.HasPrefix(id, "node/") {
			node = strings.TrimPrefix(id, "node/")
		}
	}
	if node == "" {
		return proxmox.TermTarget{}, errors.New("resource data has no node")
	}
	return proxmox.TermTarget{Node: node}, nil
}

// guestTarget resolves a guest console from a VirtualMachine or Container
// object (`node`, `vmid`, or `id` <type>/<vmid>).
func guestTarget(guestType string) targetFunc {
	return func(opts sdkexec.SessionOptions) (proxmox.TermTarget, error) {
		data := proxmox.ResourceData(opts.ResourceData)
		node := nodeOf(data)
		if node == "" {
			return proxmox.TermTarget{}, errors.New("resource data has no node")
		}
		vmid, ok := intOf(data["vmid"])
		if !ok {
			id, _ := data["id"].(string)
			vmid, ok = intOf(strings.TrimPrefix(id, guestType+"/"))
		}
		if !ok || vmid <= 0 {
			return proxmox.TermTarget{}, errors.New("resource data has no vmid")
		}
		target := proxmox.TermTarget{Node: node, GuestType: guestType, VMID: vmid}
		if guestType == guestQemu {
			target.Serial = defaultSerial
			if s := opts.Params[ParamSerial]; s != "" {
				if !serialIface.MatchString(s) {
					return proxmox.TermTarget{}, fmt.Errorf("invalid serial interface %q: want serial0..serial3", s)
				}
				target.Serial = s
			}
		}
		return target, nil
	}
}

// nodeOf reads `node`, falling back to `omniview.node`.
func nodeOf(data map[string]any) string {
	if node, _ := data["node"].(string); node != "" {
		return node
	}
	if extra, ok := data["omniview"].(map[string]any); ok {
		node, _ := extra["node"].(string)
		return node
	}
	return ""
}

// intOf reads an integer resource field: int64 or uint64 from the resource
// body, or a decimal string.
func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case uint64:
		return int(n), n <= math.MaxInt
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	default:
		return 0, false
	}
}

// shellHandler returns at once and opens the console in the background:
// the host renders an ExecError sent on stopCh (title, message,
// suggestion) but only a generic message for an error returned here, so
// every failure, including a bad resource, is reported on stopCh.
func shellHandler(clients clientFunc, resolve targetFunc) sdkexec.TTYHandlerFunc {
	return func(
		ctx *types.PluginContext,
		opts sdkexec.SessionOptions,
		tty *os.File,
		stopCh chan error,
		resize <-chan sdkexec.SessionResizeInput,
	) error {
		if ctx == nil || ctx.Context == nil {
			return errors.New("no session context")
		}
		s := &shellSession{tty: tty, stopCh: stopCh, logger: ctx.Logger, done: make(chan struct{})}
		go s.watch(ctx.Context, resize)
		go s.connect(ctx, opts, clients, resolve)
		return nil
	}
}

// shellSession bridges a PTY and a termproxy terminal.
type shellSession struct {
	tty    *os.File
	stopCh chan error
	logger logging.Logger

	// target and started are set by connect before the pumps start.
	target  proxmox.TermTarget
	started time.Time

	// mu guards term (nil until connected), size (the latest resize
	// received before that) and the closing of done.
	mu   sync.Mutex
	term *proxmox.Terminal
	size *sdkexec.SessionResizeInput

	// head is the start of the output, owned by pumpOutput.
	head []byte

	once sync.Once
	done chan struct{}
}

// connect resolves the target, opens the console and starts the pumps; any
// failure ends the session with a classified ExecError.
func (s *shellSession) connect(ctx *types.PluginContext, opts sdkexec.SessionOptions, clients clientFunc, resolve targetFunc) {
	target, err := resolve(opts)
	if err != nil {
		s.finish(shellError(err, "Invalid resource", err.Error(),
			"Open the shell from a Proxmox node, virtual machine or container.", false))
		return
	}
	client, err := clients(ctx)
	if err != nil {
		s.finish(shellError(err, "Connection unavailable", err.Error(),
			"Check the cluster entry in the connection's config file.", true))
		return
	}

	setupCtx, cancel := context.WithTimeout(ctx.Context, setupTimeout)
	defer cancel()
	term, err := client.OpenTerminal(setupCtx, target)
	if err != nil {
		if ctx.Context.Err() != nil {
			// Cancelled while connecting: a cancel is not an open failure.
			s.finish(nil)
			return
		}
		s.finish(classifyOpenError(err, target, consoleAuth{ClusterID: client.ClusterID, ConsoleUser: client.ConsoleUser()}))
		return
	}

	s.target, s.started = target, time.Now()
	if !s.attach(term) {
		// Cancelled while connecting.
		_ = term.Close()
		return
	}
	go s.pumpOutput(term)
	go s.pumpInput(term)
}

// attach publishes term and applies a resize that arrived while
// connecting. It reports false once the session has finished.
func (s *shellSession) attach(term *proxmox.Terminal) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	s.term = term
	if s.size != nil {
		s.resize(term, *s.size)
	}
	return true
}

func (s *shellSession) resize(term *proxmox.Terminal, r sdkexec.SessionResizeInput) {
	if err := term.Resize(int(r.Cols), int(r.Rows)); err != nil && s.logger != nil {
		s.logger.Warnw(context.Background(), "resize terminal", "error", err)
	}
}

// finish ends the session once: it closes the websocket, if connected (a
// normal closure that stops the console), and reports err to the manager.
// stopCh holds one value and only finish sends, so it gets exactly one
// value and the send never blocks.
func (s *shellSession) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		close(s.done)
		term := s.term
		s.mu.Unlock()
		if term != nil {
			if cerr := term.Close(); cerr != nil && s.logger != nil {
				s.logger.Warnw(context.Background(), "close terminal websocket", "error", cerr)
			}
		}
		s.stopCh <- err
	})
}

// pumpOutput copies console output to the PTY; the console closing ends the
// session.
func (s *shellSession) pumpOutput(term *proxmox.Terminal) {
	buf := make([]byte, 32<<10)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			if len(s.head) < headBytes {
				s.head = append(s.head, buf[:min(n, headBytes-len(s.head))]...)
			}
			if _, werr := s.tty.Write(buf[:n]); werr != nil {
				// The manager closed the PTY: the session is over.
				s.finish(nil)
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.finish(s.exitError())
			} else {
				s.finish(shellError(err, "Connection lost", err.Error(),
					"The console websocket closed unexpectedly. Open the shell again.", true))
			}
			return
		}
	}
}

// pumpInput copies PTY input to the console. A failed send means the
// console is closing; pumpOutput reports how it ended.
func (s *shellSession) pumpInput(term *proxmox.Terminal) {
	buf := make([]byte, 4096)
	for {
		n, err := s.tty.Read(buf)
		if n > 0 {
			if _, werr := term.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			s.finish(nil)
			return
		}
	}
}

// watch forwards resizes (remembering the latest until connected) until
// the session ends or is cancelled. Cancellation ends a session that is
// still connecting too: OpenTerminal stops with the context and attach
// refuses the late terminal.
func (s *shellSession) watch(ctx context.Context, resize <-chan sdkexec.SessionResizeInput) {
	for {
		select {
		case <-ctx.Done():
			s.finish(nil)
			return
		case <-s.done:
			return
		case r, ok := <-resize:
			if !ok {
				resize = nil
				continue
			}
			s.mu.Lock()
			term := s.term
			if term == nil {
				s.size = &r
			}
			s.mu.Unlock()
			if term != nil {
				s.resize(term, r)
			}
		}
	}
}

// exitError explains a console that exited right after starting because
// the guest cannot be reached. Normal exits return nil.
func (s *shellSession) exitError() error {
	if s.target.GuestType == "" || time.Since(s.started) > earlyExit {
		return nil
	}
	head := bytes.ToLower(s.head)
	line := strings.TrimSpace(string(s.head))
	switch {
	case bytes.Contains(head, []byte("unable to find a serial interface")):
		return shellError(errors.New(line), "No serial port", line, fmt.Sprintf(
			"The shell uses the VM's serial console. Add one with `qm set %d -%s socket`, power-cycle the VM, "+
				"and run a getty on it in the guest (e.g. console=ttyS0).",
			s.target.VMID, s.target.Serial), false)
	case bytes.Contains(head, []byte("not running")):
		return shellError(errors.New(line), "Guest not running", line,
			"Start the guest, then open the shell again.", true)
	}
	return nil
}

// consoleAuth says how the client authenticates consoles: as ConsoleUser
// when set, otherwise with the API token, which PVE rejects.
type consoleAuth struct {
	ClusterID   string
	ConsoleUser string
}

// tokenSuggestion explains PVE bug 6079 and the fix: the node termproxy
// rejects token user ids outright, and guest termproxies drop the websocket
// after the handshake because pvedaemon cannot verify a token's ticket.
func (a consoleAuth) tokenSuggestion() string {
	return fmt.Sprintf("Proxmox VE consoles reject API tokens "+
		"(https://bugzilla.proxmox.com/show_bug.cgi?id=6079). Set console_user and console_password "+
		"(or console_password_file) for cluster %q in clusters.yaml.", a.ClusterID)
}

// classifyOpenError maps a console login, termproxy or websocket failure to
// an ExecError.
func classifyOpenError(err error, target proxmox.TermTarget, auth consoleAuth) error {
	tokenOnly := auth.ConsoleUser == ""
	switch {
	case errors.Is(err, proxmox.ErrConsoleTFA):
		return shellError(err, "Console user has two-factor auth", err.Error(),
			fmt.Sprintf("%s has two-factor authentication, which a console login cannot complete. "+
				"Use a user without TFA as console_user.", auth.ConsoleUser), false)
	case errors.Is(err, proxmox.ErrConsoleLogin) && proxmox.IsStatus(err, http.StatusUnauthorized):
		return shellError(err, "Console login failed", err.Error(),
			fmt.Sprintf("Proxmox VE rejected the password of %s. Check console_user and console_password "+
				"(or console_password_file) for cluster %q in clusters.yaml.", auth.ConsoleUser, auth.ClusterID), false)
	case errors.Is(err, proxmox.ErrConsoleLogin):
		return shellError(err, "Console login failed", err.Error(),
			fmt.Sprintf("Check console_user and console_password (or console_password_file) for cluster %q "+
				"in clusters.yaml.", auth.ClusterID), true)
	case errors.Is(err, proxmox.ErrTerminalHandshake) && tokenOnly:
		return shellError(err, "Console refused the API token",
			"termproxy closed the connection after the ticket handshake", auth.tokenSuggestion(), false)
	case errors.Is(err, proxmox.ErrTerminalHandshake):
		return shellError(err, "Terminal authentication failed", err.Error(),
			"termproxy rejected the console ticket. Open the shell again.", true)
	}

	var apiErr *proxmox.APIError
	if !errors.As(err, &apiErr) {
		return shellError(err, "Connection failed", err.Error(),
			"Could not open the console websocket on the Proxmox API.", true)
	}
	message := apiErr.Reason
	lower := strings.ToLower(message)
	switch {
	case apiErr.StatusCode == http.StatusForbidden:
		return shellError(err, "Permission denied", message, permissionSuggestion(target, auth), false)
	case apiErr.StatusCode == http.StatusUnauthorized && tokenOnly:
		return shellError(err, "Unauthorized", message,
			"The API token was rejected. Check token_id and token_secret.", false)
	case apiErr.StatusCode == http.StatusUnauthorized:
		return shellError(err, "Unauthorized", message,
			"The console ticket was rejected. Open the shell again.", true)
	case strings.Contains(lower, "does not look like a valid user name"):
		return shellError(err, "Console refused the API token", message, auth.tokenSuggestion(), false)
	case strings.Contains(lower, "not running"):
		return shellError(err, "Guest not running", message, "Start the guest, then open the shell again.", true)
	default:
		return shellError(err, "Console unavailable", message,
			"Proxmox VE could not start the console. Check the task log on the node.", true)
	}
}

func permissionSuggestion(target proxmox.TermTarget, auth consoleAuth) string {
	privilege, path, role := "VM.Console", fmt.Sprintf("/vms/%d", target.VMID), "PVEVMUser"
	if target.GuestType == "" {
		privilege, path, role = "Sys.Console", "/nodes/"+target.Node, "PVEAdmin"
	}
	if auth.ConsoleUser == "" {
		return fmt.Sprintf("The console needs %s on %s. %s", privilege, path, auth.tokenSuggestion())
	}
	return fmt.Sprintf("The console needs %s on %s: grant %s a role that has it, such as %s.",
		privilege, path, auth.ConsoleUser, role)
}

// shellError builds the structured error the UI shows. Its Error text
// carries the suggestion too, for callers that only see the string.
func shellError(cause error, title, message, suggestion string, retryable bool) *sdkexec.ExecError {
	return &sdkexec.ExecError{
		Err:        fmt.Errorf("%s: %w. %s", title, cause, suggestion),
		Title:      title,
		Message:    message,
		Suggestion: suggestion,
		Retryable:  retryable,
	}
}
