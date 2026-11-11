package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	requestTimeout = 30 * time.Second
	// maxResponseBytes bounds a single API response body.
	maxResponseBytes = 64 << 20
)

// ResourceType is the type filter accepted by GET /cluster/resources.
type ResourceType string

const (
	ResourceTypeVM      ResourceType = "vm" // qemu and lxc guests
	ResourceTypeNode    ResourceType = "node"
	ResourceTypeStorage ResourceType = "storage"
)

// Entry is one native API object. Raw is the unmodified object; ID is the
// field of it that identifies the object within its kind.
type Entry struct {
	ID  string
	Raw json.RawMessage
}

// ClusterResource is one /cluster/resources entry; Type routes it to a kind.
type ClusterResource struct {
	Entry
	Type string
}

// Version is the GET /version payload.
type Version struct {
	Version string `json:"version"`
	Release string `json:"release"`
	RepoID  string `json:"repoid"`
}

// APIError is a non-2xx response from the Proxmox API.
type APIError struct {
	StatusCode int
	// Reason is the HTTP reason phrase; Proxmox puts its error message there.
	Reason string
	// Errors holds Proxmox's per-parameter messages from a rejected request
	// (400 parameter verification failed), keyed by parameter name.
	Errors map[string]string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("proxmox api: %d %s", e.StatusCode, e.Reason)
	if len(e.Errors) == 0 {
		return msg
	}
	keys := make([]string, 0, len(e.Errors))
	for k := range e.Errors {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + strings.TrimSpace(e.Errors[k])
	}
	return msg + " (" + strings.Join(parts, "; ") + ")"
}

// Client is a minimal, token-authenticated Proxmox VE API client.
type Client struct {
	ClusterID string
	base      *url.URL
	auth      string
	// console holds the optional user login for console sessions, which
	// PVE does not open for API tokens.
	console consoleLogin
	http    *http.Client
	ceph    cephRoute
	storage guestStorageCache
}

// NewClient builds a client for cluster. The token secret lives only in the
// Authorization header and never appears in errors.
func NewClient(c Cluster) (*Client, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	base, err := c.apiBase()
	if err != nil {
		return nil, err
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipVerify} //nolint:gosec // explicit per-cluster opt-in
	if c.CAFile != "" {
		path, err := ExpandPath(c.CAFile)
		if err != nil {
			return nil, err
		}
		pem, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: read ca_file: %w", c.ID, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cluster %q: ca_file contains no PEM certificates", c.ID)
		}
		tlsCfg.RootCAs = pool
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg

	return &Client{
		ClusterID: c.ID,
		base:      base,
		auth:      "PVEAPIToken=" + c.TokenID + "=" + c.TokenSecret,
		console:   consoleLoginFor(c),
		http:      &http.Client{Transport: transport, Timeout: requestTimeout},
	}, nil
}

// Close releases idle connections.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Version returns the API server version; it doubles as the auth check.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.get(ctx, "/version", nil, &v)
	return v, err
}

// ClusterResources lists cluster-wide resources of one type.
func (c *Client) ClusterResources(ctx context.Context, typ ResourceType) ([]ClusterResource, error) {
	var raw []json.RawMessage
	if err := c.get(ctx, "/cluster/resources", url.Values{"type": {string(typ)}}, &raw); err != nil {
		return nil, err
	}
	out := make([]ClusterResource, 0, len(raw))
	for _, r := range raw {
		var head struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(r, &head); err != nil {
			return nil, fmt.Errorf("decode cluster resource: %w", err)
		}
		out = append(out, ClusterResource{Entry: Entry{ID: head.ID, Raw: r}, Type: head.Type})
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, into any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, into)
}

// post sends a form-encoded POST; into, if not nil, receives the data.
func (c *Client) post(ctx context.Context, path string, form url.Values, into any) error {
	return c.do(ctx, http.MethodPost, path, nil, form, into)
}

// put sends a form-encoded PUT; into, if not nil, receives the data.
func (c *Client) put(ctx context.Context, path string, form url.Values, into any) error {
	return c.do(ctx, http.MethodPut, path, nil, form, into)
}

// del sends a DELETE with query parameters; into, if not nil, receives the data.
func (c *Client) del(ctx context.Context, path string, query url.Values, into any) error {
	return c.do(ctx, http.MethodDelete, path, query, nil, into)
}

// do sends one API request with the token. form, when not nil, is the
// urlencoded body. into, when not nil, receives the response's data, which
// must be present; with a nil into the data is ignored (writes often answer
// null).
func (c *Client) do(ctx context.Context, method, path string, query, form url.Values, into any) error {
	return c.send(ctx, c.authorizeToken, method, path, query, form, into)
}

// endpoint returns the URL of an API path. Callers build paths from
// url.PathEscape'd segments, so path is taken as escaped: setting it as
// URL.Path would escape it again (a UPID's "!" would reach Proxmox as %21
// inside the segment and name no task).
func (c *Client) endpoint(path string) (url.URL, error) {
	u := *c.base
	raw := strings.TrimSuffix(u.EscapedPath(), "/") + path
	unescaped, err := url.PathUnescape(raw)
	if err != nil {
		return url.URL{}, fmt.Errorf("api path %q: %w", path, err)
	}
	u.Path, u.RawPath = unescaped, raw
	return u, nil
}

func (c *Client) authorizeToken(req *http.Request) {
	req.Header.Set("Authorization", c.auth)
}

// send is do with the request's credentials set by authorize. It records a
// span, a log line and metrics per call; none carries a body, header, query
// or provider error text.
func (c *Client) send(ctx context.Context, authorize func(*http.Request), method, path string, query, form url.Values, into any) error {
	route := apiRoute(path)
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("proxmox.api.route", route),
	}
	if c.ClusterID != "" {
		attrs = append(attrs, attribute.String("proxmox.cluster_id", c.ClusterID))
	}
	ctx, span := Tracer().Start(ctx, "proxmox.api.request", trace.WithAttributes(attrs...))
	start := time.Now()
	status, respBytes, err := c.exchange(ctx, authorize, method, path, query, form, into)
	elapsed := time.Since(start)

	if status > 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", status))
	}
	span.SetAttributes(attribute.Int64("response_bytes", respBytes))
	EndSpan(span, err)

	APICall(metricOperation(method), apiMetricOutcome(status, err), elapsed)

	fields := []any{
		"method", method,
		"route", route,
		"status", status,
		"duration_ms", durationMs(elapsed),
		"cluster_id", c.ClusterID,
	}
	if err != nil {
		outcome, class, _ := ClassifyError(err)
		fields = append(fields, "outcome", outcome, "error_class", class)
	}
	switch {
	case status >= 500:
		pkgLogger().Warnw(ctx, "proxmox api server error", fields...)
	case err != nil && warnClass(err):
		pkgLogger().Warnw(ctx, "proxmox api request failed", fields...)
	default:
		pkgLogger().Debugw(ctx, "proxmox api request", fields...)
	}
	return err
}

// exchange performs the request and returns the HTTP status (0 when no
// response arrived) and the bytes read from the response body.
func (c *Client) exchange(ctx context.Context, authorize func(*http.Request), method, path string, query, form url.Values, into any) (status int, respBytes int64, err error) {
	u, err := c.endpoint(path)
	if err != nil {
		return 0, 0, err
	}
	u.RawQuery = query.Encode()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, 0, err
	}
	authorize(req)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	counted := &countingReader{r: resp.Body}
	defer func() { respBytes = counted.n }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
		apiErr := &APIError{StatusCode: resp.StatusCode, Reason: reason}
		var failure struct {
			Errors map[string]string `json:"errors"`
		}
		if json.NewDecoder(io.LimitReader(counted, maxResponseBytes)).Decode(&failure) == nil {
			apiErr.Errors = failure.Errors
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(counted, maxResponseBytes))
		return status, 0, apiErr
	}

	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(io.LimitReader(counted, maxResponseBytes)).Decode(&envelope); err != nil {
		return status, 0, fmt.Errorf("decode %s response: %w", path, err)
	}
	if p, ok := into.(*paged); ok {
		p.total = envelope.Total
		into = p.into
	}
	if into == nil {
		return status, 0, nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return status, 0, fmt.Errorf("proxmox api: %s returned no data", path)
	}
	if err := json.Unmarshal(envelope.Data, into); err != nil {
		return status, 0, fmt.Errorf("decode %s data: %w", path, err)
	}
	return status, 0, nil
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// paged wraps a do target for endpoints that report the full result count in
// the envelope's "total" beside a page of data (e.g. syslog).
type paged struct {
	into  any
	total int
}
