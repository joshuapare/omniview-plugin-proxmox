package proxmox

import (
	"context"
	"errors"
	"net"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const TracerName = "omniview.plugin.proxmox"

func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// ClassifyError never exposes provider error text.
func ClassifyError(err error) (outcome, class string, status int) {
	if err == nil {
		return "success", "", 0
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", "canceled", 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "canceled", "deadline_exceeded", 0
	}
	var api *APIError
	var task *TaskError
	var ceph *CephUnavailableError
	var guest *GuestNotFoundError
	var network net.Error
	var urlError *url.Error
	switch {
	case errors.Is(err, ErrConsoleLogin), errors.Is(err, ErrConsoleTFA):
		return "failure", "console_auth", 0
	case errors.Is(err, ErrTerminalHandshake):
		return "failure", "terminal_handshake", 0
	case errors.As(err, &api):
		return "failure", "api", api.StatusCode
	case errors.As(err, &task):
		return "failure", "task", 0
	case errors.As(err, &ceph):
		return "failure", "ceph_unavailable", 0
	case errors.As(err, &guest):
		return "failure", "not_found", 0
	case errors.As(err, &network) && network.Timeout():
		return "canceled", "deadline_exceeded", 0
	case errors.As(err, &urlError), errors.As(err, &network):
		return "failure", "network", 0
	default:
		return "failure", "other", 0
	}
}

func EndSpan(span trace.Span, err error) {
	outcome, class, status := ClassifyError(err)
	span.SetAttributes(attribute.String("outcome", outcome))
	if class != "" {
		span.SetAttributes(attribute.String("error.class", class))
	}
	if status > 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", status))
	}
	if outcome == "failure" {
		span.SetStatus(codes.Error, class)
	}
	span.End()
}
