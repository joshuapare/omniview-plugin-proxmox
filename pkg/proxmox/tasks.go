package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// taskPollInterval is how often WaitTask reads a task's status.
const taskPollInterval = time.Second

// TaskError is a Proxmox task that finished with a failure exit status.
type TaskError struct {
	UPID       string
	ExitStatus string
}

func (e *TaskError) Error() string {
	return "proxmox task failed: " + e.ExitStatus
}

// WaitTask blocks until the task UPID on node stops and returns a
// *TaskError unless it ended OK (or OK with warnings). Write endpoints of
// guests, Ceph daemons and the like answer with a UPID and run the work
// as a task; the call's own success only means the task was queued.
func (c *Client) WaitTask(ctx context.Context, node, upid string) (err error) {
	taskType := upidType(upid)
	ctx, span := Tracer().Start(ctx, "proxmox.task.wait")
	span.SetAttributes(
		attribute.String("proxmox.node", node),
		attribute.String("proxmox.task.type", taskType),
		attribute.String("proxmox.cluster_id", c.ClusterID),
	)
	polls := 0
	exitClass := "failed"
	start := time.Now()
	defer func() {
		span.SetAttributes(attribute.Int("polls", polls), attribute.String("exit_class", exitClass))
		EndSpan(span, err)
		TaskWait(exitClass)
		fields := []any{
			"node", node, "task_type", taskType, "polls", polls,
			"exit_class", exitClass, "duration_ms", durationMs(time.Since(start)),
		}
		if err != nil {
			pkgLogger().Debugw(ctx, "proxmox task wait ended", append(fields, classFields(err)...)...)
		} else {
			pkgLogger().Debugw(ctx, "proxmox task wait ended", fields...)
		}
	}()

	path := "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid) + "/status"
	ticker := time.NewTicker(taskPollInterval)
	defer ticker.Stop()
	for {
		var status struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		polls++
		if err := c.get(ctx, path, nil, &status); err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				exitClass = "timeout"
			case errors.Is(err, context.Canceled):
				exitClass = "canceled"
			}
			return fmt.Errorf("task %s status: %w", upid, err)
		}
		pkgLogger().Debugw(ctx, "proxmox task poll", "node", node, "task_type", taskType, "poll", polls, "state", pollState(status.Status))
		if status.Status == "stopped" {
			if status.ExitStatus == "OK" {
				exitClass = "ok"
				return nil
			}
			if strings.HasPrefix(status.ExitStatus, "WARNINGS") {
				exitClass = "warnings"
				return nil
			}
			exitClass = "failed"
			return &TaskError{UPID: upid, ExitStatus: status.ExitStatus}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				exitClass = "timeout"
			} else {
				exitClass = "canceled"
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollState maps a task status to a closed vocabulary for logs.
func pollState(status string) string {
	switch status {
	case "running", "stopped":
		return status
	default:
		return "other"
	}
}

// ClusterTasks lists the cluster's recent tasks with GET /cluster/tasks,
// identified by UPID.
func (c *Client) ClusterTasks(ctx context.Context) ([]Entry, error) {
	var raw []json.RawMessage
	if err := c.get(ctx, "/cluster/tasks", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(raw))
	for _, r := range raw {
		upid, err := stringField(r, "upid")
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{ID: upid, Raw: r})
	}
	return out, nil
}

// TaskStatus reads one task with GET /nodes/{node}/tasks/{upid}/status. It
// finds tasks too old for /cluster/tasks.
func (c *Client) TaskStatus(ctx context.Context, node, upid string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.get(ctx, taskPath(node, upid)+"/status", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// TaskLogLine is one line of a task log; N counts from 1.
type TaskLogLine struct {
	N int64  `json:"n"`
	T string `json:"t"`
}

// TaskLog reads limit lines of the task's log from line start (0-based)
// with GET /nodes/{node}/tasks/{upid}/log.
func (c *Client) TaskLog(ctx context.Context, node, upid string, start, limit int) ([]TaskLogLine, error) {
	query := url.Values{"start": {strconv.Itoa(start)}, "limit": {strconv.Itoa(limit)}}
	var lines []TaskLogLine
	if err := c.get(ctx, taskPath(node, upid)+"/log", query, &lines); err != nil {
		return nil, err
	}
	return lines, nil
}

// TaskNode returns the node of a UPID ("UPID:node:..."), false when upid is
// not one.
func TaskNode(upid string) (string, bool) {
	rest, ok := strings.CutPrefix(upid, "UPID:")
	if !ok {
		return "", false
	}
	node, _, ok := strings.Cut(rest, ":")
	if !ok || node == "" {
		return "", false
	}
	return node, true
}

func taskPath(node, upid string) string {
	return "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid)
}
