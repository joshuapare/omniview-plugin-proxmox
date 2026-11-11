package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	logging "github.com/omniviewdev/plugin-sdk/log"
	"github.com/omniviewdev/plugin-sdk/log/logtest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

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

func dumpObs(exporter *tracetest.InMemoryExporter, sink *logtest.RecordingSink) string {
	out := ""
	for _, s := range exporter.GetSpans() {
		out += fmt.Sprint(s.Name, s.Attributes, s.Events, s.Status)
	}
	for _, r := range sink.Records() {
		out += fmt.Sprint(r.Message, r.Fields)
	}
	return out
}

func spanAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestAPIRouteReplacesUPID(t *testing.T) {
	got := apiRoute("/nodes/pve1/tasks/UPID:pve1:0001:0002:0003:qmstart:100:root@pam:/status")
	if got != "/nodes/pve1/tasks/{upid}/status" {
		t.Fatalf("route = %q", got)
	}
	if got := apiRoute("/nodes/pve1/tasks/UPID%3Apve1/log?start=1"); got != "/nodes/pve1/tasks/{upid}/log" {
		t.Fatalf("route = %q", got)
	}
	if upidType("UPID:pve1:0001:0002:0003:qmstart:100:root@pam:") != "qmstart" {
		t.Fatal("upid type")
	}
}

func TestSendSpanOnServerError(t *testing.T) {
	exporter, sink := captureObs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom-provider-text", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, err := NewClient(Cluster{ID: "c1", URL: srv.URL, TokenID: "root@pam!omni", TokenSecret: "fake-secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	upid := "UPID:pve1:0001:0002:0003:qmstart:100:root@pam:"
	err = c.get(context.Background(), "/nodes/pve1/tasks/"+upid+"/status?x=1", nil, &struct{}{})
	if !IsStatus(err, 500) {
		t.Fatalf("err = %v", err)
	}
	span, ok := func() (tracetest.SpanStub, bool) {
		for _, s := range exporter.GetSpans() {
			if s.Name == "proxmox.api.request" {
				return s, true
			}
		}
		return tracetest.SpanStub{}, false
	}()
	if !ok {
		t.Fatal("no proxmox.api.request span")
	}
	if v, _ := spanAttr(span.Attributes, "proxmox.api.route"); !strings.Contains(v.AsString(), "{upid}") || strings.Contains(v.AsString(), "UPID") {
		t.Fatalf("route attr = %q", v.AsString())
	}
	if v, _ := spanAttr(span.Attributes, "http.response.status_code"); v.AsInt64() != 500 {
		t.Fatalf("status attr = %v", v)
	}
	if v, _ := spanAttr(span.Attributes, "outcome"); v.AsString() != "failure" {
		t.Fatalf("outcome = %v", v)
	}
	warned := false
	for _, r := range sink.Records() {
		if r.Level == logging.LevelWarn {
			warned = true
		}
	}
	if !warned {
		t.Fatal("expected Warn for 500")
	}
	all := dumpObs(exporter, sink)
	for _, forbidden := range []string{"fake-secret-value", "PVEAPIToken", "boom-provider-text", "UPID:pve1"} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("telemetry leaked %q", forbidden)
		}
	}
}

func TestDialErrorDropsTicketURL(t *testing.T) {
	exporter, sink := captureObs(t)
	c := tlsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/termproxy") {
			_, _ = w.Write([]byte(`{"data":{"port":5900,"ticket":"vncticket-secret","user":"root@pam"}}`))
			return
		}
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	// The websocket path answers 502, so the dial is rejected.
	_, err := c.OpenTerminal(context.Background(), TermTarget{Node: "pve1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "vncticket") || strings.Contains(dumpObs(exporter, sink), "vncticket-secret") {
		t.Fatalf("leak: %v", err)
	}
	if _, ok := func() (bool, bool) {
		for _, s := range exporter.GetSpans() {
			if s.Name == "proxmox.terminal.open" {
				return true, true
			}
		}
		return false, false
	}(); !ok {
		t.Fatal("no proxmox.terminal.open span")
	}
}

func TestNewTerminalDialErrorClassifies(t *testing.T) {
	err := newTerminalDialError(context.Background(), fmt.Errorf("Get \"https://h/vncwebsocket?vncticket=secret\": %w", io.ErrUnexpectedEOF))
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("leak")
	}
	_, class, _ := ClassifyError(err)
	if class != "network" {
		t.Fatalf("class = %q", class)
	}
	_, class, _ = ClassifyError(newTerminalDialError(context.Background(), context.Canceled))
	if class != "canceled" {
		t.Fatalf("class = %q", class)
	}
	if !errors.Is(newTerminalDialError(context.Background(), context.DeadlineExceeded), context.DeadlineExceeded) {
		t.Fatal("deadline not preserved")
	}
}

func TestWaitTaskSpanExitClass(t *testing.T) {
	exporter, _ := captureObs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Cluster{ID: "c1", URL: srv.URL, TokenID: "root@pam!omni", TokenSecret: "s"})
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:0001:0002:0003:qmstart:100:root@pam:"); err != nil {
		t.Fatal(err)
	}
	for _, s := range exporter.GetSpans() {
		if s.Name == "proxmox.task.wait" {
			if v, _ := spanAttr(s.Attributes, "exit_class"); v.AsString() != "ok" {
				t.Fatalf("exit_class = %v", v)
			}
			if v, _ := spanAttr(s.Attributes, "proxmox.task.type"); v.AsString() != "qmstart" {
				t.Fatalf("task type = %v", v)
			}
			return
		}
	}
	t.Fatal("no proxmox.task.wait span")
}
