package proxmox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/omniviewdev/plugin-sdk/pkg/types"
)

// ConnectionDataConfigFile is the Connection.Data key holding the config file
// a connection was loaded from. The token secret never travels in
// Connection.Data; it is re-read from this file.
const ConnectionDataConfigFile = "config_file"

// connClients caches one client per connection for capabilities that are
// handed a bare *types.Connection (metrics, logs, exec), so repeated calls
// reuse the HTTP transport instead of handshaking anew.
var connClients = struct {
	sync.Mutex
	m map[string]cachedClient
}{m: make(map[string]cachedClient)}

type cachedClient struct {
	cluster Cluster
	client  *Client
}

// ClientForConnection returns a client for conn, reading its cluster (and
// token secret) from the connection's config file. The config is re-read on
// every call so edits take effect; the client is reused while the cluster
// definition is unchanged.
func ClientForConnection(conn *types.Connection) (*Client, error) {
	if conn == nil {
		return nil, errors.New("no connection in context")
	}
	file, _ := conn.Data[ConnectionDataConfigFile].(string)
	if file == "" {
		return nil, fmt.Errorf("connection %q has no %s", conn.ID, ConnectionDataConfigFile)
	}
	cluster, err := FindCluster(file, conn.ID)
	if err != nil {
		pkgLogger().Debugw(context.Background(), "proxmox cluster config reload failed",
			append([]any{"connection_id", conn.ID}, classFields(err)...)...)
		return nil, err
	}
	pkgLogger().Debugw(context.Background(), "proxmox cluster config reloaded", "connection_id", conn.ID, "cluster_id", cluster.ID)

	connClients.Lock()
	defer connClients.Unlock()
	if cached, ok := connClients.m[conn.ID]; ok {
		if cached.cluster == cluster {
			return cached.client, nil
		}
		pkgLogger().Infow(context.Background(), "proxmox client replaced", "connection_id", conn.ID, "cluster_id", cluster.ID)
		cached.client.Close()
	}
	client, err := NewClient(cluster)
	if err != nil {
		delete(connClients.m, conn.ID)
		pkgLogger().Warnw(context.Background(), "proxmox client build failed",
			append([]any{"connection_id", conn.ID, "cluster_id", cluster.ID}, classFields(err)...)...)
		return nil, err
	}
	connClients.m[conn.ID] = cachedClient{cluster: cluster, client: client}
	return client, nil
}
