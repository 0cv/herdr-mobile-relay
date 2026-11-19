export * from '../lib/updates';

export const initializeAppUpdates = () => () => {};
export const importOrReload = <T>(load: () => Promise<T>): Promise<T> => load();
