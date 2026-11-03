import { type DrawerComponentAction, type DrawerContext, valueToJSON } from '@omniviewdev/runtime';
import {
  LuCirclePause,
  LuCirclePlay,
  LuLogs,
  LuPlay,
  LuPower,
  LuRotateCw,
  LuSettings2,
  LuSquare,
  LuTerminal,
  LuTrash2,
} from 'react-icons/lu';

import { PLUGIN_ID, RESOURCE_KEYS } from '../constants';
import type { NativeObject } from '../utils/columns';
import { type ActionTarget, actionDeps } from './actionDeps';

type Ctx = DrawerContext<NativeObject>;
type Action = DrawerComponentAction<NativeObject>;

function targetOf(ctx: Ctx): ActionTarget {
  const record = ctx.resource?.record;
  if (!record) throw new Error('The action has no resource record');
  const { type, id } = record.record.reference.address;
  return {
    connectionID: record.route.connection,
    resourceKey: type,
    id,
    name: ctx.resource?.name || id,
    data: ctx.data ?? {},
    record,
  };
}

const status = (ctx: Ctx) => String(ctx.data?.status ?? '');
const isTemplate = (ctx: Ctx) => ctx.data?.template === 1 || ctx.data?.template === true;
const isRunning = (ctx: Ctx) => status(ctx) === 'running' && !isTemplate(ctx);
const isPaused = (ctx: Ctx) => status(ctx) === 'paused' || status(ctx) === 'suspended';

/** Runs a lifecycle action and shows its result; the runtime reports any error. */
function runLifecycle(ctx: Ctx, actionID: string, progress: string) {
  const deps = actionDeps(ctx);
  const target = targetOf(ctx);
  deps.notify(`${progress} ${target.name}…`);
  deps
    .executeAction({
      route: target.record.route,
      reference: target.record.record.reference,
      action: actionID,
      input: { kind: 'map', value: {} },
    })
    .then((result) => {
      const { message } = valueToJSON(result) as { message?: string };
      if (message) deps.notify(message);
    })
    .catch(() => undefined);
}

/** A lifecycle action that asks first, as the Proxmox web UI does. */
function confirmLifecycle(ctx: Ctx, actionID: string, progress: string, question: string, consequence: string) {
  const target = targetOf(ctx);
  actionDeps(ctx).show({
    title: (
      <span>
        {question} <strong>{target.name}</strong>?
      </span>
    ),
    body: consequence,
    confirmLabel: question,
    cancelLabel: 'Cancel',
    onConfirm: () => runLifecycle(ctx, actionID, progress),
  });
}

/** Opens a terminal tab; Proxmox's termproxy gives a login shell, so no command is sent. */
function openShell(ctx: Ctx, params: Record<string, string>) {
  const target = targetOf(ctx);
  void actionDeps(ctx).createExecSession({
    connectionID: target.connectionID,
    label: target.name,
    icon: 'LuTerminal',
    opts: {
      resource_plugin: PLUGIN_ID,
      resource_key: target.resourceKey,
      resource_data: target.record.record.body,
      reference: target.record.record.reference,
      route: target.record.route,
      params,
      tty: true,
    },
  });
}

const viewLogs: Action = {
  title: 'View logs',
  icon: <LuLogs />,
  action: (ctx) => {
    const target = targetOf(ctx);
    actionDeps(ctx)
      .createLogSession({
        connectionID: target.connectionID,
        resourceKey: target.resourceKey,
        resourceID: target.id,
        resourceData: target.record.record.body,
        reference: target.record.record.reference,
        route: target.record.route,
        label: `Logs ${target.name}`,
        icon: 'LuLogs',
      })
      .catch(() => undefined);
  },
};

function guestActions(qemu: boolean): Action[] {
  const noun = qemu ? 'virtual machine' : 'container';
  const actions: Action[] = [
    {
      title: 'Open shell',
      icon: <LuTerminal />,
      enabled: isRunning,
      // VMs attach to a serial console; the guest needs a serial0 port.
      action: (ctx) => openShell(ctx, qemu ? { serial: 'serial0' } : {}),
    },
    {
      title: 'Edit config',
      icon: <LuSettings2 />,
      action: (ctx) => actionDeps(ctx).openEditConfig(targetOf(ctx)),
    },
    {
      title: 'Start',
      icon: <LuPlay />,
      enabled: (ctx) => status(ctx) === 'stopped' && !isTemplate(ctx),
      action: (ctx) => confirmLifecycle(ctx, 'start', 'Starting', 'Start', `Starts the ${noun}.`),
    },
    {
      title: 'Shutdown',
      icon: <LuPower />,
      enabled: isRunning,
      action: (ctx) =>
        confirmLifecycle(ctx, 'shutdown', 'Shutting down', 'Shut down', `Asks the guest OS to shut the ${noun} down.`),
    },
    {
      title: 'Reboot',
      icon: <LuRotateCw />,
      enabled: isRunning,
      action: (ctx) => confirmLifecycle(ctx, 'reboot', 'Rebooting', 'Reboot', `Asks the guest OS to restart the ${noun}.`),
    },
    {
      title: 'Stop',
      icon: <LuSquare />,
      enabled: (ctx) => isRunning(ctx) || isPaused(ctx),
      action: (ctx) =>
        confirmLifecycle(
          ctx,
          'stop',
          'Stopping',
          'Stop',
          `Stops the ${noun} immediately, like pulling the power plug. Unsaved data in the guest is lost.`,
        ),
    },
  ];
  if (qemu) {
    actions.push(
      {
        title: 'Suspend',
        icon: <LuCirclePause />,
        enabled: isRunning,
        action: (ctx) => confirmLifecycle(ctx, 'suspend', 'Suspending', 'Suspend', 'Pauses the virtual machine in memory.'),
      },
      {
        title: 'Resume',
        icon: <LuCirclePlay />,
        enabled: isPaused,
        action: (ctx) => confirmLifecycle(ctx, 'resume', 'Resuming', 'Resume', 'Resumes the paused virtual machine.'),
      },
    );
  }
  actions.push({
    title: 'Delete',
    icon: <LuTrash2 />,
    // Proxmox refuses to destroy a running guest.
    enabled: (ctx) => status(ctx) === 'stopped',
    action: (ctx) => actionDeps(ctx).openDelete(targetOf(ctx)),
  });
  return actions;
}

const ACTIONS: Record<string, Action[]> = {
  [RESOURCE_KEYS.virtualMachine]: guestActions(true),
  [RESOURCE_KEYS.container]: guestActions(false),
  [RESOURCE_KEYS.node]: [
    {
      title: 'Open shell',
      icon: <LuTerminal />,
      enabled: (ctx) => status(ctx) === 'online',
      action: (ctx) => openShell(ctx, {}),
    },
    viewLogs,
  ],
  [RESOURCE_KEYS.cephOSD]: [viewLogs],
  [RESOURCE_KEYS.cephMonitor]: [viewLogs],
  [RESOURCE_KEYS.cephManager]: [viewLogs],
  [RESOURCE_KEYS.cephMetadataServer]: [viewLogs],
};

/** Drawer header and row menu actions of a kind; handlers come from `actionDeps`. */
export function resourceActions(resourceKey: string): Action[] {
  return ACTIONS[resourceKey] ?? [];
}
