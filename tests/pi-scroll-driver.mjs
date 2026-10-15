import { copyFile, cp, mkdir, readFile, readdir, realpath, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { createHash } from 'node:crypto';

export async function allocateRun(output) {
  await mkdir(output, { recursive: true, mode: 0o700 });
  const lock = join(output, '.active');
  await mkdir(lock, { mode: 0o700 });
  try {
    const names = (await readdir(output)).filter(name => name.startsWith('run-'));
    const numbers = names.map(name => Number(/^run-([1-9]\d*)$/.exec(name)?.[1])).sort((a, b) => a - b);
    if (numbers.some((number, index) => !Number.isSafeInteger(number) || number !== index + 1)) throw new Error('qualification allocations are not contiguous and unique');
    if (numbers.length >= 9) throw new Error('nine real qualification runs exhausted');
    const index = numbers.length + 1;
    const directory = join(output, `run-${index}`);
    let predecessor = null;
    if (index > 1) {
      const previous = join(output, `run-${index - 1}`, 'receipt.json');
      predecessor = { path: previous, sha256: createHash('sha256').update(await readFile(previous)).digest('hex') };
    }
    await mkdir(directory, { mode: 0o700 });
    await writeFile(join(directory, 'allocation.json'), JSON.stringify({ index, started: new Date().toISOString(), predecessor }, null, 2) + '\n', { mode: 0o600, flag: 'wx' });
    return { index, directory, predecessor, release: () => rm(lock, { recursive: true }) };
  } catch (error) {
    await rm(lock, { recursive: true });
    throw error;
  }
}

export function parsePaneList(text) {
  const envelope = JSON.parse(text);
  const panes = envelope.result?.type === 'pane_list' && envelope.result.panes;
  if (!Array.isArray(panes) || panes.length !== 1 || !panes[0].pane_id || !panes[0].terminal_id) throw new Error('isolated pane-list must contain exactly one pane/terminal in its result envelope');
  return panes[0];
}

export function sessionSeed(root, timestamp, session) {
  return join(root, 'pi', 'sessions', `--${join(root, 'work').slice(1).replaceAll('/', '-')}--`, `${timestamp.replaceAll(':', '-').replaceAll('.', '-')}_${session}.jsonl`);
}

export function browserSetupURL(origin, token) {
  const url = new URL(origin);
  if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || url.pathname !== '/' || url.search || url.hash) throw new Error('bare localhost relay origin required');
  return `${url.origin}/#${new URLSearchParams({ setup: token, label: 'P1 native', relay: url.origin.replace('http:', 'ws:') })}`;
}

export function piSettings(root, version) {
  return { tuiMode: 'regular', fullscreenExitOutput: 'resume-hint', externalEditor: join(root, 'editor.sh'), enabledModels: [], lastChangelogVersion: version };
}

export function piLaunch(bin, mode, pane, seed, root) {
  if (!['fullscreen', 'regular'].includes(mode)) throw new Error('explicit qualification mode required');
  return [bin, 'agent', 'start', `p1-scroll-${mode}`, '--kind', 'pi', '--pane', pane, '--timeout', '30000', '--', '--offline', '--tui-mode', mode, '--session', seed, '--extension', join(root, 'pi', 'evidence.mjs'), '--extension', join(root, 'pi', 'bridge.mjs'), '--extension', join(root, 'pi', 'herdr-agent-state.ts')];
}

export async function loggedCommand(args, { directory, label, env, cwd, timeout = 30000, identity, signalOwned, spawn = Bun.spawn, sleep = Bun.sleep }) {
  const log = join(directory, `${label}.log`);
  const record = { args, cwd, env, started: new Date().toISOString(), stdout: log, stderr: log + '.stderr', status: 'starting' };
  const save = () => writeFile(log + '.command.json', JSON.stringify(record, null, 2) + '\n', { mode: 0o600 });
  await save();
  let child;
  try {
    child = spawn(args, { env, cwd, stdin: 'ignore', stdout: Bun.file(log), stderr: Bun.file(log + '.stderr') });
    record.pid = child.pid;
    record.status = 'running';
    await save();
    let birth;
    try { birth = await identity(child.pid); } catch (error) { if (child.exitCode === null) throw error; }
    const deadline = Date.now() + timeout;
    while (child.exitCode === null && Date.now() < deadline) await sleep(20);
    if (child.exitCode === null) {
      await signalOwned(child, birth, 'SIGTERM');
      await sleep(200);
      if (child.exitCode === null) await signalOwned(child, birth, 'SIGKILL');
      await child.exited;
      throw new Error(`${label} timed out; no retry (${log})`);
    }
    record.exitCode = await child.exited;
    if (record.exitCode !== 0) throw new Error(`${label} exit ${record.exitCode}; full stdout/stderr: ${log}`);
    record.status = 'passed';
    return await readFile(log, 'utf8');
  } catch (error) {
    record.status = 'failed';
    record.error = String(error.stack || error);
    record.exitCode = child?.exitCode;
    throw error;
  } finally {
    record.ended = new Date().toISOString();
    await save();
  }
}

export async function preserveReceipt(directory, root, receipt) {
  const failures = [];
  if (root) {
    try {
      if (await realpath(root) !== root) throw new Error('fixture root identity changed');
      for (const name of await readdir(root)) {
        if (!['logs', 'pi', 'workspace.json', 'panes.json', 'fixture-owner', 'launch-fullscreen.json', 'launch-regular.json', 'pi-fullscreen.json', 'pi-regular.json', 'browser-ready.json', 'browser-done.json', 'editor.sh', 'editor-active', 'editor-done', 'overlay-done'].includes(name) && !name.startsWith('evidence')) continue;
        try { await cp(join(root, name), join(directory, name), { recursive: true, errorOnExist: true, force: false }); } catch (error) { failures.push(String(error)); }
      }
    } catch (error) { failures.push(String(error)); }
  }
  if (failures.length) {
    receipt.result = 'failed';
    receipt.preservationErrors = failures;
  }
  const bytes = JSON.stringify(receipt, null, 2) + '\n';
  await writeFile(join(directory, 'receipt.json'), bytes, { mode: 0o600, flag: 'wx' });
  if (root) await copyFile(join(directory, 'receipt.json'), join(root, 'receipt.json'));
  if (failures.length) throw new Error(`qualification evidence preservation failed: ${failures.join('; ')}`);
}
