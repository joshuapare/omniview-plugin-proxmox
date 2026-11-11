package metric

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	logging "github.com/omniviewdev/plugin-sdk/log"
	sdkmetric "github.com/omniviewdev/plugin-sdk/pkg/v1/metric"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// queryTimeout bounds one query, which may take two API calls.
const queryTimeout = 30 * time.Second

func logger() logging.Logger { return logging.Default().Named("proxmox.metric") }

// errFields classifies err for a log line; it never carries the error text,
// which may come from the Proxmox API.
func errFields(err error) []any {
	outcome, class, status := proxmox.ClassifyError(err)
	kv := []any{"outcome", outcome, "error_class", class}
	if status > 0 {
		kv = append(kv, "http_response_status_code", status)
	}
	return kv
}

// Closed reasons of a failed query, for logs and spans.
const (
	reasonUnknownKind  = "unknown_kind"
	reasonNoConnection = "no_connection"
	reasonClient       = "client"
	reasonBadSeries    = "bad_series"
	reasonBadShape     = "bad_shape"
	reasonTarget       = "target"
	reasonAPI          = "api"
)

func query(qctx *sdkmetric.QueryContext, req sdkmetric.QueryRequest) (*sdkmetric.QueryResponse, error) {
	ctx := context.Background()
	k, ok := kinds[req.ResourceKey]
	if !ok {
		return failure(ctx, reasonUnknownKind, nil, "no Proxmox metrics for resource %s", req.ResourceKey), nil
	}
	if qctx == nil || qctx.Connection == nil {
		return failure(ctx, reasonNoConnection, nil, "no connection in metric query"), nil
	}
	client, err := proxmox.ClientForConnection(qctx.Connection)
	if err != nil {
		return failure(ctx, reasonClient, err, "cannot create the Proxmox API client (%s)", describeClass(err)), nil
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return run(ctx, client, k, req, time.Now()), nil
}

// queryStats is what a query learned, for its span.
type queryStats struct {
	series, points int
	// reason and err describe a failure: err is the API error when there
	// is one, reason the closed class otherwise.
	reason string
	err    error
}

// run answers req for one resource of kind k; now anchors the RRD windows.
func run(ctx context.Context, client *proxmox.Client, k kind, req sdkmetric.QueryRequest, now time.Time) *sdkmetric.QueryResponse {
	kindName := "node"
	if k.guestType != "" {
		kindName = k.guestType
	}
	shape := "other"
	switch req.Shape {
	case sdkmetric.ShapeCurrent:
		shape = "current"
	case sdkmetric.ShapeTimeseries:
		shape = "timeseries"
	}
	ctx, span := proxmox.Tracer().Start(ctx, "proxmox.metric.query", trace.WithAttributes(
		attribute.String("proxmox.kind", kindName),
		attribute.String("shape", shape),
	))
	st := &queryStats{}
	resp := execute(ctx, client, k, req, now, st)
	span.SetAttributes(attribute.Int("series", st.series), attribute.Int("points", st.points))
	switch {
	case resp.Success:
		proxmox.EndSpan(span, nil)
	case st.err != nil:
		proxmox.EndSpan(span, st.err)
	default:
		span.SetAttributes(attribute.String("outcome", "failure"), attribute.String("error.class", st.reason))
		span.SetStatus(codes.Error, st.reason)
		span.End()
	}
	return resp
}

func execute(ctx context.Context, client *proxmox.Client, k kind, req sdkmetric.QueryRequest, now time.Time, st *queryStats) *sdkmetric.QueryResponse {
	fail := func(reason string, err error, format string, args ...any) *sdkmetric.QueryResponse {
		st.reason, st.err = reason, err
		return failure(ctx, reason, err, format, args...)
	}
	wanted, err := selectSeries(k.series, req.MetricIDs)
	if err != nil {
		return fail(reasonBadSeries, nil, "%v", err)
	}
	st.series = len(wanted)

	var start, end time.Time
	frame := proxmox.RRDHour
	switch req.Shape {
	case sdkmetric.ShapeCurrent:
	case sdkmetric.ShapeTimeseries:
		start, end = req.StartTime, req.EndTime
		if end.IsZero() || end.After(now) {
			end = now
		}
		if start.IsZero() {
			start = end.Add(-time.Hour)
		}
		frame = proxmox.TimeframeFor(now.Sub(start))
	default:
		return fail(reasonBadShape, nil, "metric shape %d is not supported", req.Shape)
	}

	t, err := resolveTarget(ctx, client, k, req)
	if err != nil {
		return fail(reasonTarget, err, "%s", err)
	}
	var points []proxmox.RRDPoint
	if k.guestType == "" {
		points, err = client.NodeRRD(ctx, t.node, frame)
	} else {
		points, err = client.GuestRRD(ctx, t.node, k.guestType, t.vmid, frame)
	}
	if err != nil {
		return fail(reasonAPI, err, "%s", describeError(err, k, t))
	}
	st.points = len(points)

	results := make([]sdkmetric.MetricResult, 0, len(wanted))
	for _, s := range wanted {
		if req.Shape == sdkmetric.ShapeCurrent {
			if cv := latest(points, s); cv != nil {
				results = append(results, sdkmetric.MetricResult{CurrentValue: cv})
			}
			continue
		}
		results = append(results, sdkmetric.MetricResult{TimeSeries: timeseries(points, s, start, end)})
	}
	return &sdkmetric.QueryResponse{Success: true, Results: results}
}

// selectSeries returns the requested series in request order; no IDs means
// all of them. Unknown IDs are an error only when nothing else matched.
func selectSeries(all []series, ids []string) ([]series, error) {
	if len(ids) == 0 {
		return all, nil
	}
	out := make([]series, 0, len(ids))
	for _, id := range ids {
		for _, s := range all {
			if s.desc.ID == id {
				out = append(out, s)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("unknown metrics %s", strings.Join(ids, ", "))
	}
	return out, nil
}

func timeseries(points []proxmox.RRDPoint, s series, start, end time.Time) *sdkmetric.TimeSeries {
	ts := &sdkmetric.TimeSeries{MetricID: s.desc.ID, DataPoints: make([]sdkmetric.DataPoint, 0, len(points))}
	for _, p := range points {
		if p.Time.Before(start) || p.Time.After(end) {
			continue
		}
		if v, ok := p.Values[s.field]; ok {
			ts.DataPoints = append(ts.DataPoints, sdkmetric.DataPoint{Timestamp: p.Time, Value: v * s.scale})
		}
	}
	return ts
}

// latest is the newest recorded sample of s, or nil if there is none (e.g.
// a guest that has not run during the window).
func latest(points []proxmox.RRDPoint, s series) *sdkmetric.CurrentValue {
	for i := len(points) - 1; i >= 0; i-- {
		if v, ok := points[i].Values[s.field]; ok {
			return &sdkmetric.CurrentValue{MetricID: s.desc.ID, Value: v * s.scale, Timestamp: points[i].Time}
		}
	}
	return nil
}

// target is the node, and for guests the vmid, a query reads.
type target struct {
	node string
	vmid int
}

// resolveTarget reads the node and vmid from the resource object (a
// /cluster/resources entry), falling back to the resource ID ("node/apollo",
// "qemu/100") and, for guests without a node, a /cluster/resources lookup.
func resolveTarget(ctx context.Context, client *proxmox.Client, k kind, req sdkmetric.QueryRequest) (target, error) {
	data := proxmox.ResourceData(req.ResourceData)
	id := stringField(data, "id")
	if id == "" {
		id = req.ResourceID
	}

	if k.guestType == "" {
		node := stringField(data, "node")
		if node == "" {
			node = strings.TrimPrefix(id, "node/")
		}
		if node == "" {
			return target{}, fmt.Errorf("cannot tell which node to query: no node in the resource")
		}
		return target{node: node}, nil
	}

	vmid, ok := intField(data, "vmid")
	if !ok {
		n, err := strconv.Atoi(strings.TrimPrefix(id, k.guestType+"/"))
		if err != nil {
			return target{}, fmt.Errorf("cannot tell which guest to query from resource %q", id)
		}
		vmid = n
	}
	t := target{node: stringField(data, "node"), vmid: vmid}
	if t.node != "" {
		return t, nil
	}

	guests, err := client.ClusterResources(ctx, proxmox.ResourceTypeVM)
	if err != nil {
		return target{}, &lookupError{op: fmt.Sprintf("locate %s/%d", k.guestType, vmid), cause: err}
	}
	want := fmt.Sprintf("%s/%d", k.guestType, vmid)
	trace.SpanFromContext(ctx).AddEvent("guest.lookup.scan", trace.WithAttributes(attribute.Int("scanned", len(guests))))
	for _, g := range guests {
		if g.ID != want {
			continue
		}
		var entry struct {
			Node string `json:"node"`
		}
		if err := json.Unmarshal(g.Raw, &entry); err != nil {
			return target{}, &lookupError{op: "decode " + want, cause: err}
		}
		t.node = entry.Node
		return t, nil
	}
	return target{}, fmt.Errorf("guest %s not found in the cluster", want)
}

// describeClass names a failure without its error text, which comes from the
// Proxmox API.
func describeClass(err error) string {
	_, class, status := proxmox.ClassifyError(err)
	if status > 0 {
		return fmt.Sprintf("%s, HTTP %d", class, status)
	}
	return class
}

// lookupError is a failed API lookup whose text carries the class only.
type lookupError struct {
	op    string
	cause error
}

func (e *lookupError) Error() string { return e.op + " failed (" + describeClass(e.cause) + ")" }
func (e *lookupError) Unwrap() error { return e.cause }

// describeError names the privilege a 403 is missing; other failures show
// their class only.
func describeError(err error, k kind, t target) string {
	if proxmox.IsStatus(err, http.StatusForbidden) {
		if k.guestType == "" {
			return fmt.Sprintf("the API token needs Sys.Audit on /nodes/%s to read metrics", t.node)
		}
		return fmt.Sprintf("the API token needs VM.Audit on /vms/%d to read metrics", t.vmid)
	}
	return "reading metrics failed (" + describeClass(err) + ")"
}

// failure builds the failed response and logs its classification; the text
// goes to the caller only, never to the log.
func failure(ctx context.Context, reason string, err error, format string, args ...any) *sdkmetric.QueryResponse {
	kv := []any{"reason", reason}
	if err != nil {
		kv = append(kv, errFields(err)...)
	}
	logger().Debugw(ctx, "metric query failed", kv...)
	return &sdkmetric.QueryResponse{Success: false, Error: fmt.Sprintf(format, args...)}
}

func stringField(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}

// intField reads an integer resource field: int64 or uint64 from the
// resource body, or a decimal string.
func intField(data map[string]any, key string) (int, bool) {
	switch v := data[key].(type) {
	case int64:
		return int(v), true
	case uint64:
		return int(v), v <= math.MaxInt
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	return 0, false
}
