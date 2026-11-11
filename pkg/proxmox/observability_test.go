package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestEndSpanClassifiesWithoutErrorText(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	_, span := provider.Tracer("test").Start(context.Background(), "test")
	EndSpan(span, errors.New("token=abc123"))
	ended := recorder.Ended()[0]
	if ended.Status().Description != "other" {
		t.Fatalf("unexpected status: %v", ended.Status())
	}
	if strings.Contains(fmt.Sprint(ended.Attributes(), ended.Events(), ended.Status()), "abc123") {
		t.Fatal("span contains secret")
	}
	outcome, class, status := ClassifyError(&APIError{StatusCode: 500})
	if outcome != "failure" || class != "api" || status != 500 {
		t.Fatalf("unexpected classification: %s %s %d", outcome, class, status)
	}
}
