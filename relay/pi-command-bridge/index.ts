import { readFile } from 'node:fs/promises';

const read = name => readFile(new URL(`./${name}`, import.meta.url), 'utf8');
const moduleUrl = source => `data:text/javascript;base64,${Buffer.from(source).toString('base64')}`;

// Pi's /reload runs this file again but reuses modules it already imported, so the bridge is
// imported from its current source as data: URLs, which cannot use relative imports.
export default async function commandBridge(pi) {
  const activity = moduleUrl(await read('activity.mjs'));
  const bridge = (await read('bridge.mjs')).replaceAll("from './activity.mjs'", `from '${activity}'`);
  const { default: start } = await import(moduleUrl(bridge));
  return start(pi);
}
