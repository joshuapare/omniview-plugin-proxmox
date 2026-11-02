package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// GuestNotFoundError is a guest ID absent from /cluster/resources.
type GuestNotFoundError struct {
	ID string
}

func (e *GuestNotFoundError) Error() string {
	return "guest " + e.ID + " not found in the cluster"
}

// GuestRef is a guest located at call time, for the write methods.
type GuestRef struct {
	// ID is the cluster resource ID, qemu/100 or lxc/200.
	ID   string
	Node string
	// Name is the guest's name, empty when it has none.
	Name string
	// Raw is the /cluster/resources entry the guest was found in.
	Raw json.RawMessage
}

// Label names the guest in messages: "web01 (qemu/100)", or the ID alone.
func (g GuestRef) Label() string {
	if g.Name == "" {
		return g.ID
	}
	return g.Name + " (" + g.ID + ")"
}

// path returns the guest's API path, /nodes/{node}/{type}/{vmid}, plus suffix.
func (g GuestRef) path(suffix string) string {
	typ, vmid, _ := strings.Cut(g.ID, "/")
	return "/nodes/" + url.PathEscape(g.Node) + "/" + url.PathEscape(typ) + "/" + url.PathEscape(vmid) + suffix
}

// FindGuest returns the guest with cluster resource ID id (qemu/100,
// lxc/200) on the node it currently runs on, read from /cluster/resources so
// a migrated guest is addressed at its new node. A missing guest is a
// *GuestNotFoundError.
func (c *Client) FindGuest(ctx context.Context, id string) (GuestRef, error) {
	typ, vmid, ok := strings.Cut(id, "/")
	if !ok || vmid == "" || (typ != "qemu" && typ != "lxc") {
		return GuestRef{}, &GuestNotFoundError{ID: id}
	}
	all, err := c.ClusterResources(ctx, ResourceTypeVM)
	if err != nil {
		return GuestRef{}, err
	}
	for _, e := range all {
		if e.ID != id {
			continue
		}
		var head struct {
			Node string `json:"node"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(e.Raw, &head); err != nil {
			return GuestRef{}, fmt.Errorf("decode guest %s: %w", id, err)
		}
		if head.Node == "" {
			return GuestRef{}, fmt.Errorf("guest %s has no node", id)
		}
		return GuestRef{ID: id, Node: head.Node, Name: head.Name, Raw: e.Raw}, nil
	}
	return GuestRef{}, &GuestNotFoundError{ID: id}
}

// GuestStatusAction runs POST .../status/{action} (start, shutdown, reboot,
// stop, suspend, resume) and returns the task's UPID.
func (c *Client) GuestStatusAction(ctx context.Context, g GuestRef, action string) (string, error) {
	var upid string
	if err := c.post(ctx, g.path("/status/"+url.PathEscape(action)), url.Values{}, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// DeleteGuest destroys the guest with DELETE /nodes/{node}/{type}/{vmid},
// removing it from backup jobs, replication and HA (purge) and destroying
// its unreferenced disks too. It returns the task's UPID. Proxmox refuses
// to destroy a running guest.
func (c *Client) DeleteGuest(ctx context.Context, g GuestRef) (string, error) {
	var upid string
	query := url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}
	if err := c.del(ctx, g.path(""), query, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// GuestConfig returns the guest's configuration without its digest, and the
// digest that identifies this version of it for UpdateGuestConfig. Numbers
// are json.Number, so integers stay exact.
func (c *Client) GuestConfig(ctx context.Context, g GuestRef) (map[string]any, string, error) {
	var raw json.RawMessage
	if err := c.get(ctx, g.path("/config"), nil, &raw); err != nil {
		return nil, "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var config map[string]any
	if err := dec.Decode(&config); err != nil {
		return nil, "", fmt.Errorf("decode %s config: %w", g.ID, err)
	}
	digest, _ := config["digest"].(string)
	delete(config, "digest")
	return config, digest, nil
}

// VMPending returns Proxmox's current and pending VM configuration entries.
func (c *Client) VMPending(ctx context.Context, g GuestRef) ([]any, error) {
	var raw json.RawMessage
	if err := c.get(ctx, g.path("/pending"), nil, &raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var entries []any
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode %s pending configuration: %w", g.ID, err)
	}
	return entries, nil
}

// UpdateGuestConfig sets the keys of set and deletes the keys in del with
// PUT .../config. A non-empty digest makes Proxmox reject the change when
// the config was modified since the digest was read.
func (c *Client) UpdateGuestConfig(ctx context.Context, g GuestRef, set map[string]string, del []string, digest string) error {
	form := url.Values{}
	for k, v := range set {
		form.Set(k, v)
	}
	if len(del) > 0 {
		keys := slices.Clone(del)
		slices.Sort(keys)
		form.Set("delete", strings.Join(keys, ","))
	}
	if digest != "" {
		form.Set("digest", digest)
	}
	return c.put(ctx, g.path("/config"), form, nil)
}

// CreateGuest creates guest vmid of guestType (qemu or lxc) on node with
// POST /nodes/{node}/{type}, the form holding its configuration, and
// returns the task's UPID.
func (c *Client) CreateGuest(ctx context.Context, node, guestType string, vmid int, form map[string]string) (string, error) {
	values := url.Values{}
	for k, v := range form {
		values.Set(k, v)
	}
	values.Set("vmid", strconv.Itoa(vmid))
	var upid string
	if err := c.post(ctx, "/nodes/"+url.PathEscape(node)+"/"+url.PathEscape(guestType), values, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// CloneGuest clones the guest with POST .../clone and returns the task's UPID.
func (c *Client) CloneGuest(ctx context.Context, g GuestRef, form url.Values) (string, error) {
	var upid string
	if err := c.post(ctx, g.path("/clone"), form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// MigrateGuest moves the guest to another node with POST .../migrate and
// returns the task's UPID.
func (c *Client) MigrateGuest(ctx context.Context, g GuestRef, form url.Values) (string, error) {
	var upid string
	if err := c.post(ctx, g.path("/migrate"), form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// AgentExecStatus is the state of a command started through the QEMU
// guest agent.
type AgentExecStatus struct {
	Exited       int    `json:"exited"`
	ExitCode     *int   `json:"exitcode"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated int    `json:"out-truncated"`
	ErrTruncated int    `json:"err-truncated"`
}

// AgentExec starts command (program and arguments) in a VM through the QEMU
// guest agent with POST .../agent/exec, input fed to its stdin, and returns
// the process ID for AgentExecStatus.
func (c *Client) AgentExec(ctx context.Context, g GuestRef, command []string, input string) (int64, error) {
	form := url.Values{"command": command}
	if input != "" {
		form.Set("input-data", input)
	}
	var out struct {
		PID int64 `json:"pid"`
	}
	if err := c.post(ctx, g.path("/agent/exec"), form, &out); err != nil {
		return 0, err
	}
	return out.PID, nil
}

// AgentExecStatus reads the state of guest agent process pid with
// GET .../agent/exec-status.
func (c *Client) AgentExecStatus(ctx context.Context, g GuestRef, pid int64) (AgentExecStatus, error) {
	var status AgentExecStatus
	query := url.Values{"pid": {strconv.FormatInt(pid, 10)}}
	if err := c.get(ctx, g.path("/agent/exec-status"), query, &status); err != nil {
		return AgentExecStatus{}, err
	}
	return status, nil
}

// GuestSnapshots lists the guest's snapshots with GET .../snapshot,
// including the "current" pseudo-snapshot Proxmox appends.
func (c *Client) GuestSnapshots(ctx context.Context, g GuestRef) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := c.get(ctx, g.path("/snapshot"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSnapshot takes snapshot name of the guest with POST .../snapshot;
// vmstate (VMs only) saves the RAM too. It returns the task's UPID.
func (c *Client) CreateSnapshot(ctx context.Context, g GuestRef, name, description string, vmstate bool) (string, error) {
	form := url.Values{"snapname": {name}}
	if description != "" {
		form.Set("description", description)
	}
	if vmstate {
		form.Set("vmstate", "1")
	}
	var upid string
	if err := c.post(ctx, g.path("/snapshot"), form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// DeleteSnapshot removes snapshot name with DELETE .../snapshot/{name} and
// returns the task's UPID.
func (c *Client) DeleteSnapshot(ctx context.Context, g GuestRef, name string) (string, error) {
	var upid string
	if err := c.del(ctx, g.path("/snapshot/"+url.PathEscape(name)), nil, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// RollbackSnapshot resets the guest to snapshot name with
// POST .../snapshot/{name}/rollback and returns the task's UPID.
func (c *Client) RollbackSnapshot(ctx context.Context, g GuestRef, name string) (string, error) {
	var upid string
	if err := c.post(ctx, g.path("/snapshot/"+url.PathEscape(name)+"/rollback"), url.Values{}, &upid); err != nil {
		return "", err
	}
	return upid, nil
}
