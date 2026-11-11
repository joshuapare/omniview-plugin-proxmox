package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Ceph state is cluster-wide, but Proxmox serves it only under
// /nodes/{node}/ceph/, and only from nodes that have Ceph installed and
// initialised. cephGet asks the node that answered last, then every other
// online node, so a cluster where only some nodes run Ceph still works and
// the steady state costs one request.

// CephUnavailableError means no online node answered a Ceph request: Ceph is
// not installed or initialised on any of them, or its monitors are down.
type CephUnavailableError struct {
	// Errs holds one error per node tried, prefixed with the node name.
	Errs []error
}

func (e *CephUnavailableError) Error() string {
	if len(e.Errs) == 0 {
		return "ceph unavailable: no cluster node is online"
	}
	msgs := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		msgs[i] = err.Error()
	}
	return "ceph unavailable: " + strings.Join(msgs, "; ")
}

func (e *CephUnavailableError) Unwrap() []error { return e.Errs }

// cephRoute remembers the node that last answered a Ceph request.
type cephRoute struct {
	mu   sync.Mutex
	node string
}

func (r *cephRoute) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node
}

func (r *cephRoute) set(node string) {
	r.mu.Lock()
	r.node = node
	r.mu.Unlock()
}

// CephStatus returns the cluster status (`ceph status`) as one entry
// identified by the cluster fsid.
func (c *Client) CephStatus(ctx context.Context) ([]Entry, error) {
	var raw json.RawMessage
	if err := c.cephGet(ctx, "status", &raw); err != nil {
		return nil, err
	}
	id, err := stringField(raw, "fsid")
	if err != nil {
		return nil, err
	}
	return []Entry{{ID: id, Raw: raw}}, nil
}

// CephPools lists pools, identified by pool_name.
func (c *Client) CephPools(ctx context.Context) ([]Entry, error) {
	return c.cephList(ctx, "pool", "pool_name")
}

// CephMonitors lists monitors, identified by name.
func (c *Client) CephMonitors(ctx context.Context) ([]Entry, error) {
	return c.cephList(ctx, "mon", "name")
}

// CephManagers lists managers, identified by name.
func (c *Client) CephManagers(ctx context.Context) ([]Entry, error) {
	return c.cephList(ctx, "mgr", "name")
}

// CephMetadataServers lists MDS daemons, identified by name.
func (c *Client) CephMetadataServers(ctx context.Context) ([]Entry, error) {
	return c.cephList(ctx, "mds", "name")
}

// CephFilesystems lists CephFS file systems, identified by name.
func (c *Client) CephFilesystems(ctx context.Context) ([]Entry, error) {
	return c.cephList(ctx, "fs", "name")
}

// CephOSDs lists the OSD leaves of the CRUSH tree, identified by name
// (osd.N). Each entry is the unmodified leaf, including the `host` Proxmox
// adds for OSDs under a host bucket.
func (c *Client) CephOSDs(ctx context.Context) ([]Entry, error) {
	var tree struct {
		Root json.RawMessage `json:"root"`
	}
	if err := c.cephGet(ctx, "osd", &tree); err != nil {
		return nil, err
	}
	out := make([]Entry, 0)
	if err := collectOSDs(tree.Root, map[string]bool{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func collectOSDs(raw json.RawMessage, seen map[string]bool, out *[]Entry) error {
	if len(raw) == 0 {
		return nil
	}
	var node struct {
		Type     string            `json:"type"`
		Name     string            `json:"name"`
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("decode ceph osd tree: %w", err)
	}
	// A custom CRUSH map can place one OSD under several buckets.
	if node.Type == "osd" && node.Name != "" && !seen[node.Name] {
		seen[node.Name] = true
		*out = append(*out, Entry{ID: node.Name, Raw: raw})
	}
	for _, child := range node.Children {
		if err := collectOSDs(child, seen, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) cephList(ctx context.Context, sub, idField string) ([]Entry, error) {
	var raw []json.RawMessage
	if err := c.cephGet(ctx, sub, &raw); err != nil {
		return nil, err
	}
	out := make([]Entry, len(raw))
	for i, r := range raw {
		id, err := stringField(r, idField)
		if err != nil {
			return nil, err
		}
		out[i] = Entry{ID: id, Raw: r}
	}
	return out, nil
}

func (c *Client) cephGet(ctx context.Context, sub string, into any) (err error) {
	ctx, span := Tracer().Start(ctx, "proxmox.ceph.get")
	span.SetAttributes(
		attribute.String("proxmox.cluster_id", c.ClusterID),
		attribute.String("proxmox.ceph.resource", cephSubKind(sub)),
	)
	nodesTried := 0
	routeHit := false
	defer func() {
		span.SetAttributes(attribute.Int("nodes_tried", nodesTried), attribute.Bool("route_hit", routeHit))
		EndSpan(span, err)
	}()

	var errs []error
	// attempt reports whether the result is final: success, or a failure no
	// other node would answer differently.
	attempt := func(node string, remembered bool) (bool, error) {
		nodesTried++
		err := c.get(ctx, "/nodes/"+node+"/ceph/"+sub, nil, into)
		switch {
		case err == nil:
			if remembered {
				routeHit = true
			} else {
				span.AddEvent("ceph.route.changed", trace.WithAttributes(attribute.String("proxmox.node", node)))
				pkgLogger().Debugw(ctx, "proxmox ceph route changed", "cluster_id", c.ClusterID, "node", node)
			}
			c.ceph.set(node)
			return true, nil
		case ctx.Err() != nil, IsStatus(err, http.StatusUnauthorized), IsStatus(err, http.StatusForbidden):
			return true, err
		}
		pkgLogger().Debugw(ctx, "proxmox ceph node failed",
			append([]any{"cluster_id", c.ClusterID, "node", node, "resource", cephSubKind(sub)}, classFields(err)...)...)
		errs = append(errs, fmt.Errorf("%s: %w", node, err))
		return false, nil
	}

	last := c.ceph.get()
	if last != "" {
		if done, err := attempt(last, true); done {
			return err
		}
	}
	nodes, err := c.onlineNodes(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if node == last {
			continue
		}
		if done, err := attempt(node, false); done {
			return err
		}
	}
	if last != "" {
		span.AddEvent("ceph.route.changed", trace.WithAttributes(attribute.String("proxmox.node", "")))
	}
	c.ceph.set("")
	return &CephUnavailableError{Errs: errs}
}

// cephSubKind maps a Ceph sub-path to a closed vocabulary.
func cephSubKind(sub string) string {
	switch sub {
	case "status", "pool", "mon", "mgr", "mds", "fs", "osd":
		return sub
	default:
		return "other"
	}
}

// onlineNodes returns the names of online cluster nodes, sorted.
func (c *Client) onlineNodes(ctx context.Context) ([]string, error) {
	entries, err := c.ClusterResources(ctx, ResourceTypeNode)
	if err != nil {
		return nil, err
	}
	nodes := make([]string, 0, len(entries))
	for _, e := range entries {
		var n struct {
			Node   string `json:"node"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(e.Raw, &n); err != nil {
			return nil, fmt.Errorf("decode node resource: %w", err)
		}
		if n.Status == "online" && n.Node != "" {
			nodes = append(nodes, n.Node)
		}
	}
	sort.Strings(nodes)
	return nodes, nil
}

// stringField decodes the non-empty string field of a JSON object.
func stringField(raw json.RawMessage, field string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("decode object: %w", err)
	}
	var s string
	if err := json.Unmarshal(obj[field], &s); err != nil || s == "" {
		return "", fmt.Errorf("object has no string %q field", field)
	}
	return s, nil
}
