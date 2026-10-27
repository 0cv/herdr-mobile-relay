import { test, expect } from 'bun:test';
import { copyFile, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const source = new URL('../relay/pi-command-bridge/', import.meta.url);
const loads = () => globalThis.herdrReloadTestLoads ??= [];
const marker = label => `\nglobalThis.herdrReloadTestLoads.push(${JSON.stringify(label)});\n`;

test('reloading the extension runs bridge.mjs and activity.mjs again only after they change', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'herdr-pi-reload-'));
  try {
    for (const name of ['index.ts', 'bridge.mjs', 'activity.mjs']) await copyFile(new URL(name, source), join(dir, name));
    const original = {
      bridge: await readFile(join(dir, 'bridge.mjs'), 'utf8'),
      activity: await readFile(join(dir, 'activity.mjs'), 'utf8'),
    };
    const version = async (name, label) => writeFile(join(dir, `${name}.mjs`), original[name] + marker(`${name} ${label}`));
    await version('bridge', 'one');
    await version('activity', 'one');
    const registered = [];
    const pi = { on: type => registered.push(type), getCommands: () => [] };
    loads().splice(0);
    const { default: commandBridge } = await import(join(dir, 'index.ts'));
    const reload = async () => {
      await commandBridge(pi);
      return loads().splice(0);
    };

    expect(await reload()).toEqual(['activity one', 'bridge one']);
    expect(registered).toContain('session_start');
    expect(await reload()).toEqual([]);
    await version('activity', 'two');
    expect(await reload()).toEqual(['activity two', 'bridge one']);
    await version('bridge', 'two');
    expect(await reload()).toEqual(['bridge two']);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
