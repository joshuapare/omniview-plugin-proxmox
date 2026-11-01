// Package resource registers the Proxmox resource capability (resource v2).
package resource

import (
	"context"
	"fmt"
	"sync"

	resourcev2 "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/sdk"

	"github.com/mononendev/omniview-plugin-proxmox/pkg/proxmox"
)

// providerID is the canonical resource provider, equal to the plugin ID.
const providerID = "proxmox"

// Resource type IDs. They are opaque to the host; the UI, the exec, log and
// metric capabilities, and saved UI state all use these exact strings.
const (
	typeNode            = "proxmox::v1::Node"
	typeStorage         = "proxmox::v1::Storage"
	typeVM              = "proxmox::v1::VirtualMachine"
	typeContainer       = "proxmox::v1::Container"
	typeCephCluster     = "proxmox::v1::CephCluster"
	typeCephPool        = "proxmox::v1::CephPool"
	typeCephOSD         = "proxmox::v1::CephOSD"
	typeCephMonitor     = "proxmox::v1::CephMonitor"
	typeCephManager     = "proxmox::v1::CephManager"
	typeCephMDS         = "proxmox::v1::CephMetadataServer"
	typeCephFilesystem  = "proxmox::v1::CephFilesystem"
	typeTask            = "proxmox::v1::Task"
	typeSnapshot        = "proxmox::v1::Snapshot"
	representationID    = "native"
	representationRev   = "1"
	catalogRevisionBase = "2"
)

// staticKinds are served on every connection; cephKinds only on connections
// whose cluster answered `ceph status` when the connection started.
var (
	staticKinds = []*kind{
		clusterKind(typeNode, "Nodes", "node", proxmox.ResourceTypeNode, "node"),
		clusterKind(typeStorage, "Storage", "storage", proxmox.ResourceTypeStorage, "storage"),
		guestKind(typeVM, "Virtual Machines", "virtual machine", "qemu"),
		guestKind(typeContainer, "Containers", "container", "lxc"),
		taskKind(),
		snapshotKind(),
	}
	cephKinds = []*kind{
		cephKind(typeCephCluster, "Ceph Status", "Ceph cluster", (*proxmox.Client).CephStatus),
		cephKind(typeCephPool, "Pools", "Ceph pool", (*proxmox.Client).CephPools),
		cephKind(typeCephOSD, "OSDs", "Ceph OSD", (*proxmox.Client).CephOSDs),
		cephKind(typeCephMonitor, "Monitors", "Ceph monitor", (*proxmox.Client).CephMonitors),
		cephKind(typeCephManager, "Managers", "Ceph manager", (*proxmox.Client).CephManagers),
		cephKind(typeCephMDS, "Metadata Servers", "Ceph metadata server", (*proxmox.Client).CephMetadataServers),
		cephKind(typeCephFilesystem, "CephFS", "CephFS", (*proxmox.Client).CephFilesystems),
	}
)

// Register adds the resource capability to plugin.
func Register(plugin *sdk.Plugin) {
	configFiles := func() ([]string, error) { return plugin.SettingsProvider.GetStringSlice(SettingConfigFiles) }
	p, err := newProvider(configFiles)
	if err != nil {
		panic(fmt.Sprintf("build proxmox resource provider: %v", err))
	}
	sdk.RegisterResourcePlugin(plugin, p)
}

// provider is the resource controller plus the relationship declarations the
// host's graph indexes. Embedding keeps the controller's lifecycle, finite
// operations, updates and connection contexts.
type provider struct {
	*resourcev2.Controller[proxmox.Client]
}

var (
	_ resourcev2.RelationshipProvider      = (*provider)(nil)
	_ resourcev2.FeatureNegotiator         = (*provider)(nil)
	_ resourcev2.ConnectionContextProvider = (*provider)(nil)
)

func newProvider(configFiles func() ([]string, error)) (*provider, error) {
	state := &cephState{byConnection: map[string]bool{}}
	all := append(append([]*kind{}, staticKinds...), cephKinds...)
	registrations := make([]resourcev2.Registration[proxmox.Client], len(all))
	for i, k := range all {
		registrations[i] = k.registration()
	}
	controller, err := resourcev2.NewController(resourcev2.Config[proxmox.Client]{
		Catalog:       state.catalog,
		Registrations: registrations,
		Lifecycle:     lifecycle(configFiles, state),
	})
	if err != nil {
		return nil, err
	}
	return &provider{Controller: controller}, nil
}

// Features adds the relationship declarations to the controller's traits.
func (p *provider) Features(ctx context.Context) (resourcev2.FeatureSet, error) {
	features, err := p.Controller.Features(ctx)
	if err != nil {
		return features, err
	}
	features.Traits = append(features.Traits, resourcev2.FeatureRelationships)
	return features, nil
}

// cephState records, per started connection, whether its cluster runs Ceph.
// The connection's OnStart hook probes it once; Catalog, which the SDK calls
// around every operation, only reads it.
type cephState struct {
	mu           sync.RWMutex
	byConnection map[string]bool
}

func (s *cephState) set(connection string, ceph bool) {
	s.mu.Lock()
	s.byConnection[connection] = ceph
	s.mu.Unlock()
}

func (s *cephState) forget(connection string) {
	s.mu.Lock()
	delete(s.byConnection, connection)
	s.mu.Unlock()
}

func (s *cephState) get(connection string) (ceph, probed bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ceph, probed = s.byConnection[connection]
	return ceph, probed
}

// catalog lists the static kinds, plus the Ceph kinds when the connection
// found Ceph as it started. The controller only asks for the catalog of a
// started connection.
func (s *cephState) catalog(ctx context.Context, request resourcev2.CatalogRequest) (resourcev2.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return resourcev2.Catalog{}, err
	}
	ceph, started := s.get(request.Route.Connection)
	if !started {
		return resourcev2.Catalog{}, resourcev2.NewError("INVALID_REQUEST", "Connection is not started.")
	}
	kinds := staticKinds
	revision := catalogRevisionBase
	if ceph {
		kinds = append(append([]*kind{}, staticKinds...), cephKinds...)
		revision += "-ceph"
	}
	types := make([]resourcev2.ResourceType, len(kinds))
	for i, k := range kinds {
		types[i] = k.resourceType()
	}
	return resourcev2.Catalog{
		Provider: providerID, Types: types, Revision: revision, Coverage: resourcev2.Coverage{Kind: "complete"},
	}, nil
}

func nativeRepresentation() resourcev2.Representation {
	return resourcev2.Representation{
		ID:       representationID,
		Revision: representationRev,
		Schema:   resourcev2.SchemaBinding{State: "absent", Validation: "none", Unvalidated: []string{}},
	}
}
