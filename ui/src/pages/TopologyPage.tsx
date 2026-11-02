import { useMemo, useState } from 'react';
import { EmptyState, Spinner, Stack, StatusDot, Text, type StatusDotStatus } from '@omniviewdev/base-ui';
import { ResourceIcon, useResourceGraph } from '@omniviewdev/runtime';
import type { GraphNode } from '@omniviewdev/runtime/models';
import { LuCircleAlert, LuNetwork } from 'react-icons/lu';
import { useParams } from 'react-router';

import { PLUGIN_ID, RESOURCE_KEYS } from '../constants';
import { type ClusterObject, useClusterObjects } from '../hooks/useClusterObjects';
import { useOpenResource } from '../hooks/useOpenResource';
import { type ProxmoxObject, columnStatus, formatBytes } from '../utils/columns';
import { parseStorageId } from '../utils/refs';

const rootStyle = { height: '100%', minHeight: 0, overflow: 'auto', padding: 12 } as const;
const toolbarStyle = { padding: '0 4px 4px' } as const;
const centerStyle = { flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '48px 0' } as const;
const bandStyle = {
  border: '1px solid var(--ov-color-border-default)',
  borderRadius: 6,
  background: 'var(--ov-color-bg-surface)',
} as const;
const bandHeaderStyle = {
  display: 'flex',
  alignItems: 'center',
  gap: 8,
  padding: '8px 12px',
  borderBottom: '1px solid var(--ov-color-border-muted)',
  cursor: 'pointer',
} as const;
const bandBodyStyle = { padding: 12, overflowX: 'auto' } as const;
const countsStyle = { marginLeft: 'auto' } as const;
const columnLabelStyle = { fontSize: 10, letterSpacing: 0.5 } as const;

const BOX_W = 240;
const BOX_H = 36;
const ROW_GAP = 8;
const COLUMN_GAP = 180;
const LABEL_H = 18;
const STORAGE_X = BOX_W + COLUMN_GAP;
const WIDTH = STORAGE_X + BOX_W;
/** Characters of a name that fit in a box at 12px. */
const NAME_CHARS = 34;

const STATUS_COLORS: Record<StatusDotStatus, string> = {
  success: 'var(--ov-color-success)',
  danger: 'var(--ov-color-danger)',
  warning: 'var(--ov-color-warning)',
  info: 'var(--ov-color-info)',
  neutral: 'var(--ov-color-fg-subtle)',
  pending: 'var(--ov-color-fg-subtle)',
};
const statusColumn = { field: 'status', header: 'Status', status: true };
const USES_COLOR = 'var(--ov-color-discovery)';

type Box = {
  id: string;
  resourceKey: string;
  data?: ProxmoxObject;
  /** The listed object, for opening it; absent for one not listed (yet). */
  object?: ClusterObject;
  x: number;
  y: number;
  kicker: string;
  name: string;
  /** Guest running on another node, linked here only through a disk left on this node's storage. */
  elsewhere?: boolean;
};

type Link = { from: Box; to: Box; path: string };

/**
 * `/connection/:connectionId/topology` — every node of the cluster with the
 * guests running on it, its storage, and which storage holds each guest's
 * disks. Each node's section is that node's upstream neighbourhood in the
 * host's relationship graph.
 */
export default function TopologyPage() {
  const { connectionId = '' } = useParams<{ connectionId: string }>();
  const { objects, isLoading, error } = useClusterObjects(connectionId);
  const sortedNodes = useMemo(
    () =>
      [...objects.values()]
        .filter((o) => o.resourceKey === RESOURCE_KEYS.node)
        .sort((a, b) => String(a.data.node ?? a.id).localeCompare(String(b.data.node ?? b.id), undefined, { numeric: true })),
    [objects],
  );

  let body;
  if (error) {
    body = (
      <div style={centerStyle}>
        <EmptyState icon={<LuCircleAlert />} title="Failed to load the cluster" description={error} />
      </div>
    );
  } else if (isLoading) {
    body = (
      <div style={centerStyle}>
        <Spinner />
      </div>
    );
  } else if (sortedNodes.length === 0) {
    body = (
      <div style={centerStyle}>
        <EmptyState icon={<LuNetwork />} title="No nodes" description="Connect to the cluster to see its topology." />
      </div>
    );
  } else {
    body = sortedNodes.map((entry) => <NodeBand key={entry.id} entry={entry} objects={objects} />);
  }

  return (
    <Stack direction="column" spacing={3} style={rootStyle}>
      <Stack direction="row" align="center" spacing={2} style={toolbarStyle}>
        <LuNetwork size={18} />
        <Text weight="semibold">Topology</Text>
        <Text size="sm" tone="muted">
          Guests by node, linked to the storage holding their disks
        </Text>
      </Stack>
      {body}
    </Stack>
  );
}

type NodeBandProps = {
  entry: ClusterObject;
  objects: Map<string, ClusterObject>;
};

function NodeBand({ entry, objects }: NodeBandProps) {
  const node = entry.data;
  const open = useOpenResource();
  const [hovered, setHovered] = useState<string | null>(null);
  // Guests and storage point at their node; guests also point at storage.
  // Two hops upstream reach both, plus every guest → storage edge between them.
  const { graph, isLoading, error } = useResourceGraph({
    pluginID: PLUGIN_ID,
    reference: entry.record.record.reference,
    route: entry.record.route,
    upDepth: 2,
    downDepth: 0,
  });

  const layout = useMemo(() => {
    if (!graph) return null;
    const idOf = (n: GraphNode) => n.reference.address.id;
    const runsHere = new Set(
      graph.edges.filter((e) => e.type === 'runs_on' && idOf(e.target) === entry.id).map((e) => idOf(e.source)),
    );
    const guests: Box[] = [];
    const storage: Box[] = [];
    for (const { node: n } of graph.nodes) {
      const { id, type } = n.reference.address;
      const object = objects.get(id);
      if (type === RESOURCE_KEYS.storage) {
        storage.push({ ...storageBox(id, object?.data), object });
      } else if (type === RESOURCE_KEYS.virtualMachine || type === RESOURCE_KEYS.container) {
        guests.push({ ...guestBox(id, type, object?.data, !runsHere.has(id)), object });
      }
    }
    guests.sort(
      (a, b) =>
        Number(a.elsewhere ?? false) - Number(b.elsewhere ?? false) ||
        a.id.localeCompare(b.id, undefined, { numeric: true }),
    );
    storage.sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }));
    guests.forEach((box, i) => {
      box.y = LABEL_H + i * (BOX_H + ROW_GAP);
    });
    storage.forEach((box, i) => {
      box.x = STORAGE_X;
      box.y = LABEL_H + i * (BOX_H + ROW_GAP);
    });

    const byId = new Map([...guests, ...storage].map((b) => [b.id, b]));
    const links: Link[] = [];
    for (const e of graph.edges) {
      if (e.type !== 'uses') continue;
      const from = byId.get(idOf(e.source));
      const to = byId.get(idOf(e.target));
      if (!from || !to) continue;
      const x1 = from.x + BOX_W;
      const y1 = from.y + BOX_H / 2;
      const x2 = to.x;
      const y2 = to.y + BOX_H / 2;
      const mid = (x1 + x2) / 2;
      links.push({ from, to, path: `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}` });
    }
    const rows = Math.max(guests.length, storage.length, 1);
    return {
      boxes: [...guests, ...storage],
      links,
      guestCount: guests.filter((g) => !g.elsewhere).length,
      storageCount: storage.length,
      height: LABEL_H + rows * (BOX_H + ROW_GAP),
    };
  }, [graph, entry.id, objects]);

  const status = columnStatus(node.status, statusColumn);
  const neighbours = new Set<string>();
  if (hovered && layout) {
    neighbours.add(hovered);
    for (const l of layout.links) {
      if (l.from.id === hovered || l.to.id === hovered) {
        neighbours.add(l.from.id);
        neighbours.add(l.to.id);
      }
    }
  }

  let content;
  if (error) {
    content = (
      <Text size="sm" tone="danger">
        {error instanceof Error ? error.message : String(error)}
      </Text>
    );
  } else if (isLoading || !layout) {
    content = <Spinner size="sm" />;
  } else if (layout.boxes.length === 0) {
    content = (
      <Text size="sm" tone="muted">
        No guests or storage on this node.
      </Text>
    );
  } else {
    content = (
      <svg width={WIDTH} height={layout.height} role="img" aria-label={`Guests and storage of ${String(node.node ?? node.id)}`}>
        <defs>
          <marker id="pve-topology-arrow" viewBox="0 0 10 10" refX={9} refY={5} markerWidth={7} markerHeight={7} orient="auto">
            <path d="M 0 0 L 10 5 L 0 10 z" fill={USES_COLOR} />
          </marker>
        </defs>
        <text x={0} y={11} fill="var(--ov-color-fg-subtle)" style={columnLabelStyle}>
          GUESTS
        </text>
        <text x={STORAGE_X} y={11} fill="var(--ov-color-fg-subtle)" style={columnLabelStyle}>
          STORAGE
        </text>
        {layout.links.map((l) => {
          const active = hovered !== null && (l.from.id === hovered || l.to.id === hovered);
          return (
            <path
              key={`${l.from.id}>${l.to.id}`}
              d={l.path}
              fill="none"
              stroke={USES_COLOR}
              strokeWidth={active ? 2 : 1.25}
              strokeOpacity={hovered === null ? 0.6 : active ? 1 : 0.12}
              markerEnd="url(#pve-topology-arrow)"
            >
              <title>{`${l.from.name} stores disks on ${l.to.name}`}</title>
            </path>
          );
        })}
        {layout.boxes.map((box) => (
          <TopologyBox
            key={box.id}
            box={box}
            dimmed={hovered !== null && !neighbours.has(box.id)}
            onHover={setHovered}
            onOpen={() => box.object && open(box.object)}
          />
        ))}
      </svg>
    );
  }

  return (
    <section style={bandStyle}>
      <div
        style={bandHeaderStyle}
        role="button"
        tabIndex={0}
        onClick={() => open(entry)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            open(entry);
          }
        }}
      >
        <ResourceIcon pluginId={PLUGIN_ID} resourceKey={RESOURCE_KEYS.node} size={16} />
        <Text weight="semibold" size="sm">
          {String(node.node ?? node.id)}
        </Text>
        {status && <StatusDot status={status} label={String(node.status)} size="sm" />}
        {layout && (
          <Text size="xs" tone="muted" style={countsStyle}>
            {`${layout.guestCount} guests · ${layout.storageCount} storage${graph?.truncated ? ' · limit reached, some left out' : ''}`}
          </Text>
        )}
      </div>
      <div style={bandBodyStyle}>{content}</div>
    </section>
  );
}

function guestBox(id: string, resourceKey: string, data: ProxmoxObject | undefined, elsewhere: boolean): Box {
  const kind = resourceKey === RESOURCE_KEYS.container ? 'CT' : 'VM';
  const vmid = data?.vmid ?? id.split('/')[1];
  const where = elsewhere && data?.node ? ` · runs on ${String(data.node)}` : '';
  return {
    id,
    resourceKey,
    data,
    x: 0,
    y: 0,
    kicker: `${kind} ${String(vmid)}${data?.status ? ` · ${String(data.status)}` : ''}${where}`,
    name: data?.name ? String(data.name) : id,
    elsewhere,
  };
}

function storageBox(id: string, data: ProxmoxObject | undefined): Box {
  const { storage } = parseStorageId(id);
  const parts = [data?.plugintype, data?.shared === 1 ? 'shared' : undefined].filter(Boolean).map(String);
  if (typeof data?.disk === 'number' && typeof data?.maxdisk === 'number' && data.maxdisk > 0) {
    parts.push(`${formatBytes(data.disk)} / ${formatBytes(data.maxdisk)}`);
  }
  return { id, resourceKey: RESOURCE_KEYS.storage, data, x: 0, y: 0, kicker: parts.join(' · '), name: storage };
}

type TopologyBoxProps = {
  box: Box;
  dimmed: boolean;
  onHover: (id: string | null) => void;
  onOpen: () => void;
};

function TopologyBox({ box, dimmed, onHover, onOpen }: TopologyBoxProps) {
  const status = box.data ? columnStatus(box.data.status, statusColumn) : undefined;
  const name = box.name.length > NAME_CHARS ? `${box.name.slice(0, NAME_CHARS - 1)}…` : box.name;
  return (
    <g
      transform={`translate(${box.x}, ${box.y})`}
      role="button"
      tabIndex={0}
      aria-label={`${box.kicker} ${box.name}`}
      style={{ cursor: box.data ? 'pointer' : 'default', opacity: dimmed ? 0.35 : box.elsewhere ? 0.6 : 1 }}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onOpen();
        }
      }}
      onMouseEnter={() => onHover(box.id)}
      onMouseLeave={() => onHover(null)}
    >
      <title>{`${box.name}\n${box.kicker}`}</title>
      <rect
        width={BOX_W}
        height={BOX_H}
        rx={5}
        fill="var(--ov-color-bg-surface-raised)"
        stroke="var(--ov-color-border-default)"
        strokeDasharray={box.elsewhere ? '4 3' : undefined}
      />
      <circle cx={12} cy={BOX_H / 2} r={4} fill={status ? STATUS_COLORS[status] : STATUS_COLORS.neutral} />
      <text x={24} y={14} fontSize={10} fill="var(--ov-color-fg-muted)">
        {box.kicker}
      </text>
      <text x={24} y={29} fontSize={12} fill="var(--ov-color-fg-default)">
        {name}
      </text>
    </g>
  );
}
