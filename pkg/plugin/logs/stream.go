package logs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	sdklogs "github.com/omniviewdev/plugin-sdk/pkg/v1/logs"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const (
	// defaultTail is the backlog when the request sets neither tail nor since.
	defaultTail = 1000
	// pageSize is the syslog API page; journals are read in pages of it.
	pageSize = 1000
	// pollInterval is how often a followed journal is re-read.
	pollInterval = 2 * time.Second
	// clockMargin bounds the error of the node clock estimate (whole
	// seconds from /time plus request latency).
	clockMargin = 5 * time.Second
)

// tailWindows are the look-back windows tried, narrowest first, to find the
// last N lines. The syslog API runs journalctl over the whole window, and an
// unbounded node journal takes longer than a request may, so the widest
// window is the limit of the backlog.
var tailWindows = []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

// journal reads one node's journal, optionally restricted to a unit.
type journal struct {
	client *proxmox.Client
	node   string
	unit   string
	loc    *time.Location
	// skew is the node clock minus ours.
	skew     time.Duration
	interval time.Duration
	// pages counts the syslog requests made, for the open span.
	pages atomic.Int64
}

// syslog reads one syslog page and counts it.
func (j *journal) syslog(ctx context.Context, q proxmox.SyslogQuery) (proxmox.SyslogPage, error) {
	j.pages.Add(1)
	return j.client.Syslog(ctx, j.node, q, j.loc)
}

func (j *journal) now() time.Time { return time.Now().Add(j.skew).In(j.loc) }

// open starts streaming the journal for req. The reader yields one line per
// journal line, prefixed with its RFC 3339 timestamp. With req.Follow it
// stays open and delivers new lines until ctx ends or it is closed.
func open(ctx context.Context, client *proxmox.Client, node, unit string, req sdklogs.LogStreamRequest) (io.ReadCloser, error) {
	j := &journal{client: client, node: node, unit: unit, loc: time.Local, interval: pollInterval}
	// The journal's short timestamps are node-local wall clock without a
	// year; the node's zone and clock place them. Without /time (it needs
	// Sys.Audit) the local zone is the best guess.
	nt, err := client.NodeTime(ctx, node)
	if err == nil {
		j.loc, j.skew = nt.Location, time.Until(nt.Now)
	} else {
		logger().Debugw(ctx, "node time unavailable, using the local zone", append([]any{"node", node}, errFields(err)...)...)
	}
	return j.stream(ctx, req)
}

func (j *journal) stream(ctx context.Context, req sdklogs.LogStreamRequest) (io.ReadCloser, error) {
	started := j.now()
	openCtx, span := proxmox.Tracer().Start(ctx, "proxmox.logs.journal.open", trace.WithAttributes(
		attribute.String("proxmox.node", j.node),
		attribute.Bool("follow", req.Follow),
	))
	backlog, err := j.backlog(openCtx, req, started)
	span.SetAttributes(attribute.Int64("pages", j.pages.Load()), attribute.Int("backlog_lines", len(backlog)))
	proxmox.EndSpan(span, err)
	if err != nil {
		if !permanent(err) {
			// transient: the SDK reconnects
			logger().Warnw(ctx, "journal backlog failed, retrying", append([]any{"node", j.node}, errFields(err)...)...)
			return nil, err
		}
		logger().Warnw(ctx, "journal not readable", append([]any{"node", j.node}, errFields(err)...)...)
		// Reconnecting cannot fix a missing privilege, and an error returned
		// here never reaches the user, so say it in the log itself.
		return pipe(ctx, req.Follow, func(w *lineWriter) {
			w.message(j.now(), j.describe(err))
		}), nil
	}

	return pipe(ctx, req.Follow, func(w *lineWriter) {
		cur := &cursor{}
		if len(backlog) == 0 && j.unit != "" && req.SinceTime == nil && req.SinceSeconds <= 0 {
			// A daemon with no journal at all is a finding in itself (a down
			// OSD that never started here); an empty view would hide it.
			w.message(started, fmt.Sprintf("No journal entries for %s on %s in the last %d days.",
				j.unit, j.node, int(tailWindows[len(tailWindows)-1].Hours()/24)))
		}
		if !w.lines(cur.advance(backlog, j.parse)) {
			return
		}
		if !req.Follow {
			return
		}
		if cur.second.IsZero() {
			// Nothing was logged yet; start a little early to absorb the
			// error of the estimated node clock.
			cur.second = started.Add(-clockMargin).Truncate(time.Second)
		}
		ticker := time.NewTicker(j.interval)
		defer ticker.Stop()
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
			}
			lines, err := j.read(w.ctx, cur.second, 0)
			if err != nil {
				if w.ctx.Err() != nil {
					return
				}
				if permanent(err) {
					logger().Warnw(w.ctx, "journal not readable", append([]any{"node", j.node}, errFields(err)...)...)
					w.message(j.now(), j.describe(err))
					<-w.ctx.Done()
					return
				}
				logger().Debugw(w.ctx, "journal poll failed", append([]any{"node", j.node}, errFields(err)...)...)
				continue // transient: try again next tick
			}
			if !w.lines(cur.advance(lines, j.parse)) {
				return
			}
		}
	}), nil
}

// backlog returns the lines the session starts with: those since the
// requested time, or the last TailLines (default 1000), either capped to
// TailLines.
func (j *journal) backlog(ctx context.Context, req sdklogs.LogStreamRequest, now time.Time) ([]string, error) {
	tail := int(req.TailLines)
	var since time.Time
	switch {
	case req.SinceTime != nil && !req.SinceTime.IsZero():
		since = *req.SinceTime
	case req.SinceSeconds > 0:
		since = now.Add(-time.Duration(req.SinceSeconds) * time.Second)
	}
	if !since.IsZero() {
		return j.read(ctx, since, tail)
	}

	if tail <= 0 {
		tail = defaultTail
	}
	for i, w := range tailWindows {
		page, err := j.syslog(ctx, proxmox.SyslogQuery{Service: j.unit, Since: now.Add(-w), Limit: pageSize})
		if err != nil {
			return nil, err
		}
		if page.Total < tail && i < len(tailWindows)-1 {
			continue
		}
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("window", w.String()))
		return j.rest(ctx, now.Add(-w), page, tail)
	}
	return nil, nil
}

// read returns the lines since since (second resolution), the last tail of
// them if tail > 0.
func (j *journal) read(ctx context.Context, since time.Time, tail int) ([]string, error) {
	page, err := j.syslog(ctx, proxmox.SyslogQuery{Service: j.unit, Since: since, Limit: pageSize})
	if err != nil {
		return nil, err
	}
	return j.rest(ctx, since, page, tail)
}

// rest completes a read whose first page (from line 0) is first: it keeps
// that page when it covers the lines wanted and pages through the rest.
func (j *journal) rest(ctx context.Context, since time.Time, first proxmox.SyslogPage, tail int) ([]string, error) {
	start := 0
	if tail > 0 && first.Total > tail {
		start = first.Total - tail
	}
	var lines []string
	if start < len(first.Lines) {
		lines = append(lines, first.Lines[start:]...)
	}
	next := max(start, len(first.Lines))
	for next < first.Total {
		page, err := j.syslog(ctx, proxmox.SyslogQuery{Service: j.unit, Since: since, Start: next, Limit: pageSize})
		if err != nil {
			return nil, err
		}
		if len(page.Lines) == 0 {
			break
		}
		lines = append(lines, page.Lines...)
		next += len(page.Lines)
	}
	return lines, nil
}

func (j *journal) parse(line string) (time.Time, string, bool) {
	return proxmox.ParseSyslogLine(line, j.loc, j.now())
}

func (j *journal) describe(err error) string {
	switch {
	case proxmox.IsStatus(err, http.StatusForbidden):
		return fmt.Sprintf("cannot read the syslog of node %s: the API token needs Sys.Syslog on /nodes/%s", j.node, j.node)
	case proxmox.IsStatus(err, http.StatusUnauthorized):
		return fmt.Sprintf("cannot read the syslog of node %s: the API token was rejected", j.node)
	}
	return fmt.Sprintf("cannot read the syslog of node %s (%s)", j.node, describeClass(err))
}

// permanent reports errors that retrying will not fix.
func permanent(err error) bool {
	return proxmox.IsStatus(err, http.StatusForbidden) || proxmox.IsStatus(err, http.StatusUnauthorized) ||
		proxmox.IsStatus(err, http.StatusBadRequest)
}

// cursor remembers where a followed journal stands. The syslog API filters
// at second resolution, so each poll asks again for the newest second seen;
// seen holds that second's lines (with multiplicity) to drop the repeats.
type cursor struct {
	second time.Time
	seen   map[string]int
}

// advance takes the complete answer to a "since c.second" query (or the
// backlog) and returns its lines that were not delivered before, then moves
// the cursor to the newest second in it. Lines without a timestamp (boot
// markers, continuation lines) belong to the second of the line before.
func (c *cursor) advance(lines []string, parse func(string) (time.Time, string, bool)) []parsedLine {
	pending := c.seen
	c.seen = nil
	out := make([]parsedLine, 0, len(lines))
	keys := make([]time.Time, len(lines))
	key := c.second
	for i, l := range lines {
		ts, rest, ok := parse(l)
		if ok {
			key = ts.Truncate(time.Second)
		}
		keys[i] = key
		if key.Equal(c.second) && pending[l] > 0 {
			pending[l]--
			continue
		}
		pl := parsedLine{text: l}
		if ok {
			pl.ts, pl.text = ts, rest
		}
		out = append(out, pl)
	}

	if len(lines) == 0 {
		c.seen = pending
		return out
	}
	newest := c.second
	for _, k := range keys {
		if k.After(newest) {
			newest = k
		}
	}
	// The query returned every line from c.second on, so the lines of
	// the newest second here are all of that second's lines so far.
	c.second = newest
	c.seen = make(map[string]int)
	for i, k := range keys {
		if k.Equal(newest) {
			c.seen[lines[i]]++
		}
	}
	return out
}

type parsedLine struct {
	ts   time.Time // zero when the line has none
	text string
}

// lineWriter writes formatted lines into the pipe a reader drains.
type lineWriter struct {
	ctx context.Context
	w   *io.PipeWriter
	buf strings.Builder
}

// lines writes a batch; false means the reader is gone.
func (lw *lineWriter) lines(lines []parsedLine) bool {
	if len(lines) == 0 {
		return true
	}
	lw.buf.Reset()
	for _, l := range lines {
		if !l.ts.IsZero() {
			lw.buf.WriteString(l.ts.Format(time.RFC3339))
			lw.buf.WriteByte(' ')
		}
		lw.buf.WriteString(l.text)
		lw.buf.WriteByte('\n')
	}
	// A write error means the reader closed the pipe: the session is over,
	// which is expected and not worth a log.
	_, err := io.WriteString(lw.w, lw.buf.String())
	return err == nil
}

func (lw *lineWriter) message(now time.Time, msg string) {
	lw.lines([]parsedLine{{ts: now, text: "omniview: " + msg}})
}

// pipe runs produce in the background feeding the returned reader. In
// follow mode the reader stays open after produce returns until ctx ends or
// the reader is closed; otherwise it ends there.
func pipe(ctx context.Context, follow bool, produce func(*lineWriter)) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		defer cancel()
		// Ending ctx must end the read the SDK is blocked in.
		go func() {
			<-ctx.Done()
			_ = pw.Close() // pipe close on shutdown: an already closed pipe is expected
		}()
		produce(&lineWriter{ctx: ctx, w: pw})
		if follow {
			<-ctx.Done()
		}
		_ = pw.Close() // see above: closing an already closed pipe is expected
	}()
	return &pipeReader{PipeReader: pr, cancel: cancel}
}

type pipeReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *pipeReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}
