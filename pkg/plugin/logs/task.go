package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	sdklogs "github.com/omniviewdev/plugin-sdk/pkg/v1/logs"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// taskPageSize is the task log page read at a time.
const taskPageSize = 500

// taskWarnAfter is the number of consecutive failed follow polls that
// raises a Warn; fewer are Debug only.
const taskWarnAfter = 5

// taskSources builds the single source of a task: its log, by UPID.
func taskSources(resourceID string, _ *resourcev2.Value, _ sdklogs.LogSessionOptions) []sdklogs.LogSource {
	node, ok := proxmox.TaskNode(resourceID)
	if !ok {
		logger().Debugw(context.Background(), "log source not supported", "kind", keyTask, "reason", "invalid_upid")
		return nil
	}
	return []sdklogs.LogSource{{
		ID:     resourceID,
		Labels: map[string]string{labelNode: node, labelUPID: resourceID},
	}}
}

// taskHandler opens the log of the task taskSources labelled.
func taskHandler(ctx *types.PluginContext, req sdklogs.LogStreamRequest) (io.ReadCloser, error) {
	node, upid := req.Labels[labelNode], req.Labels[labelUPID]
	if node == "" || upid == "" {
		return nil, fmt.Errorf("log source %q is not a task", req.SourceID)
	}
	client, err := proxmox.ClientForConnection(ctx.Connection)
	if err != nil {
		logger().Warnw(ctx.Context, "task log client unavailable", append([]any{"node", node}, errFields(err)...)...)
		return nil, err
	}
	t := &taskLog{client: client, node: node, upid: upid, interval: pollInterval}
	return t.stream(ctx.Context, req)
}

// taskLog reads one task's log. Its lines carry no timestamps.
type taskLog struct {
	client   *proxmox.Client
	node     string
	upid     string
	interval time.Duration
}

// stream delivers the last TailLines lines (all when 0) and, with Follow,
// the new lines until the task has stopped and its log has no more; the
// followed reader then stays open, without lines, until ctx ends.
func (t *taskLog) stream(ctx context.Context, req sdklogs.LogStreamRequest) (io.ReadCloser, error) {
	openCtx, span := proxmox.Tracer().Start(ctx, "proxmox.logs.task.open", trace.WithAttributes(
		attribute.String("proxmox.node", t.node),
		attribute.Bool("follow", req.Follow),
		attribute.Int("tail_lines", int(req.TailLines)),
	))
	backlog, next, err := t.backlog(openCtx, int(req.TailLines))
	span.SetAttributes(attribute.Int("backlog_lines", len(backlog)))
	proxmox.EndSpan(span, err)
	if err != nil {
		if !permanent(err) {
			// transient: the SDK reconnects
			logger().Warnw(ctx, "task log backlog failed, retrying", append([]any{"node", t.node}, errFields(err)...)...)
			return nil, err
		}
		logger().Warnw(ctx, "task log not readable", append([]any{"node", t.node}, errFields(err)...)...)
		return pipe(ctx, req.Follow, func(w *lineWriter) {
			w.message(time.Now(), t.describe(err))
		}), nil
	}
	return pipe(ctx, req.Follow, func(w *lineWriter) {
		if !w.lines(backlog) || !req.Follow {
			return
		}
		var lineCount, polls, consecutive int
		lineCount = len(backlog)
		reason := "canceled"
		defer func() {
			logger().Infow(w.ctx, "task log follow ended", "node", t.node, "lines", lineCount, "polls", polls, "reason", reason)
		}()
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
			}
			// Read the status first: a task stopped before an empty read has
			// no lines left to come.
			polls++
			stopped, err := t.stopped(w.ctx)
			if err == nil {
				var lines []proxmox.TaskLogLine
				lines, err = t.client.TaskLog(w.ctx, t.node, t.upid, next, taskPageSize)
				if err == nil {
					consecutive = 0
					lines = withoutPlaceholder(lines, next)
					if len(lines) == 0 {
						if stopped {
							reason = "task_stopped"
							return
						}
						continue
					}
					next = int(lines[len(lines)-1].N)
					lineCount += len(lines)
					if !w.lines(taskLines(lines)) {
						reason = "reader_closed"
						return
					}
					continue
				}
			}
			if w.ctx.Err() != nil {
				return
			}
			consecutive++
			kv := append([]any{"node", t.node, "consecutive_errors", consecutive}, errFields(err)...)
			if consecutive == taskWarnAfter {
				logger().Warnw(w.ctx, "task log polling keeps failing", kv...)
			} else {
				logger().Debugw(w.ctx, "task log poll failed", kv...)
			}
		}
	}), nil
}

// backlog pages through the whole log and keeps its last tail lines (all
// when tail is 0). next is the start of the line after the last one read.
func (t *taskLog) backlog(ctx context.Context, tail int) (lines []parsedLine, next int, err error) {
	var all []proxmox.TaskLogLine
	for {
		page, err := t.client.TaskLog(ctx, t.node, t.upid, next, taskPageSize)
		if err != nil {
			return nil, 0, err
		}
		page = withoutPlaceholder(page, next)
		all = append(all, page...)
		if len(page) > 0 {
			next = int(page[len(page)-1].N)
		}
		if len(page) < taskPageSize {
			break
		}
	}
	if tail > 0 && len(all) > tail {
		all = all[len(all)-tail:]
	}
	return taskLines(all), next, nil
}

func (t *taskLog) stopped(ctx context.Context) (bool, error) {
	raw, err := t.client.TaskStatus(ctx, t.node, t.upid)
	if err != nil {
		return false, err
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return false, err
	}
	return status.Status == "stopped", nil
}

func (t *taskLog) describe(err error) string {
	if proxmox.IsStatus(err, http.StatusForbidden) {
		return fmt.Sprintf("cannot read the log of task %s: the API token needs Sys.Audit on /nodes/%s", t.upid, t.node)
	}
	return fmt.Sprintf("cannot read the log of task %s (%s)", t.upid, describeClass(err))
}

// withoutPlaceholder drops the single "no content" line Proxmox returns
// for an empty log read from its start.
func withoutPlaceholder(lines []proxmox.TaskLogLine, start int) []proxmox.TaskLogLine {
	if start == 0 && len(lines) == 1 && lines[0].N == 1 && lines[0].T == "no content" {
		return nil
	}
	return lines
}

func taskLines(lines []proxmox.TaskLogLine) []parsedLine {
	out := make([]parsedLine, len(lines))
	for i, l := range lines {
		out[i] = parsedLine{text: l.T}
	}
	return out
}
