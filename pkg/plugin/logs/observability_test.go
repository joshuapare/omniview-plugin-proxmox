package logs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	logging "github.com/omniviewdev/plugin-sdk/log"
	"github.com/omniviewdev/plugin-sdk/log/logtest"
	sdklogs "github.com/omniviewdev/plugin-sdk/pkg/v1/logs"
	"go.opentelemetry.io/otel/attribute"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const secretToken = "fake-secret-token-xyz"

func recordNamed(sink *logtest.RecordingSink, msg string) (logging.Record, bool) {
	for _, r := range sink.Records() {
		if r.Message == msg {
			return r, true
		}
	}
	return logging.Record{}, false
}

func fieldOf(r logging.Record, key string) any {
	for _, f := range r.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func spanAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestTaskFollowWarnsAfterConsecutiveFailures(t *testing.T) {
	exporter, sink := captureObs(t)
	var logCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case strings.HasSuffix(r.URL.Path, "/log"):
			if logCalls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"data":[{"n":1,"t":"starting"}]}`))
				return
			}
			http.Error(w, "boom "+secretToken, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: secretToken})
	if err != nil {
		t.Fatal(err)
	}
	tl := &taskLog{client: client, node: "pve1", upid: testUPID, interval: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	r, err := tl.stream(ctx, sdklogs.LogStreamRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, r) }()
	deadline := time.After(3 * time.Second)
	for {
		if _, ok := recordNamed(sink, "task log polling keeps failing"); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no Warn after consecutive failures: %+v calls=%d", sink.Records(), logCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	_ = r.Close()

	rec, _ := recordNamed(sink, "task log polling keeps failing")
	if rec.Level != logging.LevelWarn || fieldOf(rec, "consecutive_errors") != 5 || fieldOf(rec, "error_class") != "api" {
		t.Fatalf("unexpected record %+v", rec)
	}
	if _, ok := recordNamed(sink, "task log poll failed"); !ok {
		t.Error("earlier failures not logged at Debug")
	}
	span, ok := spanNamed(exporter, "proxmox.logs.task.open")
	if !ok {
		t.Fatal("no task open span")
	}
	if v, _ := spanAttr(span.Attributes, "outcome"); v.AsString() != "success" {
		t.Errorf("open outcome = %v", v)
	}
	if strings.Contains(dump(exporter, sink), secretToken) || strings.Contains(dump(exporter, sink), "boom") {
		t.Error("secret or provider text exported")
	}
}

func TestForbiddenJournalWarnsAndTracesClass(t *testing.T) {
	exporter, sink := captureObs(t)
	f, j := newJournal(t)
	f.forbidden = true
	r, err := j.stream(context.Background(), sdklogs.LogStreamRequest{TailLines: 10})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()

	rec, ok := recordNamed(sink, "journal not readable")
	if !ok || rec.Level != logging.LevelWarn || fieldOf(rec, "http_response_status_code") != 403 {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
	span, ok := spanNamed(exporter, "proxmox.logs.journal.open")
	if !ok {
		t.Fatal("no journal open span")
	}
	if v, _ := spanAttr(span.Attributes, "error.class"); v.AsString() != "api" {
		t.Errorf("error.class = %v", v)
	}
	if v, _ := spanAttr(span.Attributes, "pages"); v.AsInt64() < 1 {
		t.Errorf("pages = %v", v)
	}
}

func TestUnsupportedLogSourceIsLogged(t *testing.T) {
	_, sink := captureObs(t)
	if got := sourcesFor(keyCephOSD)("x", nil, sdklogs.LogSessionOptions{}); got != nil {
		t.Fatalf("sources = %v", got)
	}
	rec, ok := recordNamed(sink, "log source not supported")
	if !ok || fieldOf(rec, "kind") != keyCephOSD {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

func TestUnsupportedLogSourceDoesNotLogDaemonName(t *testing.T) {
	exporter, sink := captureObs(t)
	secretName := "osd.fake-secret-daemon-" + secretToken
	got := sourcesFor(keyCephOSD)("x", body(map[string]any{"host": "ares", "name": secretName}), sdklogs.LogSessionOptions{})
	if got != nil {
		t.Fatalf("sources = %v", got)
	}
	rec, ok := recordNamed(sink, "log source not supported")
	if !ok || fieldOf(rec, "error_class") != "invalid_resource" {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
	if out := dump(exporter, sink); strings.Contains(out, "fake-secret-daemon") || strings.Contains(out, secretToken) {
		t.Fatalf("daemon name leaked: %s", out)
	}
}

func TestClientFacingLogTextOmitsProviderError(t *testing.T) {
	exporter, sink := captureObs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider says "+secretToken, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: secretToken})
	if err != nil {
		t.Fatal(err)
	}

	j := &journal{client: client, node: "ares", loc: time.UTC, interval: 10 * time.Millisecond}
	r, err := j.stream(context.Background(), sdklogs.LogStreamRequest{TailLines: 5})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	_ = r.Close()
	if !strings.Contains(string(out), "cannot read the syslog of node ares (api, HTTP 400)") {
		t.Errorf("journal message = %q", out)
	}

	tl := &taskLog{client: client, node: "pve1", upid: testUPID, interval: 10 * time.Millisecond}
	r, err = tl.stream(context.Background(), sdklogs.LogStreamRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out2, _ := io.ReadAll(r)
	_ = r.Close()
	if !strings.Contains(string(out2), "(api, HTTP 400)") {
		t.Errorf("task message = %q", out2)
	}
	for _, text := range []string{string(out), string(out2), dump(exporter, sink)} {
		if strings.Contains(text, secretToken) || strings.Contains(text, "provider says") {
			t.Fatalf("provider text leaked: %q", text)
		}
	}
}
