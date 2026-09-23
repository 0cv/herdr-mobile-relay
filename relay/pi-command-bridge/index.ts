import { readFile } from 'node:fs/promises';

export default async function commandBridge(pi) {
  // Pi reloads this file but caches imports of the same bridge file path.
  const source = await readFile(new URL('./bridge.mjs', import.meta.url));
  const { default: loadBridge } = await import(`data:text/javascript;base64,${source.toString('base64')}`);
  loadBridge(pi);
}
