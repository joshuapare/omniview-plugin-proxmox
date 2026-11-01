# Omniview Proxmox VE plugin

Browse and operate Proxmox VE clusters in Omniview: nodes, storage, QEMU virtual machines, LXC
containers and the cluster's Ceph (status, pools, OSDs, monitors, managers, metadata servers,
CephFS). Guests can be started, shut down, stopped, rebooted, edited and destroyed; nodes and guests
have metrics and shells; nodes and Ceph daemons have logs. See [Operations](#operations).

Requires Omniview 0.14 or later: the plugin is built on plugin-sdk 0.10 (resource API v2,
host-owned settings) and `@omniviewdev/runtime` 0.14.

## Setup

The plugin talks to the Proxmox VE API with an API token. One API endpoint serves the whole
cluster: `/cluster/resources` on any node lists every node, guest and storage, and Ceph requests are
forwarded to a node that runs Ceph. `PVEAuditor` covers browsing, Ceph and metrics; the
[operations](#operations) need more.

### 1. Create a read-only token

On any cluster node, as root:

```sh
pveum user add omniview@pve --comment "Omniview read-only"
pveum acl modify / --users omniview@pve --roles PVEAuditor
pveum user token add omniview@pve omniview --privsep 0 --comment "Omniview plugin"
```

The last command prints the token secret (`value`) once; copy it. `--privsep 0` gives the token
the user's `PVEAuditor` rights. With privilege separation on, the token needs its own ACL:
`pveum acl modify / --tokens 'omniview@pve!omniview' --roles PVEAuditor`.

### 2. Pick a URL the certificate covers

The plugin verifies TLS. The host in `url` must be a name or IP in the node's certificate:

```sh
echo | openssl s_client -connect NODE:8006 2>/dev/null | openssl x509 -noout -issuer -ext subjectAltName
```

| Certificate | Config |
| --- | --- |
| Publicly trusted (ACME / Let's Encrypt, e.g. a Tailscale `*.ts.net` cert) | `url` with a name from the certificate; no `ca_file` |
| Proxmox default, issued by the cluster CA | `ca_file` pointing at a copy of `/etc/pve/pve-root-ca.pem` |
| Neither works | `insecure_skip_verify: true` (last resort; disables verification) |

### 3. Write the config file

Each entry under `clusters:` becomes one connection. The default file is
`~/.config/omniview-proxmox/clusters.yaml` on the machine running Omniview; list more files under
the plugin's **Cluster config files** setting. A listed file that does not exist is skipped.

```sh
mkdir -p ~/.config/omniview-proxmox && chmod 700 ~/.config/omniview-proxmox
touch ~/.config/omniview-proxmox/clusters.yaml && chmod 600 ~/.config/omniview-proxmox/clusters.yaml
```

```yaml
clusters:
  - id: homelab                     # stable connection ID, unique across all files
    name: Homelab                   # optional display name, defaults to id
    url: https://pve1.example.ts.net:8006   # server root, no /api2/json
    token_id: omniview@pve!omniview
    token_secret: 00000000-0000-0000-0000-000000000000
    # ca_file: ~/.config/omniview-proxmox/homelab-ca.pem   # copy of /etc/pve/pve-root-ca.pem
    # insecure_skip_verify: true
    # console_user: omniview@pve                # optional, for shells; see below
    # console_password_file: ~/.config/omniview-proxmox/homelab-console.pw
```

Unknown keys, duplicate IDs and a `url` with a path are rejected. The token secret is read from the
file each time a connection starts and is never sent to the UI.

Shells (node shell, container console, VM serial console) need a user login: Proxmox VE consoles
reject API tokens ([bug 6079](https://bugzilla.proxmox.com/show_bug.cgi?id=6079)). To enable them,
set `console_user` (`USER@REALM`, not a token) and exactly one of `console_password` or
`console_password_file` (a file holding the password; surrounding whitespace is trimmed). Each shell
logs that user in with `POST /access/ticket`; the password is never sent to the UI or logged. The
user needs `Sys.Console` on `/nodes/{node}` for node shells and `VM.Console` on the guest for
consoles, and must not have two-factor authentication. Node shells run as that user, so any user
other than `root@pam` gets a login prompt. VM consoles use the serial port
(`qm set <vmid> -serial0 socket`).

The endpoint is a single node: if it is down, the connection fails even when the rest of the
cluster is up. For a fallback, add a second entry with its own `id` and another node's `url`.

### 4. Check the token from the Omniview machine

Keep the single quotes; `!` in double quotes triggers shell history expansion.

```sh
curl -H 'Authorization: PVEAPIToken=omniview@pve!omniview=SECRET' \
  https://pve1.example.ts.net:8006/api2/json/version
```

A JSON reply with `"version"` means the network path, certificate and token are good. Add
`--cacert FILE` when using `ca_file`.

### 5. Connect

Connections are loaded when the plugin starts, so restart Omniview (or, in a development
workspace, `omniview-dev plugin dev restart proxmox`) after adding or removing clusters. The
**Cluster config files** setting is likewise marked as requiring a restart: Omniview shows it as
pending until the plugin restarts. Edits to an existing entry apply on its next connect.

Open **Proxmox VE** in the sidebar and click the cluster row; it connects and opens the cluster's
[Summary](#summary-page). Connecting asks the cluster for `ceph status` once; the **Ceph** section
appears only if that succeeds, so a cluster without Ceph shows none (Ceph installed later appears on
the next connect). While connected the plugin issues four GET requests every 10 seconds, plus one
per Ceph view opened since connecting (the Summary page counts as the Ceph Status view), plus one
guest config read per VM or container each minute (see [Relationships](#relationships)).

A failed connect shows one of these errors:

| Error | Cause |
| --- | --- |
| Unauthorized | Wrong `token_id` or `token_secret`, or the token was removed |
| Forbidden | The token lacks `PVEAuditor` on `/` (see privilege separation above) |
| Certificate rejected | `url` host not in the certificate, or the issuer is not trusted; see step 2 |
| Host not found / Timed out / Unavailable | The node is not reachable from the Omniview machine |

### Revoking access

```sh
pveum user token remove omniview@pve omniview
pveum user delete omniview@pve
```

## Resources

| Resource type | Source |
| --- | --- |
| `proxmox::v1::Node` | `GET /cluster/resources?type=node` |
| `proxmox::v1::Storage` | `GET /cluster/resources?type=storage` |
| `proxmox::v1::VirtualMachine` | `GET /cluster/resources?type=vm`, `type == qemu` |
| `proxmox::v1::Container` | `GET /cluster/resources?type=vm`, `type == lxc` |
| `proxmox::v1::CephCluster` | `GET /nodes/{node}/ceph/status`, one object |
| `proxmox::v1::CephPool` | `GET /nodes/{node}/ceph/pool` |
| `proxmox::v1::CephOSD` | `GET /nodes/{node}/ceph/osd`, the `osd` leaves of the CRUSH tree |
| `proxmox::v1::CephMonitor` | `GET /nodes/{node}/ceph/mon` |
| `proxmox::v1::CephManager` | `GET /nodes/{node}/ceph/mgr` |
| `proxmox::v1::CephMetadataServer` | `GET /nodes/{node}/ceph/mds` |
| `proxmox::v1::CephFilesystem` | `GET /nodes/{node}/ceph/fs` |

Objects are the API entries, plus an `omniview` object on guests and storage (see
[Relationships](#relationships)). Cluster resources are identified by Proxmox's cluster-unique `id`
(`node/pve1`, `qemu/100`, `storage/pve1/local`); the types have no scope dimensions (nodes are not
mapped onto namespaces). Ceph objects have no such `id` and are identified by their Ceph name: the
cluster `fsid`, `pool_name`, or `name` (`osd.3`, monitor/manager/MDS IDs, file system name).
Integers keep their exact value end to end.

Connecting checks the API token (`GET /version`) and asks for `ceph status`; a rejected token or an
unreachable API fails the connection. The connection's catalog lists the four cluster types, plus
the Ceph types when an online node answered `ceph status`, so a cluster without Ceph has no Ceph
section. Ceph installed later shows up on the next connect.

Ceph state is cluster-wide but Proxmox serves it only from nodes with Ceph installed. A Ceph
request goes to the node that answered the previous one, falling back to every other online node
in name order. If none answers, the request fails as `UNAVAILABLE` ("Ceph unavailable") with each node's error,
e.g. when Ceph stops answering after connect.

The Ceph Status detail view summarises `ceph status` the way `ceph -s` does: health checks with
their detail, raw capacity, monitors in quorum, managers, OSDs up/in, metadata servers, placement
group states and client I/O. The JSON view keeps the full monitor and manager maps.

Proxmox has no change feed, so while a connection is open each kind is re-read every 10 seconds
and changes are pushed to the UI as best-effort updates. Cluster kinds are watched from connect; a
Ceph kind from the first time its view is opened (Ceph status also from the first Summary page of a
cluster with Ceph). An unreachable cluster shows as a watch error and recovers on the next
successful poll.

A type is read in pages of up to 256 objects, sorted by ID. Tables page through them; the Summary
page, Topology and the home page card use the first page of each kind and say so when a kind has
more.

Table columns are defined in `ui/src/utils/columns.ts`; the backend supplies no presentation
metadata.

## Relationships

The plugin declares relationships for Omniview's resource graph, which the host builds from the
watched objects:

| From | Relationship | To | Source |
| --- | --- | --- | --- |
| VirtualMachine, Container | `runs_on` | Node | the entry's `node` |
| VirtualMachine, Container | `uses` | Storage | volumes in the guest config |
| Storage | `runs_on` | Node | the entry's `node` |

Proxmox names nodes and storage by bare name, so the plugin writes the target IDs into
`omniview.node` (`node/pve1`) and `omniview.storage` (`["storage/pve1/local-lvm"]`) and the
declarations extract those fields. Storage usage comes from `GET /nodes/{node}/{qemu|lxc}/{vmid}/config`:
disks, EFI/TPM state, CD-ROM images, LXC root and mount points and unused disks, on the storage
entry of the guest's node. Pass-through devices and bind mounts have no storage. Storage content
listings are not used: for a `PVEAuditor` token Proxmox filters guest disks out of them. Each
guest config is re-read after a minute, or at once when the guest moves to another node; a config
that cannot be read keeps its last result, and a guest never read has no `omniview.storage`.

In the UI:

- **Topology** (connection sidebar): one section per node with the guests running on it, its
  storage, and which storage each guest uses. Each section is the node's upstream neighbourhood
  (`GetSubgraph`, two hops) in the host graph and refreshes when the graph changes.
- **Related** (resource drawer): a guest's node and storage, a node's guests and storage, a storage
  entry's node and guests.
- **Storage** column in the virtual machine and container tables.

## Operations

Guest actions are in the resource drawer's header and in each table row's **…** menu (also on
right-click). Each needs a privilege beyond `PVEAuditor`; without it the action fails with
**Forbidden** naming the privilege. `PVEVMAdmin` on `/vms` covers guest operations except
guest-agent commands on PVE 8, which require `VM.Monitor`.

| Operation | Kinds | API | Privilege |
| --- | --- | --- | --- |
| Start, Shutdown, Reboot, Stop, Suspend/Resume (VMs) | VM, CT | `POST …/status/{action}` | `VM.PowerMgmt` |
| Edit config | VM, CT | `GET`/`PUT …/config`, guarded by the config digest | `VM.Config.*` |
| Delete (type the VMID; guest must be stopped; purges jobs and unreferenced disks) | VM, CT | `DELETE /nodes/{node}/{qemu,lxc}/{vmid}` | `VM.Allocate` |
| Shell | Node, CT, VM (serial console) | `termproxy` + `vncwebsocket` | `Sys.Console` / `VM.Console`, plus a console login (see [setup](#3-write-the-config-file)) |
| Metrics (CPU, memory, network, disk I/O; node load, IO wait, root disk) | Node, VM, CT | `GET …/rrddata` | `Sys.Audit` / `VM.Audit` |
| Logs (journal of the node or of `ceph-osd@N`, `ceph-mon@…`, `ceph-mgr@…`, `ceph-mds@…`) | Node, Ceph OSD/Monitor/Manager/MDS | `GET /nodes/{node}/syslog` | `Sys.Syslog` |
| Create guest | VM, CT | `POST /nodes/{node}/{qemu,lxc}` | `VM.Allocate` |
| Clone guest | VM, CT | `POST …/clone` | `VM.Clone` |
| Migrate guest | VM, CT | `POST …/migrate` | `VM.Migrate` |
| Guest agent command | VM | `POST …/agent/exec`, `GET …/agent/exec-status` | `VM.GuestAgent.Unrestricted` (PVE 9) or `VM.Monitor` (PVE 8) |
| Create, delete or roll back snapshot | Snapshot | `POST`/`DELETE …/snapshot`, `POST …/snapshot/{name}/rollback` | `VM.Snapshot` / `VM.Snapshot.Rollback` |
| Task status and log | Task | `GET /cluster/tasks`, `GET /nodes/{node}/tasks/{upid}/status` and `/log` | PVE task access permissions |

The resource catalog exposes these operations to MCP clients. Guest creation requires an ID in
the form `qemu/<vmid>` or `lxc/<vmid>` and a `node` field in the input. Guest agent commands
require the QEMU guest agent. A command can continue after its timeout.

Writes wait up to a minute for their Proxmox task and report its failure; a task still running then
is reported as such. Config changes Proxmox cannot apply to a running guest stay pending until its
next start, as in the Proxmox web UI. The node drawer also has a **Guests** view of the guests on
that node with their CPU and memory.

## Summary page

A connection opens on **Summary** (connection sidebar, above Topology), built from the same
10-second poll as the tables:

- **Health banner**: offline nodes, storage at 85 % full or more (red from 95 %), storage
  unavailable on an online node, guests neither running nor stopped, failing Ceph health checks
  (muted ones left out), and a failed last refresh. Green when there is nothing to report.
- **Capacity**: CPU and memory of online nodes (CPU weighted by core count), with the vCPUs and
  memory configured on running guests; storage summed over available storage, shared storage
  counted once (storages on the same disk or Ceph pool each count, as in Proxmox's datacenter
  summary); Ceph usage, health, OSDs, monitor quorum and client I/O, on clusters where connecting
  found Ceph. The card is left out while Proxmox cannot reach Ceph.
- **Workloads**: nodes online, and running, stopped and other VMs and containers; templates are
  counted apart. Each card opens its table.
- **Nodes** (CPU, memory, guests running/total, uptime), **Storage** (fullest first), and the five
  running guests using the most CPU cores and the most memory. Rows open the resource drawer.

## Homepage card

The plugin adds a **Proxmox VE** card to the Omniview home page with two sections, both
toggleable in the card's settings (**Edit Layout** → gear):

- **connected**: one tile per open connection, with online nodes, running VMs and running
  containers (templates excluded), plus CPU and memory use across online nodes (CPU weighted by
  core count). Meters turn amber above 60 % and red above 85 %. The name opens the cluster; each
  count opens that table. Figures come from the same 10-second poll as the tables.
- **disconnected**: configured clusters that are not connected; clicking one connects and opens it.

## Development

```sh
make init       # pnpm install
make test       # go test + UI typecheck
make build      # build/bin/plugin and ui/dist
make package    # dist/proxmox-<os>_<arch>.tar.gz
```

### Development workspace

The plugin is developed in a named Omniview development workspace, which builds it, runs Vite
for UI hot reload and rebuilds the Go backend on save. From the Omniview host checkout, with the
collection at `$ROOT`:

```sh
DEV="$(task --silent dev:cli:build)"
"$DEV" repository add proxmox --source "$ROOT/omniview-plugin-proxmox" \
  --ref origin/main --directory omniview-plugin-proxmox --root "$ROOT"
"$DEV" workspace edit WORKSPACE --root "$ROOT" --add-repo proxmox --add-plugin proxmox --apply
"$DEV" plugin dev status proxmox --workspace "$ROOT/workspaces/WORKSPACE"
```

`--apply` rebuilds and restarts the whole workspace. Edit the workspace worktree
(`$ROOT/workspaces/WORKSPACE/omniview-plugin-proxmox`, on the workspace's feature branch), not the
source checkout. Each Go rebuild restarts the plugin and drops open connections; reconnect from
the cluster page. See `omniview/docs/development-cli.md` for the workspace commands.

## Releasing

Push a `vX.Y.Z` tag. `.github/workflows/publish.yaml` runs on the `homelab` runners, tests,
cross-compiles every platform, publishes `X.Y.Z` to the marketplace at the `OMNIVIEW_API_URL`
repository variable as `mononen/proxmox`, and creates a GitHub release with the archives. The tag
overrides the `version` in `plugin.yaml`.

The workflow needs the `OMNIVIEW_API_URL` and `OMNIVIEW_STORAGE_HOST` repository variables and the
`OMNIVIEW_API_KEY` secret. To build without publishing, run the workflow by hand with a version and
`dry-run` checked.
