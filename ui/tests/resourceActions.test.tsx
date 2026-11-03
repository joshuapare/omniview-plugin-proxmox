import { describe, expect, it, vi } from 'vitest';
import type { DrawerContext } from '@omniviewdev/runtime';

import { bindActionDeps, type ActionDeps } from '../src/actions/actionDeps';
import { resourceActions } from '../src/actions/resourceActions';
import { RESOURCE_KEYS } from '../src/constants';

vi.mock('@omniviewdev/runtime', () => ({ valueToJSON: () => ({}) }));

const cases = [
  { title: 'Start', action: 'start', status: 'stopped', label: 'Start' },
  { title: 'Shutdown', action: 'shutdown', status: 'running', label: 'Shut down' },
  { title: 'Reboot', action: 'reboot', status: 'running', label: 'Reboot' },
  { title: 'Stop', action: 'stop', status: 'running', label: 'Stop' },
  { title: 'Suspend', action: 'suspend', status: 'running', label: 'Suspend' },
  { title: 'Resume', action: 'resume', status: 'paused', label: 'Resume' },
];

describe.each([
  { kind: 'VM', key: RESOURCE_KEYS.virtualMachine, actions: cases },
  { kind: 'container', key: RESOURCE_KEYS.container, actions: cases.slice(0, 4) },
])('$kind lifecycle actions', ({ key, actions }) => {
  it.each(actions)('$title requires confirmation before execution', ({ title, action, status, label }) => {
    const route = { connection: 'homelab' };
    const reference = { address: { type: key, id: key === RESOURCE_KEYS.virtualMachine ? 'qemu/100' : 'lxc/100' } };
    const ctx = {
      data: { status },
      resource: { name: 'k8s-0', record: { route, record: { reference } } },
    } as unknown as DrawerContext;
    const show = vi.fn();
    const executeAction = vi.fn().mockResolvedValue({});
    const deps = { show, executeAction, notify: vi.fn() } as unknown as ActionDeps;
    bindActionDeps(ctx, deps);

    const item = resourceActions(key).find((entry) => entry.title === title)!;
    expect(item.enabled?.(ctx)).toBe(true);
    item.action(ctx);

    expect(show).toHaveBeenCalledOnce();
    expect(executeAction).not.toHaveBeenCalled();
    const dialog = show.mock.calls[0][0];
    expect(dialog.confirmLabel).toBe(label);
    expect(dialog.cancelLabel).toBe('Cancel');
    expect(dialog.body).toBeTruthy();
    expect(dialog.title.props.children[1].props.children).toBe('k8s-0');
    // Dismissing the dialog without invoking onConfirm must not execute anything.
    expect(executeAction).not.toHaveBeenCalled();

    dialog.onConfirm();
    expect(executeAction).toHaveBeenCalledOnce();
    expect(executeAction).toHaveBeenCalledWith({
      route,
      reference,
      action,
      input: { kind: 'map', value: {} },
    });
  });
});
