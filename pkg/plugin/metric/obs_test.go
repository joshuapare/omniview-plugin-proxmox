package metric

import (
	"context"
	"fmt"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	logging "github.com/omniviewdev/plugin-sdk/log"
	"github.com/omniviewdev/plugin-sdk/log/logtest"
	"go.opentelemetry.io/otel"
)

// captureObs installs an in-memory tracer provider and a debug-level
// default logger for the test.
func captureObs(t *testing.T) (*tracetest.InMemoryExporter, *logtest.RecordingSink) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prevProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)

	sink := &logtest.RecordingSink{}
	prevLogger := logging.Default()
	logging.SetDefault(logging.New(logging.Config{
		Backend: logging.NewSinkBackend(sink),
		Level:   logging.NewLevelController(logging.LevelDebug),
	}))
	t.Cleanup(func() {
		logging.SetDefault(prevLogger)
		otel.SetTracerProvider(prevProvider)
		_ = provider.Shutdown(context.Background())
	})
	return exporter, sink
}

// dump renders everything a span or log exposes, to search for secrets.
func dump(exporter *tracetest.InMemoryExporter, sink *logtest.RecordingSink) string {
	out := ""
	for _, s := range exporter.GetSpans() {
		out += fmt.Sprint(s.Name, s.Attributes, s.Events, s.Status)
	}
	for _, r := range sink.Records() {
		out += fmt.Sprint(r.Message, r.Fields)
	}
	return out
}

func spanNamed(exporter *tracetest.InMemoryExporter, name string) (tracetest.SpanStub, bool) {
	for _, s := range exporter.GetSpans() {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}
