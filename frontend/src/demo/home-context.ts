import { readable, writable } from 'svelte/store';

export const homeLayout = readable('mixed');
export const hiddenServers = writable<string[]>([]);

export function setServerHidden(id: string, hidden: boolean): void {
  hiddenServers.update((servers) => hidden ? [...servers, id] : servers.filter((server) => server !== id));
}

export const relayStore = Object.freeze({});
