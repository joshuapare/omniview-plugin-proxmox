package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// Privileges a snapshot write needs, named in FORBIDDEN errors.
const (
	privSnapshot = "VM.Snapshot"
	privRollback = "VM.Snapshot.Rollback"
)

// snapshotReadConcurrency bounds parallel snapshot list reads, like guest
// config reads.
const snapshotReadConcurrency = 4

// snapshotName is Proxmox's snapshot name format.
var snapshotName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,39}$`)

const actionRollback = "rollback"

var rollbackAction = resourcev2.ActionDescriptor{
	ID: actionRollback, Label: "Rollback", Icon: "LuHistory", Dangerous: true,
	Scope:        resourcev2.ActionScopeInstance,
	Description:  "Reset the guest to this snapshot. Changes made since the snapshot are lost.",
	ParamsSchema: &resourcev2.Schema{},
}

// snapshotKind serves the snapshots of every guest, identified as
// <type>/<vmid>/<name>. Listing reads each guest, so it is never watched.
func snapshotKind() *kind {
	k := &kind{
		typ:           typeSnapshot,
		label:         "Snapshots",
		noun:          "snapshot",
		policy:        resourcev2.SyncNever,
		relationships: []resourcev2.RelationshipDescriptor{relSnapshotVM, relSnapshotContainer},
		list:          listSnapshots,
	}
	k.getter = k.getSnapshot
	k.writes = &kindWrites{
		operations:  []resourcev2.Operation{resourcev2.OperationAction, resourcev2.OperationCreate, resourcev2.OperationDelete},
		descriptors: func() []resourcev2.ActionDescriptor { return []resourcev2.ActionDescriptor{rollbackAction} },
		action:      k.snapshotAction,
		mutate:      k.snapshotMutate,
	}
	return k
}

// listSnapshots reads the snapshots of every guest that is not a template
// and whose node is reachable.
func listSnapshots(ctx context.Context, client *proxmox.Client) ([]proxmox.Entry, error) {
	all, err := client.ClusterResources(ctx, proxmox.ResourceTypeVM)
	if err != nil {
		return nil, err
	}
	var guests []proxmox.GuestRef
	for _, e := range all {
		var head struct {
			Node     string `json:"node"`
			Status   string `json:"status"`
			Template int    `json:"template"`
		}
		if err := json.Unmarshal(e.Raw, &head); err != nil {
			return nil, fmt.Errorf("decode guest %s: %w", e.ID, err)
		}
		if head.Template == 1 || head.Status == "unknown" || head.Node == "" {
			continue
		}
		guests = append(guests, proxmox.GuestRef{ID: e.ID, Node: head.Node, Raw: e.Raw})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([][]proxmox.Entry, len(guests))
	errs := make([]error, len(guests))
	sem := make(chan struct{}, snapshotReadConcurrency)
	var wg sync.WaitGroup
	for i, g := range guests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			results[i], errs[i] = guestSnapshots(ctx, client, g)
			if errs[i] != nil {
				cancel()
			}
		}()
	}
	wg.Wait()
	// Report the failure that caused the cancellation, not the cancellations.
	var first error
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, err
		}
		if err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return nil, first
	}
	var out []proxmox.Entry
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

// guestSnapshots lists one guest's snapshots without "current".
func guestSnapshots(ctx context.Context, client *proxmox.Client, g proxmox.GuestRef) ([]proxmox.Entry, error) {
	raws, err := client.GuestSnapshots(ctx, g)
	if err != nil {
		return nil, err
	}
	ref := refs{Node: "node/" + g.Node}
	if strings.HasPrefix(g.ID, "qemu/") {
		ref.VM = g.ID
	} else {
		ref.Container = g.ID
	}
	out := make([]proxmox.Entry, 0, len(raws))
	for _, raw := range raws {
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("decode snapshot of %s: %w", g.ID, err)
		}
		if head.Name == "" || head.Name == "current" {
			continue
		}
		body, err := withField(raw, refsField, ref)
		if err != nil {
			return nil, fmt.Errorf("encode snapshot %s of %s: %w", head.Name, g.ID, err)
		}
		out = append(out, proxmox.Entry{ID: g.ID + "/" + head.Name, Raw: body})
	}
	return out, nil
}

// splitSnapshotID splits <type>/<vmid>/<name> into the guest ID and name.
func splitSnapshotID(id string) (guest, name string, ok bool) {
	i := strings.LastIndexByte(id, '/')
	if i < 0 {
		return "", "", false
	}
	guest, name = id[:i], id[i+1:]
	typ, vmid, found := strings.Cut(guest, "/")
	if !found || (typ != "qemu" && typ != "lxc") || vmid == "" || strings.Contains(vmid, "/") || name == "" {
		return "", "", false
	}
	return guest, name, true
}

func (k *kind) getSnapshot(ctx context.Context, client *proxmox.Client, id string) (proxmox.Entry, error) {
	guest, name, ok := splitSnapshotID(id)
	if !ok {
		return proxmox.Entry{}, nil
	}
	g, err := client.FindGuest(ctx, guest)
	if err != nil {
		return proxmox.Entry{}, err
	}
	entries, err := guestSnapshots(ctx, client, g)
	if err != nil {
		return proxmox.Entry{}, err
	}
	for _, e := range entries {
		if e.ID == guest+"/"+name {
			return e, nil
		}
	}
	return proxmox.Entry{}, nil
}

// snapshotGuest resolves a snapshot ID to its guest, failing as not found.
func (k *kind) snapshotGuest(ctx context.Context, client *proxmox.Client, id string) (proxmox.GuestRef, string, error) {
	guest, name, ok := splitSnapshotID(id)
	if !ok {
		return proxmox.GuestRef{}, "", k.notFound(id)
	}
	g, err := k.findSnapshotGuest(ctx, client, guest, id)
	if err != nil {
		return proxmox.GuestRef{}, "", err
	}
	return g, name, nil
}

// findSnapshotGuest finds the guest of snapshot id; a missing guest is a
// not-found naming both.
func (k *kind) findSnapshotGuest(ctx context.Context, client *proxmox.Client, guest, id string) (proxmox.GuestRef, error) {
	g, err := client.FindGuest(ctx, guest)
	var notFoundErr *proxmox.GuestNotFoundError
	if errors.As(err, &notFoundErr) {
		return proxmox.GuestRef{}, &resourcev2.ResourceOperationError{
			Err: err, Code: "NOT_FOUND", Title: "Not found",
			Message: "No guest " + guest + " exists in this cluster for snapshot " + id + ".",
		}
	}
	return g, err
}

// snapshotAction rolls the guest back to the snapshot.
func (k *kind) snapshotAction(ctx context.Context, client *proxmox.Client, request resourcev2.ActionRequest) (resourcev2.Value, error) {
	if request.TypeTarget != nil || request.Action != actionRollback {
		return resourcev2.Value{}, &resourcev2.ResourceOperationError{
			Code: "UNSUPPORTED", Title: "Unsupported action",
			Message: "Action " + request.Action + " is not available for a snapshot.",
		}
	}
	id := request.Reference.Address.ID
	if _, err := k.get(ctx, client, resourcev2.GetRequest{Route: request.Route, Reference: request.Reference, Representation: representationID}); err != nil {
		return resourcev2.Value{}, err
	}
	g, name, err := k.snapshotGuest(ctx, client, id)
	if err != nil {
		return resourcev2.Value{}, err
	}
	what := "roll " + g.Label() + " back to snapshot " + name
	upid, err := client.RollbackSnapshot(ctx, g, name)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privRollback, what: what, err: err}
	}
	done, err := waitTask(ctx, client, g.Node, upid)
	if err != nil {
		return resourcev2.Value{}, &writeError{privilege: privRollback, what: what, err: err}
	}
	message := "Rollback of " + g.Label() + " to " + name + " is still running."
	if done {
		message = "Rolled " + g.Label() + " back to " + name + "."
	}
	return resourcev2.ValueFromNative(map[string]any{"message": message, "upid": upid})
}

// snapshotMutate takes or deletes a snapshot.
func (k *kind) snapshotMutate(ctx context.Context, client *proxmox.Client, request resourcev2.MutationRequest) (resourcev2.Record, error) {
	if request.ExpectedRevision != nil {
		return resourcev2.Record{}, resourcev2.NewError("INVALID_REQUEST", "Snapshots take no expected revision.")
	}
	switch request.Operation {
	case "create":
		return k.createSnapshot(ctx, client, request)
	case "delete":
		return k.deleteSnapshot(ctx, client, request)
	default:
		return resourcev2.Record{}, resourcev2.NewError("UNSUPPORTED", "Snapshots can only be created or deleted.")
	}
}

func (k *kind) createSnapshot(ctx context.Context, client *proxmox.Client, request resourcev2.MutationRequest) (resourcev2.Record, error) {
	id := request.Reference.Address.ID
	guest, name, ok := splitSnapshotID(id)
	if !ok || !snapshotName.MatchString(name) {
		return resourcev2.Record{}, &resourcev2.ResourceOperationError{
			Code: "INVALID_INPUT", Title: "Invalid ID",
			Message: "Create needs an ID of the form <qemu|lxc>/<vmid>/<name>, the name 2-40 letters, digits, _ or -, starting with a letter.",
		}
	}
	params, err := actionParams(request.Input)
	if err != nil {
		return resourcev2.Record{}, err
	}
	description, err := stringParam(params, "description", false)
	if err != nil {
		return resourcev2.Record{}, err
	}
	vmstate, err := boolParam(params, "vmstate")
	if err != nil {
		return resourcev2.Record{}, err
	}
	if vmstate && !strings.HasPrefix(guest, "qemu/") {
		return resourcev2.Record{}, invalidParams("vmstate is only available for virtual machines.")
	}
	g, err := k.findSnapshotGuest(ctx, client, guest, id)
	if err != nil {
		return resourcev2.Record{}, err
	}
	what := "snapshot " + g.Label() + " as " + name
	upid, err := client.CreateSnapshot(ctx, g, name, description, vmstate)
	if err != nil {
		return resourcev2.Record{}, &writeError{privilege: privSnapshot, what: what, err: err}
	}
	done, err := waitTask(ctx, client, g.Node, upid)
	if err != nil {
		return resourcev2.Record{}, &writeError{privilege: privSnapshot, what: what, err: err}
	}
	if !done {
		return resourcev2.Record{}, stillRunning("Snapshot "+name+" of "+g.Label(), upid)
	}
	return k.get(ctx, client, resourcev2.GetRequest{Route: request.Route, Reference: request.Reference, Representation: representationID})
}

func (k *kind) deleteSnapshot(ctx context.Context, client *proxmox.Client, request resourcev2.MutationRequest) (resourcev2.Record, error) {
	rec, err := k.get(ctx, client, resourcev2.GetRequest{Route: request.Route, Reference: request.Reference, Representation: representationID})
	if err != nil {
		return resourcev2.Record{}, err
	}
	g, name, err := k.snapshotGuest(ctx, client, request.Reference.Address.ID)
	if err != nil {
		return resourcev2.Record{}, err
	}
	what := "delete snapshot " + name + " of " + g.Label()
	upid, err := client.DeleteSnapshot(ctx, g, name)
	if err != nil {
		return resourcev2.Record{}, &writeError{privilege: privSnapshot, what: what, err: err}
	}
	if _, err := waitTask(ctx, client, g.Node, upid); err != nil {
		return resourcev2.Record{}, &writeError{privilege: privSnapshot, what: what, err: err}
	}
	return rec, nil
}
