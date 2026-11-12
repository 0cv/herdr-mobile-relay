import { relayStore } from './store';
import type { DirectoryListing, RelayConnectionView } from './types';

export function createDirectoryLoader(
  target: () => { relayId: string; connection: Pick<RelayConnectionView, 'capabilities'> | undefined },
  loaded: (listing: DirectoryListing, relayId: string) => void,
  failed: (error: unknown) => void,
) {
  let generation = 0;
  return {
    async load(path: string) {
      const { relayId, connection } = target();
      const current = ++generation;
      if (!relayId || !connection?.capabilities.includes('directory_browser')) return;
      const isCurrent = () => current === generation && target().relayId === relayId;
      try {
        const listing = await relayStore.listDirectories(relayId, path);
        if (isCurrent()) loaded(listing, relayId);
      } catch (error) {
        if (isCurrent()) failed(error);
      }
    },
    cancel() {
      generation += 1;
    },
  };
}
