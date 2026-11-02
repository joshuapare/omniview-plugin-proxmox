// Package logs streams node journals and Ceph daemon logs through the
// Proxmox VE syslog API (GET /nodes/{node}/syslog).
package logs

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/sdk"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	sdklogs "github.com/omniviewdev/plugin-sdk/pkg/v1/logs"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

const pluginID = "proxmox"

// Resource keys served, from the resource capability.
const (
	keyNode       = "proxmox::v1::Node"
	keyCephOSD    = "proxmox::v1::CephOSD"
	keyCephMon    = "proxmox::v1::CephMonitor"
	keyCephMgr    = "proxmox::v1::CephManager"
	keyCephMDS    = "proxmox::v1::CephMetadataServer"
	keyTask       = "proxmox::v1::Task"
	labelNode     = "node"
	labelUnit     = "unit"
	labelUPID     = "upid"
	wholeJournal  = "syslog"
	sourceIDSep   = ":"
	osdNamePrefix = "osd."
)

// cephUnits maps the Ceph daemon kinds to their systemd unit template.
var cephUnits = map[string]string{
	keyCephOSD: "ceph-osd@",
	keyCephMon: "ceph-mon@",
	keyCephMgr: "ceph-mgr@",
	keyCephMDS: "ceph-mds@",
}

// Register adds the log capability to the plugin.
func Register(plugin *sdk.Plugin) {
	handlers := make(map[string]sdklogs.Handler, len(cephUnits)+2)
	for _, key := range []string{keyNode, keyCephOSD, keyCephMon, keyCephMgr, keyCephMDS} {
		handlers[key] = sdklogs.Handler{
			Plugin:        pluginID,
			Resource:      key,
			Handler:       handler,
			SourceBuilder: sourcesFor(key),
		}
	}
	handlers[keyTask] = sdklogs.Handler{
		Plugin:        pluginID,
		Resource:      keyTask,
		Handler:       taskHandler,
		SourceBuilder: taskSources,
	}
	if err := sdklogs.RegisterPlugin(plugin, sdklogs.PluginOpts{Handlers: handlers}); err != nil {
		panic(fmt.Sprintf("register proxmox log capability: %v", err))
	}
}

// sourcesFor builds the single source of a resource: one node's journal,
// whole or restricted to the daemon's unit.
func sourcesFor(key string) sdklogs.SourceBuilderFunc {
	return func(resourceID string, body *resourcev2.Value, _ sdklogs.LogSessionOptions) []sdklogs.LogSource {
		node, unit, err := locate(key, resourceID, proxmox.ResourceData(body))
		if err != nil {
			return nil
		}
		name := unit
		if name == "" {
			name = wholeJournal
		}
		return []sdklogs.LogSource{{
			ID:     node + sourceIDSep + name,
			Labels: map[string]string{labelNode: node, labelUnit: unit},
		}}
	}
}

// handler opens the source's journal; sourcesFor labelled it.
func handler(ctx *types.PluginContext, req sdklogs.LogStreamRequest) (io.ReadCloser, error) {
	node, unit := req.Labels[labelNode], req.Labels[labelUnit]
	if node == "" {
		return nil, fmt.Errorf("log source %q has no node", req.SourceID)
	}
	client, err := proxmox.ClientForConnection(ctx.Connection)
	if err != nil {
		return nil, err
	}
	return open(ctx.Context, client, node, unit, req)
}

// locate returns the node whose journal holds the resource's log and the
// systemd unit to filter it by (empty for a whole node journal). Ceph
// daemons carry their node in `host`; nodes are /cluster/resources entries.
func locate(key, resourceID string, data map[string]any) (node, unit string, err error) {
	if key == keyNode {
		node = stringField(data, "node")
		if node == "" {
			id := stringField(data, "id")
			if id == "" {
				id = resourceID
			}
			node = strings.TrimPrefix(id, "node/")
		}
		if node == "" {
			return "", "", fmt.Errorf("no node name in the resource")
		}
		return node, "", nil
	}

	prefix, ok := cephUnits[key]
	if !ok {
		return "", "", fmt.Errorf("no logs for resource %s", key)
	}
	node = stringField(data, "host")
	if node == "" {
		return "", "", fmt.Errorf("the Ceph daemon has no host")
	}
	name := stringField(data, "name")
	if key == keyCephOSD {
		id, isNum := strings.CutPrefix(name, osdNamePrefix)
		if _, err := strconv.Atoi(id); !isNum || err != nil {
			n, ok := data["id"].(int64)
			if !ok {
				return "", "", fmt.Errorf("cannot tell the OSD id of %q", name)
			}
			id = strconv.FormatInt(n, 10)
		}
		name = id
	}
	if name == "" {
		return "", "", fmt.Errorf("the Ceph daemon has no name")
	}
	return node, prefix + name, nil
}

func stringField(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}
