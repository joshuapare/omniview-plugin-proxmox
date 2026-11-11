package metric

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	logging "github.com/omniviewdev/plugin-sdk/log"
	sdkmetric "github.com/omniviewdev/plugin-sdk/pkg/v1/metric"
	"go.opentelemetry.io/otel/attribute"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

func spanAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestQuerySpanOnSuccess(t *testing.T) {
	exporter, _ := captureObs(t)
	_, client := newFake(t)
	resp := run(context.Background(), client, kinds[keyNode], sdkmetric.QueryRequest{
		ResourceKey: keyNode, ResourceID: "node/ares", Shape: sdkmetric.ShapeCurrent,
	}, now)
	mustSucceed(t, resp)
	span, ok := spanNamed(exporter, "proxmox.metric.query")
	if !ok {
		t.Fatal("no query span")
	}
	for key, want := range map[string]string{"proxmox.kind": "node", "shape": "current", "outcome": "success"} {
		if v, _ := spanAttr(span.Attributes, key); v.AsString() != want {
			t.Errorf("%s = %v, want %s", key, v, want)
		}
	}
	if v, _ := spanAttr(span.Attributes, "points"); v.AsInt64() == 0 {
		t.Error("points not recorded")
	}
}

func TestQueryFailureIsClassifiedWithoutText(t *testing.T) {
	exporter, sink := captureObs(t)
	f, client := newFake(t)
	f.status = http.StatusForbidden
	resp := run(context.Background(), client, kinds[keyNode], sdkmetric.QueryRequest{ResourceKey: keyNode, ResourceID: "node/ares"}, now)
	if resp.Success {
		t.Fatal("expected failure")
	}
	span, _ := spanNamed(exporter, "proxmox.metric.query")
	if v, _ := spanAttr(span.Attributes, "outcome"); v.AsString() != "failure" {
		t.Errorf("outcome = %v", v)
	}
	if v, _ := spanAttr(span.Attributes, "error.class"); v.AsString() != "api" {
		t.Errorf("error.class = %v", v)
	}
	var found bool
	for _, r := range sink.Records() {
		if r.Message == "metric query failed" && r.Level == logging.LevelDebug {
			found = true
		}
	}
	if !found {
		t.Error("failure not logged")
	}
	if strings.Contains(dump(exporter, sink), "Sys.Audit") {
		t.Error("response error text leaked into telemetry")
	}
}

func TestGuestLookupScanEvent(t *testing.T) {
	exporter, _ := captureObs(t)
	_, client := newFake(t)
	mustSucceed(t, run(context.Background(), client, kinds[keyVM], sdkmetric.QueryRequest{
		ResourceKey: keyVM, ResourceID: "qemu/101",
	}, now))
	span, _ := spanNamed(exporter, "proxmox.metric.query")
	for _, e := range span.Events {
		if e.Name == "guest.lookup.scan" {
			return
		}
	}
	t.Fatal("no guest.lookup.scan event")
}

func TestClientFacingErrorOmitsProviderText(t *testing.T) {
	exporter, sink := captureObs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider says fake-provider-secret", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client, err := proxmox.NewClient(proxmox.Cluster{ID: "c", URL: srv.URL, TokenID: "root@pam!t", TokenSecret: "fake-provider-secret"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]sdkmetric.QueryRequest{
		"rrd":    {ResourceKey: keyNode, ResourceID: "node/ares"},
		"lookup": {ResourceKey: keyVM, ResourceID: "qemu/101"},
	}
	for name, req := range cases {
		k := kinds[req.ResourceKey]
		resp := run(context.Background(), client, k, req, now)
		if resp.Success {
			t.Fatalf("%s: expected failure", name)
		}
		if !strings.Contains(resp.Error, "api, HTTP 500") {
			t.Errorf("%s: error = %q, want the class", name, resp.Error)
		}
		if strings.Contains(resp.Error, "fake-provider-secret") || strings.Contains(resp.Error, "provider says") {
			t.Errorf("%s: provider text in response: %q", name, resp.Error)
		}
	}
	if strings.Contains(dump(exporter, sink), "fake-provider-secret") {
		t.Error("secret in telemetry")
	}
}
