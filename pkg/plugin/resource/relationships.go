package resource

import (
	"context"
	"encoding/json"
	"fmt"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// refsField is the object the plugin adds to each entry with the IDs of the
// entries it refers to. Proxmox entries name their node and storage by bare
// name (`"node": "pve1"`); the host's relationship graph needs the target's
// resource ID (`node/pve1`), so the references are written out here and the
// relationship extractors read them.
const refsField = "omniview"

// refs is the value of refsField.
type refs struct {
	// Node is the ID of the node a guest runs on or a storage entry belongs to.
	Node string `json:"node,omitempty"`
	// VM and Container are the ID of the guest a snapshot belongs to.
	VM        string `json:"vm,omitempty"`
	Container string `json:"container,omitempty"`
	// Storage holds the IDs of the storage entries on the guest's node with
	// the guest's volumes (disks, EFI/TPM state, CD-ROM images, unused
	// disks): empty when it has none, absent when its config could not be
	// read.
	Storage *[]string `json:"storage,omitempty"`
}

// relationship declares an edge read from a refsField path. Targets have no
// identity dimensions, so no target scope is needed.
func relationship(typ resourcev2.RelationshipType, target, label, inverse, cardinality, path string) resourcev2.RelationshipDescriptor {
	return resourcev2.RelationshipDescriptor{
		Type:            typ,
		TargetType:      target,
		Label:           label,
		InverseLabel:    inverse,
		Cardinality:     cardinality,
		Extractor:       &resourcev2.RelationshipExtractor{Method: "fieldPath", FieldPath: refsField + "." + path},
		TargetScope:     []resourcev2.ScopeEntry{},
		ScopeExtractors: []resourcev2.ScopeExtractor{},
	}
}

var (
	relGuestNode    = relationship(resourcev2.RelRunsOn, typeNode, "runs on", "runs", "many-to-one", "node")
	relGuestStorage = relationship(resourcev2.RelUses, typeStorage, "uses storage", "used by", "many-to-many", "storage")
	relStorageNode  = relationship(resourcev2.RelRunsOn, typeNode, "attached to", "has storage", "many-to-one", "node")
	relTaskNode     = relationship(resourcev2.RelRunsOn, typeNode, "ran on", "ran", "many-to-one", "node")

	relSnapshotVM        = relationship(resourcev2.RelMemberOf, typeVM, "snapshot of", "has snapshot", "many-to-one", "vm")
	relSnapshotContainer = relationship(resourcev2.RelMemberOf, typeContainer, "snapshot of", "has snapshot", "many-to-one", "container")
)

// relationshipsOf returns the edges the host indexes from refsField for one
// /cluster/resources type: guests run on a node and use storage entries;
// storage entries are attached to a node. Storage points at its node, like
// guests do, so a node's upstream neighbourhood holds its guests, its
// storage and the edges between them.
func relationshipsOf(entryType string) []resourcev2.RelationshipDescriptor {
	switch entryType {
	case "qemu", "lxc":
		return []resourcev2.RelationshipDescriptor{relGuestNode, relGuestStorage}
	case "storage":
		return []resourcev2.RelationshipDescriptor{relStorageNode}
	default:
		return nil
	}
}

// GetRelationships declares the edges of one type; the host extracts them
// from the bodies it receives.
func (p *provider) GetRelationships(
	ctx context.Context,
	_ resourcev2.AccessRoute,
	typ string,
) ([]resourcev2.RelationshipDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, k := range append(append([]*kind{}, staticKinds...), cephKinds...) {
		if k.typ == typ {
			return append([]resourcev2.RelationshipDescriptor(nil), k.relationships...), nil
		}
	}
	return nil, resourcev2.NewError("NOT_FOUND", "Unknown Proxmox resource type.")
}

// ResolveRelationships is unsupported: every edge is declared for extraction.
func (p *provider) ResolveRelationships(
	context.Context,
	resourcev2.RelationshipResolutionRequest,
) ([]resourcev2.ResolvedRelationship, error) {
	return nil, resourcev2.NewError("UNSUPPORTED", "Proxmox relationships are extracted from resource bodies.")
}

// addRefs writes refsField into each entry of the types that declare
// relationships. A guest's storage comes from its config; a guest whose
// config cannot be read is listed without storage rather than failing the
// list.
func addRefs(ctx context.Context, client *proxmox.Client, entryType string, entries []proxmox.Entry) error {
	if entryType != "qemu" && entryType != "lxc" && entryType != "storage" {
		return nil
	}

	nodes := make([]string, len(entries))
	guests := make([]proxmox.Guest, 0, len(entries))
	for i, e := range entries {
		var head struct {
			Node string `json:"node"`
		}
		if err := json.Unmarshal(e.Raw, &head); err != nil {
			return fmt.Errorf("decode %s: %w", e.ID, err)
		}
		nodes[i] = head.Node
		if entryType != "storage" && head.Node != "" {
			guests = append(guests, proxmox.Guest{ID: e.ID, Node: head.Node})
		}
	}
	var storage map[string][]string
	if len(guests) > 0 {
		storage = client.GuestStorage(ctx, guests)
	}

	for i := range entries {
		ref := refs{}
		if nodes[i] != "" {
			ref.Node = "node/" + nodes[i]
		}
		if names, ok := storage[entries[i].ID]; ok {
			ids := make([]string, len(names))
			for j, name := range names {
				ids[j] = "storage/" + nodes[i] + "/" + name
			}
			ref.Storage = &ids
		}
		raw, err := withField(entries[i].Raw, refsField, ref)
		if err != nil {
			return fmt.Errorf("encode %s: %w", entries[i].ID, err)
		}
		entries[i].Raw = raw
	}
	return nil
}

// withField returns obj with field set to value. Keys come out sorted, so
// equal objects encode to equal bytes and the watch diff stays exact.
func withField(obj json.RawMessage, field string, value any) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return nil, err
	}
	v, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m[field] = v
	return json.Marshal(m)
}
