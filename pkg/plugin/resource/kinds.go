package resource

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// lister returns every native entry of one kind.
type lister func(context.Context, *proxmox.Client) ([]proxmox.Entry, error)

// kind serves one read-only resource type from a lister. Proxmox has no
// cheaper per-object read for most kinds, so Get, Enumerate and Watch are all
// built on the full list. Bodies are the native entries, plus a refsField
// object on guests and storage, identified by the lister's ID: Proxmox's own
// cluster-unique `id` for /cluster/resources kinds (qemu/100, node/pve1,
// storage/pve1/local), the Ceph name otherwise. Types have no identity or
// query dimensions: those IDs are unique within a connection's cluster.
type kind struct {
	typ string
	// label is the plural display name: "Virtual Machines".
	label string
	// noun names one object in messages: "virtual machine", "Ceph pool".
	noun   string
	list   lister
	policy resourcev2.SyncPolicy
	// relationships are the edges the host graph extracts from the bodies.
	relationships []resourcev2.RelationshipDescriptor
	// guestType is qemu or lxc for guest kinds; empty otherwise.
	guestType string
	// getter reads one object by ID; nil scans the list.
	getter func(context.Context, *proxmox.Client, string) (proxmox.Entry, error)
	// writes adds actions and mutations; nil kinds are read-only.
	writes *kindWrites
}

// kindWrites are the write operations of a kind and their callbacks.
type kindWrites struct {
	operations  []resourcev2.Operation
	descriptors func() []resourcev2.ActionDescriptor
	action      func(context.Context, *proxmox.Client, resourcev2.ActionRequest) (resourcev2.Value, error)
	mutate      func(context.Context, *proxmox.Client, resourcev2.MutationRequest) (resourcev2.Record, error)
}

var _ resourcev2.ErrorClassifier = (*kind)(nil)

// clusterKind serves the /cluster/resources entries of one type, watched
// from connect, with refsField added where the type declares relationships.
func clusterKind(typ, label, noun string, apiType proxmox.ResourceType, entryType string) *kind {
	return &kind{
		typ:           typ,
		label:         label,
		noun:          noun,
		policy:        resourcev2.SyncOnConnect,
		relationships: relationshipsOf(entryType),
		list: func(ctx context.Context, client *proxmox.Client) ([]proxmox.Entry, error) {
			all, err := client.ClusterResources(ctx, apiType)
			if err != nil {
				return nil, err
			}
			out := make([]proxmox.Entry, 0, len(all))
			for _, e := range all {
				if e.Type == entryType {
					out = append(out, e.Entry)
				}
			}
			if err := addRefs(ctx, client, entryType, out); err != nil {
				return nil, err
			}
			return out, nil
		},
	}
}

// guestKind serves the guests of guestType (qemu or lxc).
func guestKind(typ, label, noun, guestType string) *kind {
	k := clusterKind(typ, label, noun, proxmox.ResourceTypeVM, guestType)
	k.guestType = guestType
	k.writes = &kindWrites{
		operations: []resourcev2.Operation{
			resourcev2.OperationAction, resourcev2.OperationMutationUpdate, resourcev2.OperationDelete,
		},
		descriptors: k.actionDescriptors,
		action:      k.action,
		mutate:      k.mutate,
	}
	return k
}

// cephKind serves one Ceph kind from a Client method expression, e.g.
// (*proxmox.Client).CephPools. It is watched only once opened: Ceph state
// needs no polling until asked.
func cephKind(
	typ, label, noun string,
	method func(*proxmox.Client, context.Context) ([]proxmox.Entry, error),
) *kind {
	return &kind{
		typ:    typ,
		label:  label,
		noun:   noun,
		policy: resourcev2.SyncOnFirstQuery,
		list: func(ctx context.Context, client *proxmox.Client) ([]proxmox.Entry, error) {
			return method(client, ctx)
		},
	}
}

// resourceType declares the kind: one native representation, point reads,
// enumeration, the kind's writes and, unless it is never synced,
// best-effort updates.
func (k *kind) resourceType() resourcev2.ResourceType {
	operations := []resourcev2.Operation{resourcev2.OperationGet, resourcev2.OperationEnumerate}
	if k.writes != nil {
		operations = append(operations, k.writes.operations...)
	}
	capabilities := make([]resourcev2.Capability, 0, len(operations)+1)
	for _, op := range operations {
		capabilities = append(capabilities, resourcev2.Capability{Operation: op, Scope: resourcev2.QueryScope{}})
	}
	if k.policy != resourcev2.SyncNever {
		capabilities = append(capabilities, resourcev2.Capability{
			Operation: resourcev2.OperationUpdate,
			Scope:     resourcev2.QueryScope{},
			Guarantee: resourcev2.BestEffortGuarantee,
		})
	}
	return resourcev2.ResourceType{
		Type:                  k.typ,
		DisplayName:           k.label,
		IdentityDimensions:    []resourcev2.Dimension{},
		QueryDimensions:       []resourcev2.Dimension{},
		Representations:       []resourcev2.Representation{nativeRepresentation()},
		DefaultRepresentation: representationID,
		Capabilities:          capabilities,
	}
}

// registration binds the kind's callbacks; it matches resourceType.
func (k *kind) registration() resourcev2.Registration[proxmox.Client] {
	r := resourcev2.Registration[proxmox.Client]{
		Type:            k.typ,
		Get:             k.get,
		Enumerate:       k.enumerate,
		SyncPolicy:      k.policy,
		ErrorClassifier: k,
	}
	if k.policy != resourcev2.SyncNever {
		r.Watch = k.watch
	}
	if k.writes != nil {
		r.Mutate = k.writes.mutate
		if k.writes.descriptors != nil {
			r.Action = k.writes.action
			r.Actions = k.writes.descriptors()
		}
	}
	return r
}

func (k *kind) get(ctx context.Context, client *proxmox.Client, request resourcev2.GetRequest) (resourcev2.Record, error) {
	id := request.Reference.Address.ID
	if k.getter != nil {
		e, err := k.getter(ctx, client, id)
		var notFoundErr *proxmox.GuestNotFoundError
		switch {
		case errors.As(err, &notFoundErr), err == nil && e.ID != id:
			return resourcev2.Record{}, k.notFound(id)
		case err != nil:
			return resourcev2.Record{}, err
		}
		return record(request.Reference, e)
	}
	entries, err := k.list(ctx, client)
	if err != nil {
		return resourcev2.Record{}, err
	}
	for _, e := range entries {
		if e.ID == id {
			return record(request.Reference, e)
		}
	}
	return resourcev2.Record{}, k.notFound(id)
}

// enumerate pages through the kind sorted by ID. The cursor is the last ID
// of the previous page, so an object added or removed between pages shifts
// no other object onto or off a page.
func (k *kind) enumerate(
	ctx context.Context,
	client *proxmox.Client,
	request resourcev2.EnumerateRequest,
) (resourcev2.SourcePage, error) {
	entries, err := k.list(ctx, client)
	if err != nil {
		return resourcev2.SourcePage{}, err
	}
	slices.SortFunc(entries, func(a, b proxmox.Entry) int { return strings.Compare(a.ID, b.ID) })
	start := 0
	if request.Cursor != nil {
		after := *request.Cursor
		start, _ = slices.BinarySearchFunc(entries, after, func(e proxmox.Entry, id string) int {
			return strings.Compare(e.ID, id)
		})
		if start < len(entries) && entries[start].ID == after {
			start++
		}
	}
	size := int(request.PageSize)
	if size == 0 || size > resourcev2.MaxPageRecords {
		size = resourcev2.MaxPageRecords
	}
	end := min(start+size, len(entries))

	target := request.Target
	records := make([]resourcev2.Record, 0, end-start)
	for _, e := range entries[start:end] {
		rec, err := record(reference(target.Provider, target.Authority, target.Type, e.ID), e)
		if err != nil {
			return resourcev2.SourcePage{}, err
		}
		records = append(records, rec)
	}
	total := strconv.Itoa(len(entries))
	page := resourcev2.SourcePage{
		Records:     records,
		Count:       resourcev2.Count{Kind: "exact", Value: &total},
		Coverage:    resourcev2.Coverage{Kind: "complete"},
		Consistency: "bestEffort",
	}
	if end < len(entries) {
		next := entries[end-1].ID
		page.NextCursor = &next
	}
	return page, nil
}

func reference(provider, authority, typ, id string) resourcev2.ResourceReference {
	return resourcev2.ResourceReference{Address: resourcev2.ResourceAddress{
		Provider: provider, Authority: authority, Type: typ, Scope: []resourcev2.ScopeEntry{}, ID: id,
	}}
}

// record wraps a native entry, keeping its integers exact.
func record(ref resourcev2.ResourceReference, e proxmox.Entry) (resourcev2.Record, error) {
	body, err := resourcev2.JSONToValue(e.Raw)
	if err != nil {
		return resourcev2.Record{}, &resourcev2.ResourceOperationError{
			Err: err, Code: "PROVIDER_ERROR", Title: "Unreadable Proxmox object",
			Message: "The Proxmox API returned an object that is not a JSON object: " + e.ID + ".",
		}
	}
	return resourcev2.Record{
		Reference:      ref,
		Body:           body,
		Representation: nativeRepresentation(),
		Coverage:       resourcev2.Coverage{Kind: "complete"},
	}, nil
}

func (k *kind) notFound(id string) error {
	return &resourcev2.ResourceOperationError{
		Code:    "NOT_FOUND",
		Title:   "Not found",
		Message: "No " + k.noun + " with ID " + id + " exists in this cluster.",
	}
}

// ClassifyError maps Proxmox API failures to structured errors.
func (k *kind) ClassifyError(err error) error {
	var opErr *resourcev2.ResourceOperationError
	if errors.As(err, &opErr) {
		return err
	}
	var (
		cephErr     *proxmox.CephUnavailableError
		writeErr    *writeError
		apiErr      *proxmox.APIError
		taskErr     *proxmox.TaskError
		notFoundErr *proxmox.GuestNotFoundError
	)
	isWrite := errors.As(err, &writeErr)
	switch {
	case errors.As(err, &notFoundErr):
		notFound := k.notFound(notFoundErr.ID).(*resourcev2.ResourceOperationError)
		notFound.Err = err
		return notFound
	case isWrite && proxmox.IsStatus(err, http.StatusForbidden):
		suggestion := "Grant the token PVEVMAdmin on /vms (or the guest's path)."
		if writeErr.privilege == privAudit {
			suggestion = "Grant the token PVEAuditor on /vms (or the guest's path)."
		}
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "FORBIDDEN", Title: "Forbidden",
			Message:     "The Proxmox API token lacks the " + writeErr.privilege + " privilege needed to " + writeErr.what + ".",
			Suggestions: []string{suggestion},
		}
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest:
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "INVALID_INPUT", Title: "Rejected by Proxmox", Message: invalidMessage(apiErr),
		}
	case errors.As(err, &taskErr):
		what := "The Proxmox task"
		if isWrite {
			what = "The task to " + writeErr.what
		}
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "PROVIDER_ERROR", Title: "Task failed",
			Message: what + " failed: " + taskErr.ExitStatus,
		}
	case proxmox.IsStatus(err, http.StatusUnauthorized):
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "UNAUTHORIZED", Title: "Unauthorized",
			Message: "The Proxmox API token was rejected.",
		}
	case proxmox.IsStatus(err, http.StatusForbidden):
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "FORBIDDEN", Title: "Forbidden",
			Message:     "The Proxmox API token lacks permission to read " + k.noun + " objects.",
			Suggestions: []string{"Grant the token PVEAuditor on /."},
		}
	case errors.As(err, &cephErr):
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "UNAVAILABLE", Title: "Ceph unavailable", Message: err.Error(),
			Suggestions: []string{
				"Ceph must be installed and initialised (pveceph install, pveceph init) on an online node, with its monitors in quorum.",
				"A cluster without Ceph can ignore the Ceph section.",
			},
		}
	default:
		return &resourcev2.ResourceOperationError{
			Err: err, Code: "PROVIDER_ERROR", Title: "Proxmox API error", Message: err.Error(),
		}
	}
}

// invalidMessage renders a 400 response: a first line with Proxmox's reason
// (no "word: " pair in it), then one "param: reason" line per parameter
// error, sorted by parameter, for the UI to attach to its fields.
func invalidMessage(e *proxmox.APIError) string {
	msg := "Proxmox rejected the request (" + strings.TrimSuffix(e.Reason, ".") + ")"
	keys := make([]string, 0, len(e.Errors))
	for k := range e.Errors {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		reason := strings.Join(strings.Fields(strings.ReplaceAll(e.Errors[k], ";", ",")), " ")
		msg += "\n" + k + ": " + reason
	}
	return msg
}
