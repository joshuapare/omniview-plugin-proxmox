package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// taskKind serves the cluster's tasks, identified by UPID. The list is the
// cluster's recent tasks; Get reads any task by UPID, older ones included.
func taskKind() *kind {
	return &kind{
		typ:           typeTask,
		label:         "Tasks",
		noun:          "task",
		policy:        resourcev2.SyncOnFirstQuery,
		relationships: []resourcev2.RelationshipDescriptor{relTaskNode},
		list: func(ctx context.Context, client *proxmox.Client) ([]proxmox.Entry, error) {
			tasks, err := client.ClusterTasks(ctx)
			if err != nil {
				return nil, err
			}
			for i, e := range tasks {
				var head struct {
					Node string `json:"node"`
				}
				if err := json.Unmarshal(e.Raw, &head); err != nil {
					return nil, fmt.Errorf("decode task %s: %w", e.ID, err)
				}
				if head.Node == "" {
					head.Node, _ = proxmox.TaskNode(e.ID)
				}
				if tasks[i].Raw, err = withTaskRefs(e.Raw, head.Node); err != nil {
					return nil, fmt.Errorf("encode task %s: %w", e.ID, err)
				}
			}
			return tasks, nil
		},
		getter: func(ctx context.Context, client *proxmox.Client, upid string) (proxmox.Entry, error) {
			node, ok := proxmox.TaskNode(upid)
			if !ok {
				return proxmox.Entry{}, nil
			}
			raw, err := client.TaskStatus(ctx, node, upid)
			if err != nil {
				// Proxmox answers an unknown UPID on a node with a 400 or 500
				// ("no such task"), an unknown node with a 404 or 595.
				if proxmox.IsStatus(err, http.StatusBadRequest) || proxmox.IsStatus(err, http.StatusNotFound) {
					return proxmox.Entry{}, nil
				}
				return proxmox.Entry{}, err
			}
			if raw, err = withTaskRefs(raw, node); err != nil {
				return proxmox.Entry{}, fmt.Errorf("encode task %s: %w", upid, err)
			}
			return proxmox.Entry{ID: upid, Raw: raw}, nil
		},
	}
}

func withTaskRefs(raw json.RawMessage, node string) (json.RawMessage, error) {
	ref := refs{}
	if node != "" {
		ref.Node = "node/" + node
	}
	return withField(raw, refsField, ref)
}
