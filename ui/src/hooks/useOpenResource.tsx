import { useCallback } from 'react';
import { CodeBlock } from '@omniviewdev/base-ui';
import { ResourceIcon, useRightDrawer, type DrawerComponent } from '@omniviewdev/runtime';
import { LuBraces, LuChartLine, LuServer, LuSquareChartGantt, LuWaypoints } from 'react-icons/lu';

import DrawerActionsBridge from '../actions/DrawerActionsBridge';
import { resourceActions } from '../actions/resourceActions';
import CephStatusOverview from '../components/CephStatusOverview';
import NodeGuests from '../components/NodeGuests';
import RelatedResources from '../components/RelatedResources';
import ResourceMetrics from '../components/ResourceMetrics';
import ResourceOverview from '../components/ResourceOverview';
import VMPendingConfig from '../components/VMPendingConfig';
import { GRAPH_RESOURCE_KEYS, METRIC_RESOURCE_KEYS, PLUGIN_ID, RESOURCE_KEYS } from '../constants';
import { type NativeObject, displayName, kindFor } from '../utils/columns';
import { type ProxmoxEntry, drawerContextOf } from './useProxmoxResources';

/**
 * Opens the detail drawer of one object: overview, related resources (for
 * kinds in the relationship graph), metrics, a node's guests and raw JSON,
 * with the kind's actions in the header.
 */
export function useOpenResource() {
  const { openDrawer } = useRightDrawer();

  return useCallback(
    function open(entry: ProxmoxEntry) {
      const { resourceKey, data } = entry;
      const kind = kindFor(resourceKey);
      const name = displayName(data, kind);
      const views: DrawerComponent<NativeObject>['views'] = [
        {
          title: 'Overview',
          icon: <LuSquareChartGantt />,
          component: (ctx) =>
            resourceKey === RESOURCE_KEYS.cephCluster ? (
              <CephStatusOverview data={ctx.data} />
            ) : (
              <>
                <ResourceOverview data={ctx.data} columns={kind.columns} />
                {resourceKey === RESOURCE_KEYS.virtualMachine && (
                  <VMPendingConfig record={ctx.resource?.record ?? entry.record} />
                )}
              </>
            ),
        },
      ];
      if (GRAPH_RESOURCE_KEYS.has(resourceKey)) {
        views.push({
          title: 'Related',
          icon: <LuWaypoints />,
          component: () => <RelatedResources entry={entry} onOpen={open} />,
        });
      }
      if (METRIC_RESOURCE_KEYS.has(resourceKey)) {
        views.push({
          title: 'Metrics',
          icon: <LuChartLine />,
          component: (ctx) => <ResourceMetrics record={ctx.resource?.record ?? entry.record} />,
        });
      }
      if (resourceKey === RESOURCE_KEYS.node) {
        views.push({
          title: 'Guests',
          icon: <LuServer />,
          component: () => (
            <NodeGuests connectionId={entry.record.route.connection} node={String(data.node ?? '')} onOpen={open} />
          ),
        });
      }
      views.push({
        title: 'JSON',
        icon: <LuBraces />,
        component: (ctx) => <CodeBlock language="json" code={jsonText(ctx.data)} copyable wrap />,
      });
      // Status-dependent actions follow the object while the drawer is open.
      const live = METRIC_RESOURCE_KEYS.has(resourceKey);
      const drawer: DrawerComponent<NativeObject> = {
        title: name,
        icon: <ResourceIcon pluginId={PLUGIN_ID} resourceKey={resourceKey} size={16} />,
        views: views.map((view) => ({
          ...view,
          component: (ctx) => (
            <DrawerActionsBridge ctx={ctx} drawer={drawer} live={live}>
              {view.component(ctx)}
            </DrawerActionsBridge>
          ),
        })),
        actions: resourceActions(resourceKey),
      };
      openDrawer(drawer, drawerContextOf(entry, name));
    },
    [openDrawer],
  );
}

/** Indented JSON; integers beyond 2^53 (bigint once decoded) print as strings. */
function jsonText(data: unknown): string {
  return JSON.stringify(data, (_key, value: unknown) => (typeof value === 'bigint' ? value.toString() : value), 2);
}
