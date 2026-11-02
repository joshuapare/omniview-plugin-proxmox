import { useCallback, useEffect, useState } from 'react';
import { Banner, Button, Spinner, Text } from '@omniviewdev/base-ui';
import { valueToJSON, type HostRecord } from '@omniviewdev/runtime';
import { ResourceClient } from '@omniviewdev/runtime/api';

import { PLUGIN_ID } from '../constants';
import { errorMessage } from '../hooks/useProxmoxResources';

type PendingEntry = { key: string; value?: string; pending?: string; delete?: number | bigint };

function parseEntries(value: unknown): PendingEntry[] {
  if (!value || typeof value !== 'object' || !('entries' in value) || !Array.isArray(value.entries)) {
    throw new Error('Invalid pending configuration response');
  }
  return value.entries.map((entry: unknown) => {
    if (!entry || typeof entry !== 'object' || !('key' in entry) || typeof entry.key !== 'string') {
      throw new Error('Invalid pending configuration entry');
    }
    const result: PendingEntry = { key: entry.key };
    if ('value' in entry && (typeof entry.value === 'string' || typeof entry.value === 'number' || typeof entry.value === 'bigint')) {
      result.value = String(entry.value);
    }
    if ('pending' in entry && (typeof entry.pending === 'string' || typeof entry.pending === 'number' || typeof entry.pending === 'bigint')) {
      result.pending = String(entry.pending);
    }
    if ('delete' in entry && (typeof entry.delete === 'number' || typeof entry.delete === 'bigint')) result.delete = entry.delete;
    return result;
  });
}

export const VM_CONFIG_CHANGED = 'proxmox-vm-config-changed';

/** Reads the VM's pending endpoint, which includes both active and staged values. */
export default function VMPendingConfig({ record }: { record: HostRecord }) {
  const [entries, setEntries] = useState<PendingEntry[]>();
  const [error, setError] = useState<string>();
  const [revision, setRevision] = useState(0);
  const refresh = useCallback(() => setRevision((n) => n + 1), []);
  const { route, record: resource } = record;
  const id = resource.reference.address.id;

  useEffect(() => {
    const onChange = (event: Event) => {
      const detail = (event as CustomEvent<{ connection: string; id: string }>).detail;
      if (detail?.connection === route.connection && detail.id === id) refresh();
    };
    window.addEventListener(VM_CONFIG_CHANGED, onChange);
    return () => window.removeEventListener(VM_CONFIG_CHANGED, onChange);
  }, [route.connection, id, refresh]);

  useEffect(() => {
    let cancelled = false;
    setError(undefined);
    setEntries(undefined);
    ResourceClient.Action(PLUGIN_ID, {
      route,
      reference: resource.reference,
      action: 'pending',
      input: { kind: 'map', value: {} },
    }).then((result) => {
      if (!cancelled) setEntries(parseEntries(valueToJSON(result)));
    }).catch((err: unknown) => {
      if (!cancelled) setError(errorMessage(err));
    });
    return () => { cancelled = true; };
  }, [route, resource.reference, revision]);

  const changes = entries?.filter((entry) => entry.pending !== undefined || Boolean(entry.delete))
    .sort((a, b) => a.key.localeCompare(b.key));

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12, padding: 12 }}>
      <div><Button size="sm" variant="outline" onClick={refresh}>Refresh</Button></div>
      {error ? <Banner color="danger" size="sm"><Banner.Content>Could not read pending configuration: {error}</Banner.Content></Banner>
        : !changes ? <Spinner size="sm" />
        : changes.length === 0 ? <Text size="sm">No pending configuration changes.</Text>
        : <>
          <Banner color="warning" size="sm"><Banner.Content>Pending changes require a VM restart. Shut down and start the VM to apply them.</Banner.Content></Banner>
          <table style={{ width: '100%', textAlign: 'left', borderSpacing: '8px' }}>
            <thead><tr><th>Setting</th><th>Current value</th><th>Pending value</th></tr></thead>
            <tbody>{changes.map((entry) => <tr key={entry.key}>
              <td>{entry.key}</td><td style={{ overflowWrap: 'anywhere' }}>{entry.value ?? 'Not set'}</td>
              <td style={{ overflowWrap: 'anywhere' }}>{entry.pending !== undefined ? entry.pending : 'Not set'}{Boolean(entry.delete) && (Number(entry.delete) === 2 ? ' (force removal)' : ' (remove)')}</td>
            </tr>)}</tbody>
          </table>
        </>}
    </div>
  );
}
