import { useMemo, type ReactNode } from 'react';
import { Button, EmptyState, Meter, Spinner, StatusDot, Text } from '@omniviewdev/base-ui';
import { ResourceIcon, useConnection, useConnectionStatus, usePluginRouter, useResourceTypes } from '@omniviewdev/runtime';
import type { IconType } from 'react-icons';
import {
  LuBox,
  LuCircleAlert,
  LuCircleCheck,
  LuCircleX,
  LuCpu,
  LuDatabase,
  LuHardDrive,
  LuMemoryStick,
  LuMonitor,
  LuPlug,
  LuServer,
  LuTriangleAlert,
} from 'react-icons/lu';
import { Navigate, useParams } from 'react-router';

import { CEPH_USAGE_ZONES, PLUGIN_ID, RESOURCE_KEYS, USAGE_ZONES, resourcePath } from '../constants';
import { type ClusterObject, useClusterObjects } from '../hooks/useClusterObjects';
import { useOpenResource } from '../hooks/useOpenResource';
import { useProxmoxConnection } from '../hooks/useProxmoxConnection';
import { useProxmoxResources } from '../hooks/useProxmoxResources';
import { type CephStatusSummary, summarizeCephStatus } from '../utils/cephStatus';
import { type ClusterOverview, type GuestBreakdown, type GuestLoad, type Issue, analyzeCluster } from '../utils/clusterOverview';
import type { Capacity } from '../utils/clusterSummary';
import { formatBytes, formatDuration, statusOf } from '../utils/columns';
import styles from './OverviewPage.module.css';

const percent = (fraction: number) => Math.round(fraction * 100);

/** Connection index: open the overview. */
export function ConnectionIndexRedirect() {
  return <Navigate to="overview" replace />;
}

/**
 * `/connection/:connectionId/overview` — the connection's landing page: health,
 * capacity, workload counts, per-node load, storage and the busiest guests.
 * Everything comes from the watched `/cluster/resources` kinds, so it refreshes
 * with them, plus `ceph status` on clusters where connecting found Ceph.
 */
export default function OverviewPage() {
  const { connectionId = '' } = useParams<{ connectionId: string }>();
  const { connection } = useConnection({ pluginID: PLUGIN_ID, connectionID: connectionId });
  const { entries } = useConnectionStatus();
  const name = connection.data?.name || connectionId;
  const status = entries.find(
    (e) => e.pluginID === PLUGIN_ID && e.connectionID === connectionId && e.readiness === 'CONNECTED',
  );

  if (!status) return <NotConnected connectionId={connectionId} name={name} />;
  return <ConnectedOverview connectionId={connectionId} name={name} stale={status.hasErrors} />;
}

function NotConnected({ connectionId, name }: { connectionId: string; name: string }) {
  const { pending, connect } = useProxmoxConnection(connectionId, false);
  return (
    <div className={styles.Center}>
      <EmptyState
        icon={<LuPlug />}
        title={`${name} is not connected`}
        description="Connect to see the cluster's health, capacity and workloads."
        action={
          <Button size="sm" disabled={pending} onClick={connect} startDecorator={pending ? <Spinner size="xs" /> : undefined}>
            Connect
          </Button>
        }
      />
    </div>
  );
}

type ConnectedProps = { connectionId: string; name: string; stale: boolean };

function ConnectedOverview({ connectionId, name, stale }: ConnectedProps) {
  const cluster = useClusterObjects(connectionId);
  // Same query as the sidebar: the catalog has the Ceph types only when connecting found Ceph.
  const { resourceTypes } = useResourceTypes({ pluginID: PLUGIN_ID, route: { connection: connectionId } });
  const hasCeph = Boolean(resourceTypes.data?.some((t) => t.type === RESOURCE_KEYS.cephCluster));
  const { nodes, storage, virtualMachines, containers } = cluster;
  const overview = useMemo(
    () => analyzeCluster(nodes, storage, virtualMachines, containers),
    [nodes, storage, virtualMachines, containers],
  );

  if (cluster.error) {
    return (
      <div className={styles.Center}>
        <EmptyState icon={<LuCircleAlert />} title="Failed to load the cluster" description={cluster.error} />
      </div>
    );
  }
  if (cluster.isLoading) {
    return (
      <div className={styles.Center}>
        <Spinner />
      </div>
    );
  }

  const render = (ceph?: CephStatusSummary) => (
    <OverviewBody
      connectionId={connectionId}
      name={name}
      stale={stale}
      overview={overview}
      ceph={ceph}
      objects={cluster.objects}
    />
  );
  return hasCeph ? <WithCeph connectionId={connectionId}>{render}</WithCeph> : render();
}

/** Reads `ceph status`; while Proxmox cannot reach Ceph the page renders without it. */
function WithCeph({ connectionId, children }: { connectionId: string; children: (ceph?: CephStatusSummary) => ReactNode }) {
  const { entries, error } = useProxmoxResources(connectionId, RESOURCE_KEYS.cephCluster);
  const status = entries[0]?.data;
  const ceph = useMemo(() => (status ? summarizeCephStatus(status) : undefined), [status]);
  return children(error ? undefined : ceph);
}

type BodyProps = ConnectedProps & {
  overview: ClusterOverview;
  ceph?: CephStatusSummary;
  objects: Map<string, ClusterObject>;
};

function OverviewBody({ connectionId, name, stale, overview, ceph, objects }: BodyProps) {
  const { navigate } = usePluginRouter();
  const open = useOpenResource();
  const openObject = (id: string) => {
    const object = objects.get(id);
    if (object) open(object);
  };
  const openTable = (resourceKey: string) => navigate(resourcePath(connectionId, resourceKey));
  const { summary, allocated } = overview;

  const issues: Issue[] = [
    ...(stale ? [{ severity: 'warning' as const, message: 'The last refresh failed; figures may be stale' }] : []),
    ...overview.issues,
    ...(ceph?.health.checks ?? [])
      .filter((check) => !check.muted)
      .map((check): Issue => ({ severity: check.severity === 'HEALTH_ERR' ? 'danger' : 'warning', message: `Ceph: ${check.summary}` })),
  ];
  const guestCount = summary.virtualMachines.total + summary.containers.total;

  return (
    <div className={styles.Root}>
      <div className={styles.Header}>
        <span className={styles.Title}>{name}</span>
        <Text size="sm" tone="muted">
          {`${summary.nodes.total} ${summary.nodes.total === 1 ? 'node' : 'nodes'} · ${guestCount} ${guestCount === 1 ? 'guest' : 'guests'} · refreshed every 10 s`}
        </Text>
      </div>

      <HealthBanner issues={issues} />

      <div className={styles.Grid}>
        <CapacityCard
          icon={LuCpu}
          label="CPU"
          fraction={summary.cpu}
          detail={`${summary.cores.used.toFixed(1)} of ${summary.cores.total} cores busy`}
          footer={`${allocated.vcpus} vCPUs on running guests${summary.cores.total > 0 ? ` (${(allocated.vcpus / summary.cores.total).toFixed(1)}× cores)` : ''}`}
          onClick={() => openTable(RESOURCE_KEYS.node)}
        />
        <CapacityCard
          icon={LuMemoryStick}
          label="Memory"
          fraction={summary.memory}
          detail={capacityText(summary.memoryBytes)}
          footer={`${formatBytes(allocated.memory)} assigned to running guests`}
          onClick={() => openTable(RESOURCE_KEYS.node)}
        />
        <CapacityCard
          icon={LuHardDrive}
          label="Storage"
          fraction={overview.storage.total > 0 ? overview.storage.used / overview.storage.total : undefined}
          detail={capacityText(overview.storage)}
          footer={`Sum of ${overview.storageEntries.filter((s) => s.available).length} available storages; shared ones counted once`}
          onClick={() => openTable(RESOURCE_KEYS.storage)}
        />
        {ceph && <CephCard ceph={ceph} onClick={() => openTable(RESOURCE_KEYS.cephCluster)} />}
      </div>

      <div className={styles.Grid}>
        <button type="button" className={styles.Card} onClick={() => openTable(RESOURCE_KEYS.node)}>
          <span className={styles.CardLabel}>
            <LuServer size={14} aria-hidden />
            Nodes
          </span>
          <span className={styles.CardValue}>
            {summary.nodes.up}
            <small>of {summary.nodes.total} online</small>
          </span>
          <span className={styles.CardDetail} data-alert={summary.nodes.up < summary.nodes.total || undefined}>
            {summary.nodes.up < summary.nodes.total ? `${summary.nodes.total - summary.nodes.up} offline` : 'All online'}
          </span>
        </button>
        <GuestCard
          icon={LuMonitor}
          label="Virtual machines"
          guests={overview.virtualMachines}
          onClick={() => openTable(RESOURCE_KEYS.virtualMachine)}
        />
        <GuestCard icon={LuBox} label="Containers" guests={overview.containers} onClick={() => openTable(RESOURCE_KEYS.container)} />
      </div>

      <div className={styles.Columns}>
        <Panel icon={LuServer} title="Nodes">
          <div className={styles.Table} data-columns="nodes">
            <div className={styles.Head}>
              <span>Node</span>
              <span>CPU</span>
              <span>Memory</span>
              <span className={styles.Right}>Guests</span>
              <span className={styles.Right}>Uptime</span>
            </div>
            {overview.nodes.map((n) => (
              <button
                key={n.entry.id}
                type="button"
                className={styles.Row}
                title={`${n.name}: ${n.cores} cores, ${capacityText(n.memory)} memory`}
                onClick={() => openObject(n.entry.id)}
              >
                <span className={styles.Name}>
                  <StatusDot status={statusOf(n.entry.status)} size="sm" />
                  <span>{n.name}</span>
                </span>
                <Usage label={`${n.name} CPU`} fraction={n.cpu} />
                <Usage label={`${n.name} memory`} fraction={n.online && n.memory.total > 0 ? n.memory.used / n.memory.total : undefined} />
                <span className={styles.Right}>
                  {n.guests.running}
                  <span className={styles.Muted}>/{n.guests.total}</span>
                </span>
                <span className={`${styles.Right} ${styles.Muted}`}>{n.online ? formatDuration(n.uptime) : String(n.entry.status ?? '')}</span>
              </button>
            ))}
          </div>
        </Panel>

        <Panel icon={LuHardDrive} title="Storage">
          {overview.storageEntries.length === 0 ? (
            <Text as="div" size="sm" tone="muted" className={styles.PanelEmpty}>
              No storage configured.
            </Text>
          ) : (
            <div className={styles.Table} data-columns="storage">
              <div className={styles.Head}>
                <span>Storage</span>
                <span>Used</span>
                <span className={styles.Right}>Size</span>
              </div>
              {overview.storageEntries.map((s) => (
                <button
                  key={s.entry.id}
                  type="button"
                  className={styles.Row}
                  title={`${s.label}: ${s.type}${s.node ? '' : ', shared'}`}
                  onClick={() => openObject(s.entry.id)}
                >
                  <span className={styles.Name}>
                    <StatusDot status={statusOf(s.entry.status)} size="sm" />
                    <span>
                      {s.name} <span className={styles.Muted}>{s.node ?? 'shared'}</span>
                    </span>
                  </span>
                  <Usage label={`${s.label} used`} fraction={s.available ? s.fraction : undefined} />
                  <span className={`${styles.Right} ${styles.Muted}`}>{s.available && s.total > 0 ? capacityText(s) : '—'}</span>
                </button>
              ))}
            </div>
          )}
        </Panel>
      </div>

      <div className={styles.Columns}>
        <GuestPanel
          title="Busiest guests by CPU"
          icon={LuCpu}
          guests={overview.topCpu}
          fraction={(g) => g.cpu}
          amount={(g) => `${g.cores.toFixed(2)} of ${g.vcpus} vCPUs`}
          onOpen={openObject}
        />
        <GuestPanel
          title="Largest guests by memory"
          icon={LuMemoryStick}
          guests={overview.topMemory}
          fraction={(g) => (g.memory.total > 0 ? g.memory.used / g.memory.total : undefined)}
          amount={(g) => capacityText(g.memory)}
          onOpen={openObject}
        />
      </div>
    </div>
  );
}

function capacityText({ used, total }: Capacity): string {
  return `${formatBytes(used)} of ${formatBytes(total)}`;
}

function HealthBanner({ issues }: { issues: Issue[] }) {
  if (issues.length === 0) {
    return (
      <div className={styles.Banner} role="status">
        <LuCircleCheck size={16} aria-hidden />
        <span>All nodes online, all storage available and under 85 % full, every guest running or stopped.</span>
      </div>
    );
  }
  const danger = issues.some((i) => i.severity === 'danger');
  const Icon = danger ? LuCircleX : LuTriangleAlert;
  return (
    <div className={styles.Banner} data-severity={danger ? 'danger' : 'warning'} role="status">
      <Icon size={16} aria-hidden />
      <ul className={styles.Issues}>
        {issues.map((issue) => (
          <li key={issue.message}>{issue.message}</li>
        ))}
      </ul>
    </div>
  );
}

type CapacityCardProps = {
  icon: IconType;
  label: string;
  /** 0–1; undefined while nothing is online to measure. */
  fraction?: number;
  detail: string;
  footer: string;
  onClick: () => void;
};

function CapacityCard({ icon: Icon, label, fraction, detail, footer, onClick }: CapacityCardProps) {
  return (
    <button type="button" className={styles.Card} onClick={onClick}>
      <span className={styles.CardLabel}>
        <Icon size={14} aria-hidden />
        {label}
      </span>
      <span className={styles.CardValue}>{fraction === undefined ? '—' : `${percent(fraction)} %`}</span>
      <span className={styles.CardDetail}>{detail}</span>
      <Meter
        size="sm"
        value={fraction === undefined ? 0 : percent(fraction)}
        {...USAGE_ZONES}
        aria-label={`${label} ${fraction === undefined ? 'unknown' : `${percent(fraction)}%`}`}
        className={styles.CardMeter}
      />
      <span className={styles.CardDetail}>{footer}</span>
    </button>
  );
}

function CephCard({ ceph, onClick }: { ceph: CephStatusSummary; onClick: () => void }) {
  const { capacity, osds, monitors, io } = ceph;
  const fraction = capacity ? capacity.used / capacity.total : undefined;
  const parts: string[] = [];
  if (osds) parts.push(`${osds.up}/${osds.total} OSDs up, ${osds.in} in`);
  if (monitors) parts.push(`${monitors.inQuorum.length}/${monitors.total} mons in quorum`);
  if (io && (io.readBytes > 0 || io.writeBytes > 0)) parts.push(`${formatBytes(io.readBytes)}/s rd, ${formatBytes(io.writeBytes)}/s wr`);
  const degraded = (osds && osds.up < osds.total) || (monitors && monitors.inQuorum.length < monitors.total);
  return (
    <button type="button" className={styles.Card} onClick={onClick}>
      <span className={styles.CardLabel}>
        <LuDatabase size={14} aria-hidden />
        Ceph
        <StatusDot status={statusOf(ceph.health.status)} label={ceph.health.status} size="sm" />
      </span>
      <span className={styles.CardValue}>{fraction === undefined ? '—' : `${percent(fraction)} %`}</span>
      <span className={styles.CardDetail}>{capacity ? capacityText(capacity) : 'Capacity unknown'}</span>
      <Meter
        size="sm"
        value={fraction === undefined ? 0 : percent(fraction)}
        {...CEPH_USAGE_ZONES}
        aria-label={`Ceph ${fraction === undefined ? 'unknown' : `${percent(fraction)}%`}`}
        className={styles.CardMeter}
      />
      <span className={styles.CardDetail} data-alert={degraded || undefined}>
        {parts.join(' · ')}
      </span>
    </button>
  );
}

type GuestCardProps = { icon: IconType; label: string; guests: GuestBreakdown; onClick: () => void };

function GuestCard({ icon: Icon, label, guests, onClick }: GuestCardProps) {
  const parts = [`${guests.stopped} stopped`];
  if (guests.other > 0) parts.push(`${guests.other} other`);
  if (guests.templates > 0) parts.push(`${guests.templates} ${guests.templates === 1 ? 'template' : 'templates'}`);
  return (
    <button type="button" className={styles.Card} onClick={onClick}>
      <span className={styles.CardLabel}>
        <Icon size={14} aria-hidden />
        {label}
      </span>
      <span className={styles.CardValue}>
        {guests.running}
        <small>running</small>
      </span>
      <span className={styles.CardDetail} data-alert={guests.other > 0 || undefined}>
        {parts.join(' · ')}
      </span>
    </button>
  );
}

function Panel({ icon: Icon, title, children }: { icon: IconType; title: string; children: ReactNode }) {
  return (
    <section className={styles.Panel}>
      <div className={styles.PanelHeader}>
        <Icon size={14} aria-hidden />
        <Text size="sm" weight="semibold">
          {title}
        </Text>
      </div>
      {children}
    </section>
  );
}

/** A usage meter with its percentage; a dash when unknown (offline node, unavailable storage). */
function Usage({ label, fraction }: { label: string; fraction?: number }) {
  if (fraction === undefined) return <span className={styles.Muted}>—</span>;
  const pct = percent(fraction);
  return (
    <span className={styles.Usage}>
      <Meter size="sm" value={Math.min(pct, 100)} {...USAGE_ZONES} aria-label={`${label} ${pct}%`} />
      <span>{pct} %</span>
    </span>
  );
}

type GuestPanelProps = {
  title: string;
  icon: IconType;
  guests: GuestLoad[];
  fraction: (g: GuestLoad) => number | undefined;
  amount: (g: GuestLoad) => string;
  /** Opens the guest with this Proxmox ID. */
  onOpen: (id: string) => void;
};

function GuestPanel({ title, icon, guests, fraction, amount, onOpen }: GuestPanelProps) {
  return (
    <Panel icon={icon} title={title}>
      {guests.length === 0 ? (
        <Text as="div" size="sm" tone="muted" className={styles.PanelEmpty}>
          No running guests.
        </Text>
      ) : (
        <div className={styles.Table} data-columns="guests">
          {guests.map((g) => (
            <button key={g.entry.id} type="button" className={styles.Row} onClick={() => onOpen(g.entry.id)}>
              <span className={styles.Name}>
                <ResourceIcon pluginId={PLUGIN_ID} resourceKey={g.resourceKey} size={14} />
                <span title={g.name}>
                  {g.name} <span className={styles.Muted}>{String(g.entry.node ?? '')}</span>
                </span>
              </span>
              <Usage label={`${g.name} ${title}`} fraction={fraction(g)} />
              <span className={`${styles.Right} ${styles.Muted}`}>{amount(g)}</span>
            </button>
          ))}
        </div>
      )}
    </Panel>
  );
}
