package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	sdklogs "github.com/omniviewdev/plugin-sdk/pkg/v1/logs"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// taskPageSize is the task log page read at a time.
const taskPageSize = 500

// taskSources builds the single source of a task: its log, by UPID.
func taskSources(resourceID string, _ *resourcev2.Value, _ sdklogs.LogSessionOptions) []sdklogs.LogSource {
	node, ok := proxmox.TaskNode(resourceID)
	if !ok {
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
// the new lines until the task has stopped and its log has no more.
func (t *taskLog) stream(ctx context.Context, req sdklogs.LogStreamRequest) (io.ReadCloser, error) {
	backlog, next, err := t.backlog(ctx, int(req.TailLines))
	if err != nil {
		if !permanent(err) {
			return nil, err // transient: the SDK reconnects
		}
		return pipe(ctx, false, func(w *lineWriter) {
			w.message(time.Now(), t.describe(err))
		}), nil
	}
	return pipe(ctx, false, func(w *lineWriter) {
		if !w.lines(backlog) || !req.Follow {
			return
		}
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
			stopped, err := t.stopped(w.ctx)
			if err != nil {
				continue
			}
			lines, err := t.client.TaskLog(w.ctx, t.node, t.upid, next, taskPageSize)
			if err != nil {
				continue
			}
			if len(lines) == 0 {
				if stopped {
					return
				}
				continue
			}
			next = int(lines[len(lines)-1].N)
			if !w.lines(taskLines(lines)) {
				return
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
		return fmt.Sprintf("cannot read the log of task %s: the API token needs Sys.Audit on /nodes/%s (%v)", t.upid, t.node, err)
	}
	return fmt.Sprintf("cannot read the log of task %s: %v", t.upid, err)
}

func taskLines(lines []proxmox.TaskLogLine) []parsedLine {
	out := make([]parsedLine, len(lines))
	for i, l := range lines {
		out[i] = parsedLine{text: l.T}
	}
	return out
}
