package resource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	logging "github.com/omniviewdev/plugin-sdk/log"
	"github.com/omniviewdev/plugin-sdk/log/logtest"
	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const providerLeak = "provider-secret-text"

// observe installs an in-memory tracer provider and a debug-level recording
// logger for one test.
func observe(t *testing.T) (*tracetest.SpanRecorder, *logtest.RecordingSink) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	sink := &logtest.RecordingSink{}
	previousLogger := logging.Default()
	logging.SetDefault(logging.New(logging.Config{
		Backend: logging.NewSinkBackend(sink),
		Level:   logging.NewLevelController(logging.LevelDebug),
	}))
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		logging.SetDefault(previousLogger)
		_ = provider.Shutdown(context.Background())
	})
	return recorder, sink
}

func endedSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span %q among %d spans", name, len(recorder.Ended()))
	return nil
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func assertNoLeak(t *testing.T, recorder *tracetest.SpanRecorder, sink *logtest.RecordingSink, secrets ...string) {
	t.Helper()
	var all strings.Builder
	for _, s := range recorder.Ended() {
		fmt.Fprint(&all, s.Name(), s.Attributes(), s.Events(), s.Status())
	}
	for _, r := range sink.Records() {
		fmt.Fprint(&all, r.Message, r.Fields)
	}
	for _, secret := range secrets {
		if strings.Contains(all.String(), secret) {
			t.Fatalf("telemetry contains %q", secret)
		}
	}
}

func findLog(sink *logtest.RecordingSink, level logging.Level, message string) (logging.Record, bool) {
	for _, r := range sink.Records() {
		if r.Level == level && r.Message == message {
			return r, true
		}
	}
	return logging.Record{}, false
}

func logField(r logging.Record, key string) any {
	for _, f := range r.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func TestWriteFailureSpanIsClassifiedWithoutProviderText(t *testing.T) {
	recorder, sink := observe(t)
	f, client := newFakePVE(t)
	f.writeCode = http.StatusInternalServerError
	f.writeBody = `{"data":null,"errors":{"vmid":"` + providerLeak + `"}}`

	if _, err := act(vms, client, "start", "qemu/100"); err == nil {
		t.Fatal("expected the write to fail")
	}

	span := endedSpan(t, recorder, "proxmox.action.start")
	for key, want := range map[string]string{
		"proxmox.kind": typeVM, "outcome": "failure", "error.class": "api", "privilege": privPowerMgmt,
	} {
		if got, ok := spanAttr(span, key); !ok || got.AsString() != want {
			t.Errorf("%s = %v (present %v), want %q", key, got, ok, want)
		}
	}
	if got, ok := spanAttr(span, "proxmox.vmid"); !ok || got.AsInt64() != 100 {
		t.Errorf("proxmox.vmid = %v", got)
	}
	if got, ok := spanAttr(span, "proxmox.node"); !ok || got.AsString() != "pve2" {
		t.Errorf("proxmox.node = %v", got)
	}
	record, ok := findLog(sink, logging.LevelError, "proxmox write failed")
	if !ok || logField(record, "error_class") != "api" {
		t.Fatalf("missing classified error log: %+v", sink.Records())
	}
	assertNoLeak(t, recorder, sink, providerLeak, "root@pam!t")
}

func TestWriteSuccessLogsAcceptedTask(t *testing.T) {
	recorder, sink := observe(t)
	_, client := newFakePVE(t)

	if _, err := act(vms, client, "start", "qemu/100"); err != nil {
		t.Fatal(err)
	}

	span := endedSpan(t, recorder, "proxmox.action.start")
	if got, _ := spanAttr(span, "outcome"); got.AsString() != "success" {
		t.Fatalf("outcome = %v", got)
	}
	accepted, ok := findLog(sink, logging.LevelInfo, "proxmox task accepted")
	if !ok || logField(accepted, "task_type") != "other" {
		t.Fatalf("missing task accepted log (UPID without a type segment): %+v", sink.Records())
	}
	if _, ok := findLog(sink, logging.LevelInfo, "proxmox write finished"); !ok {
		t.Fatal("missing write finished log")
	}
}

func TestUnknownActionNamesStayBounded(t *testing.T) {
	recorder, _ := observe(t)
	_, client := newFakePVE(t)

	_, _ = act(vms, client, "not-an-action-"+providerLeak, "qemu/100")

	endedSpan(t, recorder, "proxmox.action.unknown")
	assertNoLeak(t, recorder, &logtest.RecordingSink{}, providerLeak)
}

func TestTaskStillRunningWarns(t *testing.T) {
	_, sink := observe(t)
	f, client := newFakePVE(t)
	f.taskStatus = "running"
	previous := taskWaitBound
	taskWaitBound = 50 * time.Millisecond
	t.Cleanup(func() { taskWaitBound = previous })

	if _, err := act(vms, client, "start", "qemu/100"); err != nil {
		t.Fatal(err)
	}
	if _, ok := findLog(sink, logging.LevelWarn, "proxmox task still running after wait bound"); !ok {
		t.Fatalf("missing wait bound warning: %+v", sink.Records())
	}
}

func TestListSpanCarriesKindAndEntries(t *testing.T) {
	recorder, _ := observe(t)
	_, client := newFakePVE(t)

	entries, err := vms.fetch(ctx, client)
	if err != nil {
		t.Fatal(err)
	}

	span := endedSpan(t, recorder, "proxmox.list")
	if got, _ := spanAttr(span, "entries"); got.AsInt64() != int64(len(entries)) {
		t.Fatalf("entries = %v, want %d", got, len(entries))
	}
	if got, ok := spanAttr(span, "proxmox.kind"); !ok || got.AsString() != typeVM {
		t.Fatalf("proxmox.kind = %v", got)
	}
}

func TestSourceTerminationWarnsAndClassifies(t *testing.T) {
	recorder, sink := observe(t)
	k := &kind{typ: "qemu", list: func(context.Context, *proxmox.Client) ([]proxmox.Entry, error) {
		return nil, errors.New(providerLeak)
	}}
	emit := &recordingSource{}
	p := pollSource{kind: k, emit: emit}

	if err := p.poll(ctx, true); err != nil {
		t.Fatal(err)
	}

	if emit.reset != resourcev2.SourceResetSourceInvalidated || !p.terminal {
		t.Fatalf("reset %q terminal %v", emit.reset, p.terminal)
	}
	record, ok := findLog(sink, logging.LevelWarn, "source terminated")
	if !ok || logField(record, "error_class") != "other" {
		t.Fatalf("missing source terminated warning: %+v", sink.Records())
	}
	span := endedSpan(t, recorder, "proxmox.source.poll")
	terminal := false
	for _, e := range span.Events() {
		terminal = terminal || e.Name == "source.terminal"
	}
	if !terminal {
		t.Fatal("missing source.terminal span event")
	}
	if got, _ := spanAttr(span, "initial"); !got.AsBool() {
		t.Fatal("initial attribute missing")
	}
	assertNoLeak(t, recorder, sink, providerLeak)
}

func TestConnectionCheckFailureIsClassifiedWithoutProviderText(t *testing.T) {
	recorder, sink := observe(t)
	f, client := newFakePVE(t)
	f.writeCode = http.StatusInternalServerError
	f.writeBody = `{"errors":{"x":"` + providerLeak + `"}}`

	status, err := checkConnection(ctx, types.Connection{ID: "c"}, client)
	if err != nil {
		t.Fatal(err)
	}

	span := endedSpan(t, recorder, "proxmox.connection.check")
	if got, _ := spanAttr(span, "error.class"); got.AsString() != "api" {
		t.Fatalf("error.class = %v", got)
	}
	if got, _ := spanAttr(span, "status"); got.AsString() != string(status.Status) {
		t.Fatalf("status = %v", got)
	}
	if strings.Contains(status.Details, providerLeak) {
		t.Fatalf("status details contain provider text: %q", status.Details)
	}
	assertNoLeak(t, recorder, sink, providerLeak)
}

func TestClassifyConnectionErrorDefaultOmitsErrorText(t *testing.T) {
	failure := classifyConnectionError(errors.New(providerLeak))
	if strings.Contains(failure.details, providerLeak) || !strings.Contains(failure.details, "other") {
		t.Fatalf("details: %q", failure.details)
	}
}

func TestMissingConfigFileIsLogged(t *testing.T) {
	_, sink := observe(t)

	conns, err := loadConnections(ctx, []string{"/nonexistent/clusters.yaml"})
	if err != nil || len(conns) != 0 {
		t.Fatalf("conns %v err %v", conns, err)
	}
	record, ok := findLog(sink, logging.LevelDebug, "proxmox config file missing")
	if !ok {
		t.Fatal("missing config file was not logged")
	}
	if strings.Contains(fmt.Sprint(record.Fields), "nonexistent") {
		t.Fatal("log carries the config path")
	}
}
