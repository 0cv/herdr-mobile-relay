import { afterEach, describe, expect, test } from 'bun:test';
import { mkdir, mkdtemp, readFile, readdir, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { allocateRun, browserSetupURL, loggedCommand, parsePaneList, piLaunch, piSettings, preserveReceipt, sessionSeed } from './pi-scroll-driver.mjs';

const roots = [];
async function directory() {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'p1-offline-driver-')));
  roots.push(root);
  return root;
}
async function predecessors(root, count) {
  for (let index = 1; index <= count; index++) {
    await mkdir(join(root, `run-${index}`));
    await writeFile(join(root, `run-${index}`, 'receipt.json'), JSON.stringify({ result: 'failed', index }));
  }
}
afterEach(async () => { for (const root of roots.splice(0)) await rm(root, { recursive: true }); });

describe('offline driver orchestration only; no Herdr/Pi or native qualification', () => {
  test('charges existing six sequentially through nine, links predecessors and refuses ten', async () => {
    const root = await directory();
    await predecessors(root, 6);
    const before = await readFile(join(root, 'run-6/receipt.json'), 'utf8');
    const allocation = await allocateRun(root);
    expect(allocation.index).toBe(7);
    expect(allocation.predecessor.path).toBe(join(root, 'run-6/receipt.json'));
    expect(allocation.predecessor.sha256).toHaveLength(64);
    expect(JSON.parse(await readFile(join(allocation.directory, 'allocation.json'), 'utf8')).index).toBe(7);
    await expect(allocateRun(root)).rejects.toThrow();
    await allocation.release();
    await preserveReceipt(allocation.directory, undefined, { result: 'failed', error: 'setup failed' });
    const next = await allocateRun(root);
    expect(next.index).toBe(8);
    await next.release();
    await preserveReceipt(next.directory, undefined, { result: 'failed', error: 'second setup failed' });
    const last = await allocateRun(root);
    expect(last.index).toBe(9);
    expect(last.predecessor.path).toBe(join(root, 'run-8/receipt.json'));
    expect(last.predecessor.sha256).toHaveLength(64);
    await last.release();
    await expect(allocateRun(root)).rejects.toThrow('exhausted');
    expect(await readFile(join(root, 'run-6/receipt.json'), 'utf8')).toBe(before);
  });

  test('concurrent allocation admits one and failed contenders do not remove its lock', async () => {
    const root = await directory();
    const results = await Promise.allSettled([allocateRun(root), allocateRun(root)]);
    expect(results.filter(result => result.status === 'fulfilled')).toHaveLength(1);
    await expect(allocateRun(root)).rejects.toThrow();
    const winner = results.find(result => result.status === 'fulfilled').value;
    await winner.release();
    expect((await readdir(root)).filter(name => name.startsWith('run-'))).toEqual(['run-1']);
  });

  test('gaps, duplicate numeric aliases, missing predecessor receipts and excess allocations fail closed', async () => {
    for (const names of [['run-2'], ['run-1', 'run-01'], ['run-1', 'run-1-copy'], ['run-1']]) {
      const root = await directory();
      for (const name of names) await mkdir(join(root, name));
      await expect(allocateRun(root)).rejects.toThrow();
      expect(await readdir(root)).not.toContain('.active');
    }
    for (const count of [9, 10]) {
      const root = await directory();
      await predecessors(root, count);
      await expect(allocateRun(root)).rejects.toThrow('exhausted');
    }
  });

  test('parses actual Herdr response envelope and rejects old top-level/ambiguous shapes', () => {
    const pane = { pane_id: 'w1:p1', terminal_id: 'term_123' };
    expect(parsePaneList(JSON.stringify({ id: 'cli:pane:list', result: { type: 'pane_list', panes: [pane] } }))).toEqual(pane);
    for (const value of [{ panes: [pane] }, { error: { code: 'unavailable' } }, { result: { type: 'pane_list', panes: [] } }, { result: { type: 'pane_list', panes: [pane, pane] } }, { result: { type: 'other', panes: [pane] } }, { result: { type: 'pane_list', panes: [{ pane_id: 'a' }] } }]) expect(() => parsePaneList(JSON.stringify(value))).toThrow();
  });

  test('seed uses canonical Pi workspace directory and names are unique across two modes', () => {
    const root = '/private/tmp/herdr-scroll-p1-fixture';
    const seed = sessionSeed(root, '2026-10-15T12:34:56.789Z', 'session-id');
    expect(seed).toBe(`${root}/pi/sessions/--private-tmp-herdr-scroll-p1-fixture-work--/2026-10-15T12-34-56-789Z_session-id.jsonl`);
    const full = piLaunch('/fixture/herdr', 'fullscreen', 'pane', seed, root);
    const regular = piLaunch('/fixture/herdr', 'regular', 'pane', seed, root);
    expect(full[3]).not.toBe(regular[3]);
    for (const args of [full, regular]) {
      expect(args[args.indexOf('--session') + 1]).toBe(seed);
      expect(args).toContain('--offline');
      expect(args.filter(value => value === '--extension')).toHaveLength(3);
      expect(args).toContain(`${root}/pi/herdr-agent-state.ts`);
    }
    expect(() => piLaunch('herdr', 'unknown', 'pane', seed, root)).toThrow();
  });

  test('paired setup uses a bare socket origin and suppresses duplicate exit transcripts in the private seed settings', () => {
    const url = new URL(browserSetupURL('http://127.0.0.1:12345', 'x'.repeat(32)));
    const params = new URLSearchParams(url.hash.slice(1));
    expect(params.get('relay')).toBe('ws://127.0.0.1:12345');
    expect(new URL(params.get('relay')).pathname).toBe('/');
    expect(params.get('setup')).toBe('x'.repeat(32));
    expect(params.get('label')).toBe('P1 native');
    expect(() => browserSetupURL('http://127.0.0.1:12345/ws', 'secret')).toThrow();
    expect(piSettings('/private/fixture', '1.2.0')).toEqual({ tuiMode: 'regular', fullscreenExitOutput: 'resume-hint', externalEditor: '/private/fixture/editor.sh', enabledModels: [], lastChangelogVersion: '1.2.0' });
  });

  test('real Bun stdio is separate and full command/env/status/output are durable', async () => {
    const root = await directory();
    const args = [process.execPath, '-e', 'console.log("stdout"); console.error("stderr")'];
    const result = await loggedCommand(args, { directory: root, label: 'stdio', env: { PATH: '/usr/bin:/bin' }, cwd: root, identity: async () => 'offline-child', signalOwned: async () => { throw new Error('unexpected signal'); } });
    expect(result.trim()).toBe('stdout');
    expect((await readFile(join(root, 'stdio.log.stderr'), 'utf8')).trim()).toBe('stderr');
    const record = JSON.parse(await readFile(join(root, 'stdio.log.command.json'), 'utf8'));
    expect(record).toMatchObject({ args, cwd: root, env: { PATH: '/usr/bin:/bin' }, status: 'passed', exitCode: 0 });
    expect(record.ended).toBeTruthy();
  });

  test('mock spawn failure and timeout cleanup failure retain failed command evidence', async () => {
    const root = await directory();
    for (const [label, spawn, signalOwned] of [
      ['spawn', () => { throw new Error('stdio setup failure'); }, async () => {}],
      ['cleanup', () => ({ pid: 123, exitCode: null }), async () => { throw new Error('identity changed; no signal'); }],
    ]) {
      await expect(loggedCommand(['offline-mock'], { directory: root, label, env: {}, cwd: root, timeout: 0, spawn, identity: async () => 'birth', signalOwned })).rejects.toThrow();
      const record = JSON.parse(await readFile(join(root, `${label}.log.command.json`), 'utf8'));
      expect(record.status).toBe('failed');
      expect(record.error).toContain(label === 'spawn' ? 'stdio setup failure' : 'identity changed');
      expect(record.ended).toBeTruthy();
    }
  });

  test('mock timeout signals owned child once and does not retry the command', async () => {
    const root = await directory();
    const child = { pid: 123, exitCode: null, exited: Promise.resolve(143) };
    const signals = [];
    let calls = 0;
    await expect(loggedCommand(['offline-mock'], { directory: root, label: 'timeout', env: {}, cwd: root, timeout: 0, spawn: () => { calls++; return child; }, identity: async () => 'birth', signalOwned: async (_child, birth, signal) => { expect(birth).toBe('birth'); signals.push(signal); child.exitCode = 143; }, sleep: async () => {} })).rejects.toThrow('no retry');
    expect(calls).toBe(1);
    expect(signals).toEqual(['SIGTERM']);
  });

  test('nonzero process output and exit status remain available without a retry', async () => {
    const root = await directory();
    await expect(loggedCommand([process.execPath, '-e', 'console.error("diagnosis"); process.exit(7)'], { directory: root, label: 'failure', env: {}, cwd: root, identity: async () => 'offline-child', signalOwned: async () => {} })).rejects.toThrow('exit 7');
    expect((await readFile(join(root, 'failure.log.stderr'), 'utf8')).trim()).toBe('diagnosis');
    expect(JSON.parse(await readFile(join(root, 'failure.log.command.json'), 'utf8'))).toMatchObject({ exitCode: 7, status: 'failed' });
  });

  test('capture-copy errors are included in the durable failed receipt', async () => {
    const output = await directory(), root = await directory();
    const allocation = await allocateRun(output);
    await mkdir(join(root, 'logs'));
    await mkdir(join(allocation.directory, 'logs'));
    await writeFile(join(root, 'logs', 'raw'), 'new');
    await writeFile(join(allocation.directory, 'logs', 'raw'), 'original');
    await expect(preserveReceipt(allocation.directory, root, { result: 'passed' })).rejects.toThrow('preservation failed');
    const receipt = JSON.parse(await readFile(join(allocation.directory, 'receipt.json'), 'utf8'));
    expect(receipt.result).toBe('failed');
    expect(receipt.preservationErrors.length).toBeGreaterThan(0);
    expect(await readFile(join(allocation.directory, 'logs', 'raw'), 'utf8')).toBe('original');
    await allocation.release();
  });

  test('early setup failure and raw captures survive; duplicate receipt cannot overwrite', async () => {
    const output = await directory(), root = await directory();
    const allocation = await allocateRun(output);
    await mkdir(join(root, 'logs'));
    await writeFile(join(root, 'logs', 'raw.bin'), Buffer.from([0, 27, 255]));
    const receipt = { result: 'failed', error: 'early setup failure', processCleanup: [{ error: 'cleanup failed' }] };
    await preserveReceipt(allocation.directory, root, receipt);
    expect(JSON.parse(await readFile(join(allocation.directory, 'receipt.json'), 'utf8'))).toEqual(receipt);
    expect([...await readFile(join(allocation.directory, 'logs/raw.bin'))]).toEqual([0, 27, 255]);
    await expect(preserveReceipt(allocation.directory, undefined, { result: 'passed' })).rejects.toThrow();
    expect(JSON.parse(await readFile(join(allocation.directory, 'receipt.json'), 'utf8'))).toEqual(receipt);
    await allocation.release();
  });
});
