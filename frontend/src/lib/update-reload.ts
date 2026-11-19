import { relayStore } from './store';
import type { AppUpdateStatus, RelayConnectionView } from './types';
import {
  clearPendingRelayUpdate,
  pendingRelayUpdate,
  relayServesCurrentOrigin,
  reloadHeldMessage,
  reloadUpdatedSameOriginApp,
} from './updates';

export function settingsUpdateLabel(connections: Map<string, RelayConnectionView>, appState: AppUpdateStatus['state']): string {
  const appUpdateAvailable = appState === 'reload-ready' || appState === 'deployment-required';
  let needsAttention = false;
  for (const { status, capabilities, update } of connections.values()) {
    if (update.state === 'available') {
      return `Settings, ${appUpdateAvailable ? 'phone app and relay updates' : 'relay update'} available`;
    }
    needsAttention ||= update.state === 'blocked' || (status === 'connected' && !capabilities.includes('self_update'));
  }
  const label = appUpdateAvailable ? 'Settings, phone app update available' : 'Settings';
  return needsAttention ? `${label}${appUpdateAvailable ? ' and' : ','} relay update needs attention` : label;
}

export function createRelayUpdateObserver() {
  const requested = new Set<string>();
  const awaitedDeployments = new Set<string>();
  return (connections: Map<string, RelayConnectionView>) => {
    const connected = [...connections].filter(([, connection]) => connection.status === 'connected');
    for (const [relayId, { capabilities, releaseVersion, revision }] of connected) {
      if (!capabilities.includes('self_update')) continue;
      const identity = `${relayId}:${releaseVersion}:${revision}`;
      if (requested.has(identity)) continue;
      requested.add(identity);
      void relayStore.checkRelayUpdate(relayId).catch(() => { requested.delete(identity); });
    }
    for (const [relayId, connection] of connected) reloadAfterRelayUpdate(relayId, connection);
    for (const [, { appDeploy: deployment }] of connected) {
      if (deployment.state !== 'succeeded' || deployment.origin !== location.origin || !deployment.target_version) continue;
      // A relay keeps reporting its last deployment. Only wait for each build once.
      const identity = `${deployment.target_version}:${deployment.target_revision}`;
      if (awaitedDeployments.has(identity)) continue;
      awaitedDeployments.add(identity);
      reloadIntoUpdate(deployment.target_version);
    }
  };
}

function reloadIntoUpdate(version: string) {
  void reloadUpdatedSameOriginApp(version).then((reloaded) => {
    const held = reloadHeldMessage();
    if (!reloaded && held) relayStore.showToast(held, true);
  });
}

function reloadAfterRelayUpdate(relayId: string, connection: RelayConnectionView): void {
  const pending = pendingRelayUpdate(relayId);
  if (!pending || connection.releaseVersion !== pending.version) return;
  const revision = connection.revision.replace(/-dirty$/, '');
  if (!revision || !pending.revision.startsWith(revision)) return;
  clearPendingRelayUpdate(relayId);
  relayStore.showToast(`${connection.relay.label} updated to v${pending.version}.`);
  if (relayServesCurrentOrigin(connection.relay.url)) reloadIntoUpdate(pending.version);
}
