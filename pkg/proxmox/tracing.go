package proxmox

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	logging "github.com/omniviewdev/plugin-sdk/log"
)

// pkgLogger is the SDK process logger; the SDK installs it in NewPlugin.
func pkgLogger() logging.Logger { return logging.Default() }

func durationMs(d time.Duration) int64 { return d.Milliseconds() }

// apiRoute returns path (never a query) with every UPID segment replaced by
// {upid}, so routes stay low-cardinality and carry no task identity.
func apiRoute(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if len(seg) >= 7 && (strings.EqualFold(seg[:5], "UPID:") || strings.EqualFold(seg[:7], "UPID%3A")) {
			segments[i] = "{upid}"
		}
	}
	return strings.Join(segments, "/")
}

var taskTypePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// upidType returns the task type segment of a UPID
// (UPID:node:pid:pstart:starttime:type:id:user:), or "other".
func upidType(upid string) string {
	parts := strings.Split(upid, ":")
	if len(parts) > 5 && taskTypePattern.MatchString(parts[5]) {
		return parts[5]
	}
	return "other"
}

// metricOperation maps an HTTP method to the closed metric vocabulary.
func metricOperation(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read"
	case http.MethodDelete:
		return "delete"
	default:
		return "write"
	}
}

// apiMetricOutcome maps an API result to the closed metric vocabulary.
func apiMetricOutcome(status int, err error) string {
	switch {
	case err == nil:
		return "ok"
	case status >= 500:
		return "server_error"
	case status >= 400:
		return "client_error"
	default:
		return "failed"
	}
}

// classFields returns log fields describing err only by its classification.
func classFields(err error) []any {
	outcome, class, status := ClassifyError(err)
	kv := []any{"outcome", outcome}
	if class != "" {
		kv = append(kv, "error_class", class)
	}
	if status > 0 {
		kv = append(kv, "status", status)
	}
	return kv
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// warnClass reports whether a failed call is worth a Warn: timeouts and
// network errors, not caller cancellation or API client errors.
func warnClass(err error) bool {
	_, class, _ := ClassifyError(err)
	return class == "network" || class == "deadline_exceeded"
}
