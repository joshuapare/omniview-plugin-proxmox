import { useMemo, type ReactNode } from 'react';
import { IconButton, Meter, Spinner, StatusDot, Text } from '@omniviewdev/base-ui';
import {
  useConnectionStatus,
  useConnections,
  usePluginRouter,
  type HomepageCardMeta,
  type HomepageCardProps,
} from '@omniviewdev/runtime';
import type { types } from '@omniviewdev/runtime/models';
import type { IconType } from 'react-icons';
import { LuBox, LuMonitor, LuServer, LuUnplug } from 'react-icons/lu';
import { SiProxmox } from 'react-icons/si';

import { PLUGIN_ID, RESOURCE_KEYS, USAGE_ZONES, connectionPath, resourcePath } from '../../constants';
import { useProxmoxConnection } from '../../hooks/useProxmoxConnection';
import { useProxmoxResources } from '../../hooks/useProxmoxResources';
import { summarizeCluster, type Count } from '../../utils/clusterSummary';
import styles from './ProxmoxHomepageCard.module.css';

export const proxmoxHomepageCardMeta: HomepageCardMeta = {
  label: 'Proxmox VE',
  description: 'Connected clusters with guest counts and load; connect to the others',
  defaultConfig: { sections: ['connected', 'disconnected'], maxItems: 5 },
  defaultWidth: 'medium',
  icon: SiProxmox,
};

const loadingStyle = { display: 'flex', justifyContent: 'center', padding: 24 } as const;

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div>
      <Text as="div" size="xs" className={styles.SectionTitle}>
        {title}
      </Text>
      {children}
    </div>
  );
}

function Note({ children, tone = 'subtle' }: { children: ReactNode; tone?: 'subtle' | 'danger' }) {
  return (
    <Text as="div" size="xs" tone={tone} className={styles.Note}>
      {children}
    </Text>
  );
}

/** Homepage card: configured Proxmox VE clusters, split into connected and disconnected. */
export default function ProxmoxHomepageCard({ config }: HomepageCardProps) {
  const maxItems = config.maxItems ?? 5;
  const sections = Array.isArray(config.sections) ? config.sections : [];
  const { connections } = useConnections({ plugin: PLUGIN_ID });
  const { entries } = useConnectionStatus();

  const statusById = useMemo(
    () =>
      new Map(
        entries
          .filter((e) => e.pluginID === PLUGIN_ID && e.readiness === 'CONNECTED')
          .map((e) => [e.connectionID, e]),
      ),
    [entries],
  );
  const sorted = useMemo(
    () => [...(connections.data ?? [])].sort((a, b) => (a.name || a.id).localeCompare(b.name || b.id)),
    [connections.data],
  );

  if (connections.isLoading) {
    return (
      <div style={loadingStyle}>
        <Spinner size="lg" />
      </div>
    );
  }
  if (connections.error) {
    return <Note tone="danger">Failed to load connections: {connections.error.message}</Note>;
  }
  if (sorted.length === 0) {
    return <Note>No clusters configured. Clusters are read from the config files listed in this plugin's settings.</Note>;
  }

  const connected = sorted.filter((c) => statusById.has(c.id));
  const disconnected = sorted.filter((c) => !statusById.has(c.id));

  return (
    <div>
      {sections.includes('connected') && (
        <Section title="Connected">
          {connected.length === 0 ? (
            <Note>No clusters connected</Note>
          ) : (
            <div className={styles.TileGrid}>
              {connected.slice(0, maxItems).map((c) => (
                <ConnectedClusterTile key={c.id} connection={c} hasErrors={statusById.get(c.id)?.hasErrors ?? false} />
              ))}
            </div>
          )}
        </Section>
      )}
      {sections.includes('disconnected') && (
        <Section title="Disconnected">
          {disconnected.length === 0 ? (
            <Note>All clusters connected</Note>
          ) : (
            <div className={styles.ChipGrid}>
              {disconnected.slice(0, maxItems).map((c) => (
                <DisconnectedClusterChip key={c.id} connection={c} />
              ))}
            </div>
          )}
        </Section>
      )}
    </div>
  );
}

function ConnectedClusterTile({ connection, hasErrors }: { connection: types.Connection; hasErrors: boolean }) {
  const name = connection.name || connection.id;
  const { navigate } = usePluginRouter();
  const { pending, disconnect } = useProxmoxConnection(connection.id, true);

  const nodes = useProxmoxResources(connection.id, RESOURCE_KEYS.node);
  const virtualMachines = useProxmoxResources(connection.id, RESOURCE_KEYS.virtualMachine);
  const containers = useProxmoxResources(connection.id, RESOURCE_KEYS.container);
  const queries = [nodes, virtualMachines, containers];
  const loading = queries.some((q) => q.isLoading);
  const error = queries.find((q) => q.error)?.error;

  const summary = useMemo(
    () =>
      summarizeCluster(
        nodes.entries.map((e) => e.data),
        virtualMachines.entries.map((e) => e.data),
        containers.entries.map((e) => e.data),
      ),
    [nodes.entries, virtualMachines.entries, containers.entries],
  );

  const open = (resourceKey: string) => navigate(resourcePath(connection.id, resourceKey));

  return (
    <div className={styles.Tile}>
      <div className={styles.TileHeader}>
        <StatusDot status={hasErrors || error ? 'danger' : 'success'} size="sm" />
        <button type="button" className={`${styles.Link} ${styles.Name}`} title={name} onClick={() => navigate(connectionPath(connection.id))}>
          <span>{name}</span>
        </button>
        <IconButton
          size="xs"
          variant="ghost"
          color="neutral"
          aria-label={`Disconnect ${name}`}
          title="Disconnect"
          className={styles.Disconnect}
          disabled={pending}
          onClick={disconnect}
        >
          <LuUnplug size={12} />
        </IconButton>
      </div>

      {error ? (
        <Text size="xs" className={styles.Problem}>
          {error}
        </Text>
      ) : loading ? (
        <Spinner size="xs" />
      ) : (
        <>
          {hasErrors && (
            <Text size="xs" className={styles.Problem}>
              Last refresh failed; figures may be stale
            </Text>
          )}
          <div className={styles.Counts}>
            <CountLink
              icon={LuServer}
              count={summary.nodes}
              noun="nodes"
              upLabel="online"
              down={summary.nodes.up < summary.nodes.total}
              onClick={() => open(RESOURCE_KEYS.node)}
            />
            <CountLink icon={LuMonitor} count={summary.virtualMachines} noun="VMs" upLabel="running" onClick={() => open(RESOURCE_KEYS.virtualMachine)} />
            <CountLink icon={LuBox} count={summary.containers} noun="CTs" upLabel="running" onClick={() => open(RESOURCE_KEYS.container)} />
          </div>
          {(summary.cpu !== undefined || summary.memory !== undefined) && (
            <div className={styles.Usage}>
              {summary.cpu !== undefined && <UsageRow label="CPU" fraction={summary.cpu} />}
              {summary.memory !== undefined && <UsageRow label="Mem" fraction={summary.memory} />}
            </div>
          )}
        </>
      )}
    </div>
  );
}

type CountLinkProps = {
  icon: IconType;
  count: Count;
  noun: string;
  upLabel: string;
  down?: boolean;
  onClick: () => void;
};

function CountLink({ icon: Icon, count, noun, upLabel, down, onClick }: CountLinkProps) {
  return (
    <button
      type="button"
      className={`${styles.Link} ${styles.Count}`}
      data-down={down || undefined}
      title={`${count.up} of ${count.total} ${noun} ${upLabel}`}
      onClick={onClick}
    >
      <Icon size={12} aria-hidden />
      {count.up}/{count.total} {noun}
    </button>
  );
}

function UsageRow({ label, fraction }: { label: string; fraction: number }) {
  const pct = Math.round(fraction * 100);
  return (
    <>
      <span className={styles.UsageLabel}>{label}</span>
      <Meter size="sm" value={pct} {...USAGE_ZONES} aria-label={`${label} ${pct}%`} />
      <span className={styles.UsageValue}>{pct}%</span>
    </>
  );
}

function DisconnectedClusterChip({ connection }: { connection: types.Connection }) {
  const name = connection.name || connection.id;
  const { pending, open } = useProxmoxConnection(connection.id, false);

  return (
    <button type="button" className={styles.Chip} title={`Connect to ${name}`} disabled={pending} onClick={open}>
      {pending ? <Spinner size="xs" /> : <StatusDot status="neutral" size="sm" />}
      <span>{name}</span>
    </button>
  );
}
