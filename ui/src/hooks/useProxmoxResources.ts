import { useMemo } from 'react';
import {
  type DrawerContext,
  type HostRecord,
  parseAppError,
  useResolvedResourceTarget,
  useResources,
  valueToJSON,
} from '@omniviewdev/runtime';

import { PLUGIN_ID } from '../constants';
import type { ProxmoxObject } from '../utils/columns';

/** One listed object: its decoded body, the host's record of it, and its type. */
export type ProxmoxEntry<T = ProxmoxObject> = {
  resourceKey: string;
  /** The backend ID: `qemu/100`, `node/apollo`, `osd.19`. */
  id: string;
  data: T;
  record: HostRecord;
};

/** Decodes a host record's body for display. */
export function entryOf(record: HostRecord): ProxmoxEntry {
  const { type, id } = record.record.reference.address;
  return { resourceKey: type, id, data: valueToJSON(record.record.body) as ProxmoxObject, record };
}

/** The drawer and row-action context of one object. */
export function drawerContextOf(entry: ProxmoxEntry, name: string): DrawerContext<ProxmoxObject> {
  const { record } = entry;
  return {
    data: entry.data,
    resource: {
      reference: record.record.reference,
      targetId: record.targetId,
      route: record.route,
      representation: record.record.representation.id,
      key: record.key,
      name,
      pluginID: PLUGIN_ID,
      record,
    },
  };
}

/** The detail text of a runtime or host error. */
export function errorMessage(error: unknown): string {
  const appError = parseAppError(error);
  return appError.detail || appError.title;
}

type Options = {
  enabled?: boolean;
};

/**
 * A live collection of a resource type: the host resolves the target,
 * enumerates it and applies the plugin's poll updates.
 */
export function useProxmoxResources(connectionId: string, resourceKey: string, { enabled = true }: Options = {}) {
  const target = useResolvedResourceTarget({
    pluginID: PLUGIN_ID,
    route: { connection: connectionId },
    type: resourceKey,
    enabled: enabled && !!connectionId,
  });
  const result = useResources({
    pluginID: PLUGIN_ID,
    request: target.data && {
      targetId: target.data.targetId,
      request: { target: target.data.target, pageSize: 256 },
    },
    enabled: enabled && !!target.data,
    collection: true,
    live: true,
  });
  const records = result.resources.data?.records;
  const entries = useMemo(() => (records ?? []).map(entryOf), [records]);
  const error = target.error ?? result.resources.error ?? (result.sourceError ? result.source?.error ?? 'Source failed' : undefined);

  return {
    entries,
    isLoading: enabled && !result.resources.data && !error,
    error: error ? errorMessage(error) : undefined,
    refresh: () => void (result.acquired ? result.refreshAcquiredPage() : result.resources.refetch()),
  };
}
