import { useMemo, useState } from 'react';
import {
  useConfirmationModal,
  useExec,
  useExecuteAction,
  useLogs,
  useResourceMutations,
  useNotifications,
} from '@omniviewdev/runtime';

import { PLUGIN_ID } from '../constants';
import type { ActionDeps, ActionTarget } from './actionDeps';
import DeleteGuestDialog from './DeleteGuestDialog';
import EditConfigDialog from './EditConfigDialog';

type Options = {
  /** Runs after a delete from this mount point succeeds, e.g. to close the drawer. */
  onDeleted?: () => void;
};

/**
 * The runtime hooks actions run with, plus the dialogs they open. Render
 * `dialogs` wherever the hook is used.
 */
export function useActionExecution({ onDeleted }: Options = {}) {
  const { show } = useConfirmationModal();
  const { executeAction } = useExecuteAction({ pluginID: PLUGIN_ID });
  const { createSession } = useExec({ pluginID: PLUGIN_ID });
  const { createLogSession } = useLogs({ pluginID: PLUGIN_ID });
  const { mutate } = useResourceMutations({ pluginID: PLUGIN_ID });
  const { notify } = useNotifications();
  const [editTarget, setEditTarget] = useState<ActionTarget>();
  const [deleteTarget, setDeleteTarget] = useState<ActionTarget>();

  const deps = useMemo<ActionDeps>(
    () => ({
      show,
      executeAction,
      createExecSession: createSession,
      createLogSession,
      notify: (message) => { void notify({ title: message, severity: 'info' }); },
      openEditConfig: setEditTarget,
      openDelete: setDeleteTarget,
    }),
    [show, executeAction, createSession, createLogSession, notify],
  );

  const dialogs = (
    <>
      {editTarget && (
        <EditConfigDialog target={editTarget} mutate={mutate} onClose={() => setEditTarget(undefined)} />
      )}
      {deleteTarget && (
        <DeleteGuestDialog
          target={deleteTarget}
          mutate={mutate}
          onClose={() => setDeleteTarget(undefined)}
          onDeleted={() => onDeleted?.()}
        />
      )}
    </>
  );

  return { deps, dialogs };
}
