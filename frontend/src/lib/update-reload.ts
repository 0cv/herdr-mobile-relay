import { relayStore } from './store';
import type { RelayConnectionView } from './types';
import {
  clearPendingRelayUpdate,
  pendingRelayUpdate,
  relayServesCurrentOrigin,
  reloadHeldMessage,
  reloadUpdatedSameOriginApp,
} from './updates';

function reloadIntoUpdate(version: string) {
  void reloadUpdatedSameOriginApp(version).then((reloaded) => {
    const held = reloadHeldMessage();
    if (!reloaded && held) relayStore.showToast(held, true);
  });
}

export function reloadAfterRelayUpdate(connections: Map<string, RelayConnectionView>): void {
  for (const [relayId, connection] of connections) {
    if (connection.status !== 'connected') continue;
    const pending = pendingRelayUpdate(relayId);
    if (!pending || connection.releaseVersion !== pending.version) continue;
    const revision = connection.revision.replace(/-dirty$/, '');
    if (!revision || !pending.revision.startsWith(revision)) continue;
    clearPendingRelayUpdate(relayId);
    relayStore.showToast(`${connection.relay.label} updated to v${pending.version}.`);
    if (relayServesCurrentOrigin(connection.relay.url)) {
      reloadIntoUpdate(pending.version);
    }
  }
}

export function createDeploymentReloader() {
  const awaitedDeployments = new Set<string>();

  return function reloadAfterDeployment(connections: Map<string, RelayConnectionView>): void {
    for (const connection of connections.values()) {
      const deployment = connection.appDeploy;
      if (
        connection.status !== 'connected'
        || deployment.state !== 'succeeded'
        || deployment.origin !== location.origin
        || !deployment.target_version
      ) continue;
      // A relay announces its last successful deployment forever; without this
      // guard every store emission would restart the two-minute wait for a
      // stale target the origin will never serve again.
      const identity = `${deployment.target_version}:${deployment.target_revision}`;
      if (awaitedDeployments.has(identity)) continue;
      awaitedDeployments.add(identity);
      reloadIntoUpdate(deployment.target_version);
    }
  };
}
