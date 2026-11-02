import { useMemo } from 'react';

import { RESOURCE_KEYS } from '../constants';
import type { ProxmoxObject } from '../utils/columns';
import { type ProxmoxEntry, useProxmoxResources } from './useProxmoxResources';

export type ClusterObject = ProxmoxEntry;

/**
 * Every node, storage entry and guest of a connection: per kind, and by
 * Proxmox ID. Graph nodes carry only IDs; `objects` supplies names, status
 * and host records for drawing and opening them.
 */
export function useClusterObjects(connectionId: string) {
  const nodes = useProxmoxResources(connectionId, RESOURCE_KEYS.node);
  const storage = useProxmoxResources(connectionId, RESOURCE_KEYS.storage);
  const vms = useProxmoxResources(connectionId, RESOURCE_KEYS.virtualMachine);
  const containers = useProxmoxResources(connectionId, RESOURCE_KEYS.container);

  const objects = useMemo(() => {
    const byId = new Map<string, ClusterObject>();
    for (const entries of [nodes.entries, storage.entries, vms.entries, containers.entries]) {
      for (const entry of entries) byId.set(entry.id, entry);
    }
    return byId;
  }, [nodes.entries, storage.entries, vms.entries, containers.entries]);

  const data = (entries: ProxmoxEntry[]): ProxmoxObject[] => entries.map((e) => e.data);
  return {
    objects,
    nodes: useMemo(() => data(nodes.entries), [nodes.entries]),
    storage: useMemo(() => data(storage.entries), [storage.entries]),
    virtualMachines: useMemo(() => data(vms.entries), [vms.entries]),
    containers: useMemo(() => data(containers.entries), [containers.entries]),
    isLoading: nodes.isLoading || storage.isLoading || vms.isLoading || containers.isLoading,
    error: nodes.error ?? storage.error ?? vms.error ?? containers.error,
  };
}
