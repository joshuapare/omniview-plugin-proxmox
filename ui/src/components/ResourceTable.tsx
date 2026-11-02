import { useMemo, useState, type ComponentProps } from 'react';
import {
  ContextMenu,
  EmptyState,
  IconButton,
  SearchInput,
  SortableHeader,
  Spinner,
  Stack,
  StatusDot,
  Table,
  Text,
  useSortableTable,
  type SortableColumnDef,
} from '@omniviewdev/base-ui';
import { type DrawerContext, ResourceIcon, useRightDrawer } from '@omniviewdev/runtime';
import { LuChevronLeft, LuChevronRight, LuCircleAlert, LuRefreshCw } from 'react-icons/lu';

import { bindActionDeps } from '../actions/actionDeps';
import { resourceActions } from '../actions/resourceActions';
import { useActionExecution } from '../actions/useActionExecution';
import { PAGE_SIZE, PLUGIN_ID } from '../constants';
import { useOpenResource } from '../hooks/useOpenResource';
import { type ProxmoxEntry, drawerContextOf, useProxmoxResources } from '../hooks/useProxmoxResources';
import { RowActionsButton, RowContextMenuPopup, menuEntries } from './RowActions';
import {
  type Column,
  type Kind,
  type NativeObject,
  cellValue,
  columnStatus,
  displayName,
  formatValue,
  isNumeric,
  kindFor,
  sortValue,
} from '../utils/columns';

const rootStyle = { height: '100%', minHeight: 0, padding: 8 } as const;
const toolbarStyle = { padding: '0 4px' } as const;
const tableContainerStyle = { flex: 1, minHeight: 0, overflow: 'auto' } as const;
const centerStyle = { flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' } as const;
const actionsCellStyle = { width: 36, paddingBlock: 0 } as const;
const rowGroupStyle = { display: 'table-row-group' } as const;

type Props = {
  connectionId: string;
  resourceKey: string;
  label: string;
};

/**
 * Table for one Proxmox resource kind, one page of PAGE_SIZE objects at a
 * time. Rows are the decoded native objects; columns come from kindFor().
 */
export default function ResourceTable({ connectionId, resourceKey, label }: Props) {
  const [filter, setFilter] = useState('');
  const [pageIndex, setPageIndex] = useState(0);
  const kind = kindFor(resourceKey);

  const resources = useProxmoxResources(connectionId, resourceKey);
  const rows = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    if (!needle) return resources.entries;
    return resources.entries.filter(
      (entry) =>
        entry.id.toLowerCase().includes(needle) ||
        kind.columns.some((col) => formatValue(cellValue(entry.data, col), col).toLowerCase().includes(needle)),
    );
  }, [resources.entries, filter, kind]);
  const sortColumns = useMemo<Array<SortableColumnDef<ProxmoxEntry>>>(
    () =>
      kind.columns.map((col) => ({
        id: col.field,
        header: col.header,
        accessor: (row) => sortValue(cellValue(row.data, col)),
      })),
    [kind.columns],
  );
  const { sortedData, sort, onSort } = useSortableTable({ data: rows, columns: sortColumns });

  const total = rows.length;
  const currentPage = Math.min(pageIndex, Math.max(0, Math.ceil(total / PAGE_SIZE) - 1));
  const first = currentPage * PAGE_SIZE;
  const pageRows = sortedData.slice(first, first + PAGE_SIZE);
  const paged = total > PAGE_SIZE;
  const pageLabel = `${first + 1}–${first + pageRows.length} of ${total}`;

  let body;
  if (resources.error) {
    body = (
      <div style={centerStyle}>
        <EmptyState icon={<LuCircleAlert />} title={`Failed to load ${label.toLowerCase()}`} description={resources.error} />
      </div>
    );
  } else if (resources.isLoading) {
    body = (
      <div style={centerStyle}>
        <Spinner />
      </div>
    );
  } else if (rows.length === 0 && !filter) {
    body = (
      <div style={centerStyle}>
        <EmptyState
          icon={<ResourceIcon pluginId={PLUGIN_ID} resourceKey={resourceKey} size={24} />}
          title={`No ${label.toLowerCase()}`}
        />
      </div>
    );
  } else {
    body = <SortedTable resourceKey={resourceKey} kind={kind} rows={pageRows} sort={sort} onSort={onSort} />;
  }

  return (
    <Stack direction="column" spacing={2} style={rootStyle}>
      <Stack direction="row" align="center" justify="between" spacing={2} style={toolbarStyle}>
        <Stack direction="row" align="center" spacing={2}>
          <ResourceIcon pluginId={PLUGIN_ID} resourceKey={resourceKey} size={18} />
          <Text weight="semibold">{label}</Text>
          {!resources.isLoading && !resources.error && (
            <Text size="sm" tone="muted">
              {paged ? pageLabel : rows.length}
            </Text>
          )}
        </Stack>
        <Stack direction="row" align="center" spacing={1}>
          <SearchInput
            size="sm"
            placeholder={`Filter ${label.toLowerCase()}`}
            value={filter}
            onValueChange={(value) => { setFilter(value); setPageIndex(0); }}
            clearable
          />
          {paged && (
            <>
              <IconButton
                size="sm"
                variant="ghost"
                color="neutral"
                aria-label="Previous page"
                disabled={currentPage === 0}
                onClick={() => setPageIndex(currentPage - 1)}
              >
                <LuChevronLeft />
              </IconButton>
              <IconButton
                size="sm"
                variant="ghost"
                color="neutral"
                aria-label="Next page"
                disabled={first + PAGE_SIZE >= total}
                onClick={() => setPageIndex(currentPage + 1)}
              >
                <LuChevronRight />
              </IconButton>
            </>
          )}
          <IconButton size="sm" variant="ghost" color="neutral" aria-label="Refresh" onClick={resources.refresh}>
            <LuRefreshCw />
          </IconButton>
        </Stack>
      </Stack>
      {body}
    </Stack>
  );
}

type SortedTableProps = {
  resourceKey: string;
  kind: Kind;
  rows: ProxmoxEntry[];
  sort: ComponentProps<typeof SortableHeader>['sort'];
  onSort: ComponentProps<typeof SortableHeader>['onSort'];
};

function SortedTable({ resourceKey, kind, rows, sort, onSort }: SortedTableProps) {
  const { isOpen } = useRightDrawer();
  const open = useOpenResource();
  const [selectedId, setSelectedId] = useState<string>();
  const [menuRowId, setMenuRowId] = useState<string>();
  const { columns } = kind;
  const actions = resourceActions(resourceKey);
  const { deps, dialogs } = useActionExecution();

  // Row menus run the drawer's actions against a context bound to this table's handlers.
  const contexts = useMemo(() => {
    const byId = new Map<string, DrawerContext<NativeObject>>();
    for (const row of rows) {
      byId.set(row.id, bindActionDeps(drawerContextOf(row, displayName(row.data, kind)), deps));
    }
    return byId;
  }, [rows, kind, deps]);

  const openResource = (row: ProxmoxEntry) => {
    setSelectedId(row.id);
    open(row);
  };

  const hasActions = actions.length > 0;
  const tableRows = rows.map((row) => {
    const ctx = contexts.get(row.id);
    return (
      <Table.Row
        key={row.id}
        interactive
        selected={isOpen && row.id === selectedId}
        onClick={() => openResource(row)}
        onContextMenu={hasActions ? () => setMenuRowId(row.id) : undefined}
      >
        {columns.map((col) => (
          <ResourceCell key={col.field} row={row.data} col={col} />
        ))}
        {hasActions && (
          <Table.Cell align="right" style={actionsCellStyle}>
            {ctx && <RowActionsButton entries={menuEntries(actions, ctx)} />}
          </Table.Cell>
        )}
      </Table.Row>
    );
  });
  const menuCtx = menuRowId === undefined ? undefined : contexts.get(menuRowId);

  return (
    <Table.Container style={tableContainerStyle}>
      <Table size="sm" stickyHeader hoverable>
        <Table.Head>
          <Table.Row>
            {columns.map((col) => (
              <Table.HeaderCell key={col.field} align={isNumeric(col) ? 'right' : 'left'}>
                <SortableHeader columnId={col.field} sort={sort} onSort={onSort}>
                  {col.header}
                </SortableHeader>
              </Table.HeaderCell>
            ))}
            {hasActions && <Table.HeaderCell style={actionsCellStyle} aria-label="Actions" />}
          </Table.Row>
        </Table.Head>
        {hasActions ? (
          <ContextMenu.Root>
            {/* The trigger's own class makes it a block; keep the tbody a row group. */}
            <ContextMenu.Trigger render={<Table.Body />} style={rowGroupStyle}>
              {tableRows}
            </ContextMenu.Trigger>
            <RowContextMenuPopup entries={menuCtx ? menuEntries(actions, menuCtx) : []} />
          </ContextMenu.Root>
        ) : (
          <Table.Body>{tableRows}</Table.Body>
        )}
      </Table>
      {dialogs}
    </Table.Container>
  );
}

function ResourceCell({ row, col }: { row: NativeObject; col: Column }) {
  const value = cellValue(row, col);
  const text = formatValue(value, col);
  const status = columnStatus(value, col);
  return (
    <Table.Cell align={isNumeric(col) ? 'right' : 'left'} numeric={isNumeric(col)} truncate>
      {status && text ? <StatusDot status={status} label={text} size="sm" /> : text}
    </Table.Cell>
  );
}
