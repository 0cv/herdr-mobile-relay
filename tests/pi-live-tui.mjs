import assert from 'node:assert/strict';
import { createServer, createConnection } from 'node:net';
import { spawn, spawnSync } from 'node:child_process';
import { mkdir, readFile, writeFile, lstat, rmdir } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { instanceIdentity } from '../relay/pi-command-bridge/bridge.mjs';

const root = resolve(process.env.PI_LIVE_TEST_WORK || '');
const packageRoot = resolve(process.env.PI_TEST_PACKAGE || '');
assert(process.env.PI_LIVE_TEST_WORK && process.env.PI_TEST_PACKAGE && process.env.HOME === join(root, 'home'));
assert(process.env.PI_TEST_BUN, 'Explicit Bun binary required for the private PTY');
const manifest = JSON.parse(await readFile(join(packageRoot, 'package.json'), 'utf8'));
assert.equal(manifest.version, '1.3.0');
const repo = resolve(import.meta.dirname, '..');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const results = [];
async function qualifyBrowser(work, onReady) {
  const fixture = spawn(process.env.PI_TEST_APP_FIXTURE, ['-test.run', '^TestNativePiActivityBrowserFixture$', '-test.timeout', '35s'], { cwd: repo, env: { PATH: process.env.PATH, HOME: join(work, 'home'), TMPDIR: work, XDG_CONFIG_HOME: join(work, 'config'), XDG_CACHE_HOME: join(work, 'cache'), XDG_DATA_HOME: join(work, 'data'), XDG_RUNTIME_DIR: join(work, 'runtime'), PI_NATIVE_BROWSER_WORK: work, PI_NATIVE_BROWSER_ROLE: process.env.PI_TEST_BROWSER_ROLE || '', PI_NATIVE_BROWSER_WEB: join(repo, 'frontend/dist'), PI_OFFLINE: '1' }, stdio: ['ignore', 'pipe', 'pipe'] });
  let log = '';
  fixture.stdout.on('data', chunk => { log += chunk.toString(); }); fixture.stderr.on('data', chunk => { log += chunk.toString(); });
  let exited = false;
  const ended = new Promise(resolve => fixture.once('exit', code => { exited = true; resolve(code); }));
  let browser;
  let config;
  let page;
  try {
    const deadline = Date.now() + 15000;
    while (!config && Date.now() < deadline && !exited) { try { config = JSON.parse(await readFile(join(work, 'browser-config.json'), 'utf8')); } catch { await sleep(20); } }
    assert(config, 'Private encrypted relay fixture did not start');
    const { chromium, devices } = await import(pathToFileURL(join(repo, 'frontend/node_modules/playwright/index.mjs')));
    browser = await chromium.launch({ headless: true, env: { PATH: process.env.PATH, HOME: join(work, 'home'), TMPDIR: work } });
    const context = await browser.newContext({ ...devices['Pixel 7'], serviceWorkers: 'block' });
    await context.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.continue() : route.abort());
    await context.addInitScript(config => {
      localStorage.setItem('herdr_relays', JSON.stringify([{ id: 'private-native', label: 'Native relay', url: config.ws, token: config.token }]));
      if (config.invitation) localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { 'private-native': { kind: 'invitation', id: config.invitation.invitation_id, version: config.invitation.version, secret: config.invitation.secret, expiresAt: Date.parse(config.invitation.expires_at) } } }));
      localStorage.setItem('herdr_default_agent_view', 'conversation'); localStorage.setItem('herdr-conversation-view', 'activity');
    }, config);
    page = await context.newPage();
    await page.goto(config.url);
    await page.getByRole('button', { name: 'Open Native live on Native relay' }).click({ timeout: 10000 });
    await page.getByText('Live events · provisional text', { exact: true }).waitFor({ timeout: 10000 });
    const assertReaderConversation = async () => {
      assert.equal(await page.getByRole('textbox', { name: 'Prompt', exact: true }).isDisabled(), true);
      for (const name of ['Attach photos', 'Attach files', 'Send prompt']) assert.equal(await page.getByRole('button', { name, exact: true }).isDisabled(), true);
    };
    if (config.role === 'reader') {
      assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_device_auth_v1')).relays['private-native'].role), 'reader');
      await assertReaderConversation();
    }
    onReady();
    await page.getByText('Synthetic private qualification prompt', { exact: true }).waitFor({ timeout: 5000 });
    const partial = page.locator('[data-conversation-entry-id^="live:"]');
    await partial.waitFor({ timeout: 10000 });
    const first = await partial.textContent();
    await page.waitForFunction(first => { const node = document.querySelector('[data-conversation-entry-id^="live:"]'); return node && node.textContent !== first; }, first, { timeout: 5000 });
    await page.locator('[data-conversation-entry-id^="pi:"]').filter({ hasText: 'Visible fragment 39.' }).waitFor({ timeout: 10000 });
    assert.equal(await partial.count(), 0);
    if (config.role === 'reader') {
      await assertReaderConversation();
      await page.getByRole('button', { name: 'Conversation', exact: true }).click();
      await page.getByText('Visible fragment 39.', { exact: false }).waitFor();
      await assertReaderConversation();
      await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
      assert.equal(await page.getByRole('combobox', { name: 'Prompt', exact: true }).isDisabled(), true);
      for (const name of ['Attach photos', 'Attach files', 'Enter', 'Tab', 'Ctrl', 'Shift', 'Arrow keys', 'Function keys']) assert.equal(await page.getByRole('button', { name, exact: true }).isDisabled(), true);
      assert.equal(await page.locator('.terminal-find button:enabled, .terminal-find select:enabled').count(), 0);
      await page.getByRole('log').dispatchEvent('wheel', { deltaY: -200 });
      await page.setViewportSize({ width: 390, height: 740 });
    }
    await writeFile(join(work, 'browser-done'), 'ok', { mode: 0o600 });
    assert.equal(await ended, 0);
    const result = JSON.parse(await readFile(join(work, 'browser-result.json'), 'utf8'));
    assert.equal(result.authenticated_browser, true); assert.equal(result.agent_mutations, 0);
    assert.equal(result.role, process.env.PI_TEST_BROWSER_ROLE || 'controller');
    if (result.role === 'reader') assert.equal(result.received_methods.acknowledge_pane || 0, 0);
    return result;
  } catch (error) {
    if (page) await writeFile(join(work, 'browser-failure.txt'), (await page.locator('body').innerText()).replaceAll(config?.token || 'not-a-token', '[redacted]'), { mode: 0o600 });
    throw error;
  } finally {
    await browser?.close();
    if (!exited) { fixture.kill('SIGTERM'); await ended; }
    await writeFile(join(work, 'browser-fixture.log'), log, { mode: 0o600 });
  }
}
for (const mode of ['fullscreen', 'regular']) {
  const work = join(root, mode);
  for (const path of ['home', 'agent', 'project', 'config', 'cache', 'data', 'runtime']) await mkdir(join(work, path), { recursive: true, mode: 0o700 });
  await writeFile(join(work, 'agent/settings.json'), JSON.stringify({ enableInstallTelemetry: false, cacheWarming: 'off', compaction: { enabled: false }, defaultProjectTrust: 'never', quietStartup: true, defaultTools: [] }), { mode: 0o600 });
  const socketPath = join(work, 'herdr.sock');
  const herdrMethods = [];
  const herdr = createServer(socket => {
    let pending = '';
    socket.on('error', () => {});
    socket.on('data', async chunk => {
      pending += chunk.toString();
      if (!pending.includes('\n')) return;
      try {
        const request = JSON.parse(pending);
        herdrMethods.push(request.method);
        const identity = JSON.parse(await readFile(join(work, 'identity.json'), 'utf8'));
        if (request.method !== 'pane.process_info') { socket.end(JSON.stringify({ id: request.id, error: { code: 'unknown_method', message: 'Private fixture only permits process observation' } }) + '\n'); return; }
        socket.end(JSON.stringify({ id: request.id, result: { type: 'pane_process_info', process_info: { pane_id: 'fixture-pane', shell_pid: child.pid, foreground_process_group_id: identity.pid, foreground_processes: [{ pid: identity.pid, name: 'pi' }] } } }) + '\n');
      } catch { socket.destroy(); }
    });
  });
  await new Promise((resolve, reject) => { herdr.once('error', reject); herdr.listen(socketPath, resolve); });
  const instance = await instanceIdentity(socketPath);
  const directory = `/tmp/herdr-pi-${process.getuid()}-${instance.slice(0, 16)}`;
  const guard = join(work, 'network-guard.mjs');
  await writeFile(guard, `import net from 'node:net'; import tls from 'node:tls'; import http from 'node:http'; import https from 'node:https'; import { syncBuiltinESMExports } from 'node:module'; import { writeFileSync } from 'node:fs'; const deny = () => { writeFileSync(${JSON.stringify(join(work, 'network-attempt'))}, 'blocked', { mode: 0o600 }); throw new Error('Network forbidden in private fixture'); }; globalThis.fetch=deny; net.Socket.prototype.connect=deny; net.connect=deny; net.createConnection=deny; tls.connect=deny; http.request=deny; http.get=deny; https.request=deny; https.get=deny; syncBuiltinESMExports();`, { mode: 0o600 });
  const node = process.execPath;
  const args = [node, '--import', guard, join(packageRoot, manifest.bin.pi), '--offline', '--no-approve', '--no-extensions', '--no-skills', '--no-prompt-templates', '--no-themes', '--no-context-files', '--no-tools', '--extension', join(repo, 'tests/fixtures/pi-live-provider.mjs'), '--extension', join(repo, 'relay/pi-command-bridge/index.ts'), '--provider', 'herdr-native-fixture', '--model', 'synthetic', '--thinking', 'off', '--tui-mode', mode];
  const child = spawn(process.env.PI_TEST_BUN, ['--no-env-file', join(repo, 'tests/fixtures/pi-live-pty.mjs')], {
    cwd: join(work, 'project'),
    env: { PI_TEST_APP_FIXTURE: process.env.PI_TEST_APP_FIXTURE || '', PI_LIVE_PTY_ARGS: JSON.stringify(args), PATH: `${resolve(node, '..')}:/usr/bin:/bin`, HOME: join(work, 'home'), XDG_CONFIG_HOME: join(work, 'config'), XDG_CACHE_HOME: join(work, 'cache'), XDG_DATA_HOME: join(work, 'data'), XDG_RUNTIME_DIR: join(work, 'runtime'), TMPDIR: work, PI_CODING_AGENT_DIR: join(work, 'agent'), PI_LIVE_TEST_WORK: work, PI_TEST_PACKAGE: packageRoot, PI_OFFLINE: '1', PI_SKIP_VERSION_CHECK: '1', PI_TELEMETRY: '0', HERDR_PANE_ID: 'fixture-pane', HERDR_SOCKET_PATH: socketPath, TERM: 'xterm-256color', COLUMNS: '100', LINES: '40' },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  let output = '';
  const capture = chunk => { if (output.length < 4 * 1024 * 1024) output += chunk.toString(); };
  child.stdout.on('data', capture); child.stderr.on('data', capture);
  let exited = false;
  const exit = new Promise(resolve => child.once('exit', (code, signal) => { exited = true; resolve({ code, signal }); }));
  const timer = setTimeout(() => { child.stdin.end(); child.kill('SIGTERM'); }, 20000);
  let stream;
  try {
    let identity;
    const deadline = Date.now() + 12000;
    while (Date.now() < deadline && !exited) {
      try { identity = JSON.parse(await readFile(join(work, 'identity.json'), 'utf8')); await lstat(join(directory, `${identity.pid}.activity.sock`)); break; } catch { await sleep(20); }
    }
    assert(identity, 'Stock TUI did not start the public fixture extension');
    assert.equal(identity.mode, 'tui');
    const target = { instance, pane: 'fixture-pane', session: identity.session, pid: identity.pid, challenge: 'c'.repeat(32) };
    stream = createConnection(join(directory, `${identity.pid}.activity.sock`));
    const frames = [];
    let pending = '';
    await new Promise((resolve, reject) => {
      stream.on('connect', () => { stream.write(JSON.stringify({ ...target, operation: 'subscribe' }) + '\n'); resolve(); });
      stream.on('error', reject);
      stream.on('data', chunk => { pending += chunk.toString(); let index; while ((index = pending.indexOf('\n')) >= 0) { frames.push(JSON.parse(pending.slice(0, index))); pending = pending.slice(index + 1); } });
    });
    const query = operation => new Promise((resolve, reject) => {
      const socket = createConnection(join(directory, `${identity.pid}.sock`)); let data = '';
      socket.setTimeout(1500, () => socket.destroy(new Error('Catalog query timed out')));
      socket.on('connect', () => socket.write(JSON.stringify({ ...target, operation }) + '\n'));
      socket.on('data', chunk => { data += chunk.toString(); }); socket.on('error', reject); socket.on('end', () => { try { resolve(JSON.parse(data)); } catch(error) { reject(error); } });
    });
    let attached = () => {};
    const browserReady = process.env.PI_TEST_APP_FIXTURE ? new Promise(resolve => { attached = resolve; }) : Promise.resolve();
    const browserQualification = process.env.PI_TEST_APP_FIXTURE ? qualifyBrowser(work, attached).then(result => ({ result }), error => { attached(); return { error }; }) : Promise.resolve({ result: undefined });
    const before = await query('catalog');
    assert.equal(before.session, identity.session);
    assert(Array.isArray(before.commands));
    await browserReady;
    child.stdin.write('Synthetic private qualification prompt\r');
    const responseDeadline = Date.now() + 12000;
    while (!frames.some(frame => frame.entries?.some(entry => entry.id.startsWith('pi:') && entry.role === 'assistant')) && !exited && Date.now() < responseDeadline) await sleep(20);
    const partials = frames.flatMap(frame => frame.entries || []).filter(entry => entry.id.startsWith('live:'));
    const finals = frames.flatMap(frame => frame.entries || []).filter(entry => entry.id.startsWith('pi:') && entry.role === 'assistant');
    assert(partials.length >= 2, 'Expected incremental stock TUI partials');
    assert.equal(finals.length, 1);
    assert.equal(new Set(partials.map(entry => entry.id)).size, 1);
    assert(frames.some(frame => frame.remove?.includes(partials[0].id)));
    const desktopInput = JSON.parse(await readFile(join(work, 'desktop-input.json'), 'utf8'));
    assert.equal(desktopInput.accepted, true); assert.equal(desktopInput.source, 'interactive');
    const paired = await browserQualification;
    if (paired.error) throw paired.error;
    const after = await query('catalog');
    assert.equal(after.session, identity.session);
    assert(Array.isArray(after.commands));
    assert.equal(before.incarnation, after.incarnation);
    const status = await exit;
    assert.equal(status.code, 0);
    await assert.rejects(lstat(join(directory, `${identity.pid}.activity.sock`)));
    await assert.rejects(lstat(join(work, 'network-attempt')));
    assert(herdrMethods.length > 0 && herdrMethods.every(method => method === 'pane.process_info'));
    results.push({ herdrMethods: [...new Set(herdrMethods)], mode, stockVersion: manifest.version, partialBatches: partials.length, finalEntries: finals.length, catalogIdentityUnchanged: true, desktopInputQualified: true, shutdownRemovedSocket: true, networkAttempts: 0, ...(paired.result ? { enrolledBrowser: paired.result } : {}) });
  } finally {
    clearTimeout(timer); stream?.destroy();
    if (!exited) {
      try {
        const identity = JSON.parse(await readFile(join(work, 'identity.json'), 'utf8'));
        const info = spawnSync('/bin/ps', ['-p', String(identity.pid), '-o', 'ppid=', '-o', 'uid='], { encoding: 'utf8', env: { PATH: '/usr/bin:/bin' } });
        const [parent, uid] = info.stdout.trim().split(/\s+/).map(Number);
        if (parent === child.pid && uid === process.getuid()) process.kill(identity.pid, 'SIGTERM');
      } catch (error) { output += `\nPrivate fixture cleanup: ${error.code || 'unavailable'}\n`; }
      child.stdin.end(); child.kill('SIGTERM'); await exit;
    }
    await new Promise(resolve => herdr.close(resolve));
    await writeFile(join(work, 'terminal.log'), output, { mode: 0o600 });
    await rmdir(directory).catch(() => {});
  }
}
await writeFile(join(root, 'result.json'), JSON.stringify({ schema: 1, qualification: 'stock TUI synthetic provider; direct private bridge, not physical phone or real provider', results }, null, 2) + '\n', { mode: 0o600 });
console.log(JSON.stringify({ results }));
