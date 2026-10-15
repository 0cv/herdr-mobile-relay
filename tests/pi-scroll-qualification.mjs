import { chmod, copyFile, cp, mkdir, readFile, readdir, realpath, rename, stat, writeFile, rm } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { randomUUID, createHash } from 'node:crypto';

const repo = resolve(import.meta.dir, '..');
const output = process.env.HERDR_P1_QUALIFICATION_OUTPUT;
if (!output || !output.startsWith('/')) throw new Error('HERDR_P1_QUALIFICATION_OUTPUT must name the persistent review directory');
await mkdir(output, { recursive: true, mode: 0o700 });
await mkdir(join(output, '.active'), { mode: 0o700 });
let root, runDirectory;
const receipt = { schema: 1, result: 'failed', root: '', versions: {}, isolation: { socket: '', liveSocketUntouched: 'Never queried or contacted; all Herdr commands use a fresh private socket and clean environment.' }, modes: [], tests: [], viewportMoved: { up: false, down: false }, retries: 0, processCleanup: [], browserEndToEnd: 'remaining-gap: encrypted paired browser → real relay → Pi not exercised; browser specs use an explicit mock.', sourceHashes: {} };
const bins = Object.fromEntries(['herdr', 'pi', 'bun', 'go', 'node', 'lsof'].map(name => [name, Bun.which(name)]));
if (Object.values(bins).some(value => !value)) throw new Error('installed tools unavailable');
const path = [...new Set(Object.values(bins).map(value => value.slice(0, value.lastIndexOf('/')))), '/usr/bin', '/bin', '/usr/sbin', '/sbin'].join(':');
let env = { PATH: path, LC_ALL: 'C' }, server, isolated = false, activePi;
let sequence = 0;
const sleep = ms => Bun.sleep(ms);
const json = async file => JSON.parse(await readFile(file, 'utf8'));
const assert = (value, message) => { if (!value) throw new Error(message); };
const normalize = value => value.trim().replace(/\s+/g, ' ');

async function identity(pid) {
  assert(Number.isSafeInteger(pid) && pid > 0, 'invalid owned PID');
  const result = Bun.spawnSync(['/bin/ps', '-p', String(pid), '-o', 'lstart=', '-o', 'uid='], { env, cwd: repo });
  const fields = result.stdout.toString().trim().split(/\s+/);
  assert(result.exitCode === 0 && fields.length === 6 && Number(fields[5]) === process.getuid(), `process identity unavailable: ${pid}`);
  return fields.slice(0, 5).join(' ');
}

async function signalOwned(child, birth, signal) {
  if (child.exitCode !== null) return;
  assert(await identity(child.pid) === birth, 'owned child changed; no signal sent');
  child.kill(signal);
}

async function run(args, label, extra = {}, timeout = 30000) {
  const log = join(root, 'logs', `${String(++sequence).padStart(3, '0')}-${label}.log`);
  const child = Bun.spawn(args, { env: { ...env, ...extra }, cwd: repo, stdin: 'ignore', stdout: Bun.file(log), stderr: Bun.file(log + '.stderr') });
  let birth;
  try { birth = await identity(child.pid); } catch { if (child.exitCode === null) throw new Error(`could not bind ${label} child`); }
  const deadline = Date.now() + timeout;
  while (child.exitCode === null && Date.now() < deadline) await sleep(20);
  if (child.exitCode === null) {
    await signalOwned(child, birth, 'SIGTERM');
    await sleep(200);
    if (child.exitCode === null) await signalOwned(child, birth, 'SIGKILL');
    await child.exited;
    throw new Error(`${label} timed out; no retry (${log})`);
  }
  const code = await child.exited;
  const text = await readFile(log, 'utf8');
  assert(code === 0, `${label} exit ${code}: ${text.slice(-4000)} ${(await readFile(log + '.stderr', 'utf8')).slice(-4000)} (${log})`);
  return text;
}

async function proveIsolation() {
  assert(server && await identity(server.pid) === server.birth, 'isolation-unavailable: server identity changed');
  const info = await stat(env.HERDR_SOCKET_PATH);
  assert(info.isSocket() && info.uid === process.getuid() && (info.mode & 0o777) === 0o600, 'isolation-unavailable: private socket unavailable');
  const owners = await run([bins.lsof, '-n', '-P', '-a', '-p', String(server.pid), '-U', '-Fn'], 'socket-owner');
  assert(owners.split('\n').includes(`n${env.HERDR_SOCKET_PATH}`), 'isolation-unavailable: spawned server does not own private socket');
  isolated = true;
}

async function herdr(...args) {
  await proveIsolation();
  return run([bins.herdr, ...args], `herdr-${args[0]}-${args[1] || ''}`);
}

async function waitFile(file) {
  const deadline = Date.now() + 10000;
  while (Date.now() < deadline) {
    if (await Bun.file(file).exists()) return json(file);
    await sleep(100);
  }
  throw new Error(`fixture readiness unavailable: ${file}`);
}

async function command(pane, value) {
  await herdr('pane', 'send-text', pane, value);
  await herdr('pane', 'send-keys', pane, 'Enter');
}

async function fixture(mode, pane, terminal, session) {
  const evidenceDirectory = join(root, 'evidence');
  const previous = await readdir(evidenceDirectory);
  assert(previous.length === 0, 'evidence directory not fresh');
  const contract = JSON.stringify({ root, socketPath: env.HERDR_SOCKET_PATH, uid: process.getuid(), serverPid: server.pid, serverStart: server.birth, evidenceDirectory });
  const variables = { HERDR_P1_CONTRACT_JSON: contract };
  for (const prefix of mode === 'fullscreen' ? ['SCROLL', 'LATENCY', 'ROW'] : ['SCROLL']) {
    for (const [key, value] of Object.entries({ SOCKET: env.HERDR_SOCKET_PATH, PANE: pane, TERMINAL: terminal, SESSION: session, BIN: bins.herdr, MODE: mode })) variables[`HERDR_${prefix}_FIXTURE_${key}`] = value;
  }
  const output = await run([bins.go, 'test', '-p', '1', '-run', 'TestDisposablePi', './internal/app', './internal/coordinator', '-count=1', '-v'], `fixtures-${mode}`, variables, 180000);
  receipt.tests.push({ name: `TestDisposablePi (${mode})`, status: 'passed', output });
  if (mode === 'fullscreen') {
    const row = await json(join(evidenceDirectory, 'row-lease.json'));
    receipt.viewportMoved.up = true;
    receipt.viewportMoved.down = true;
    receipt.geometry = { initial: row.initial, leased: row.leased, restored: row.restored };
    receipt.sgr = { up: row.independent_up, down: row.independent_down };
  }
  await rename(evidenceDirectory, join(root, `evidence-${mode}`));
  await mkdir(evidenceDirectory, { mode: 0o700 });
}

try {
  const existing = (await readdir(output)).filter(name => /^run-[1-6]$/.test(name));
  const number = existing.length + 1;
  assert(number <= 6, 'six real qualification runs exhausted');
  runDirectory = join(output, `run-${number}`);
  await mkdir(runDirectory, { mode: 0o700 });
  const allocation = Bun.spawnSync(['/usr/bin/mktemp', '-d', '/tmp/herdr-scroll-p1-XXXXXX'], { env, cwd: repo });
  assert(allocation.exitCode === 0, 'fixture allocation failed');
  root = await realpath(allocation.stdout.toString().trim());
  receipt.root = root;
  console.log(`qualification root: ${root}`);
  await writeFile(join(root, 'fixture-owner'), 'disposable-scroll-fixture\n', { mode: 0o600 });
  for (const dir of ['home', 'config', 'config/herdr', 'cache', 'data', 'state', 'runtime', 'tmp', 'pi', 'work', 'logs', 'evidence']) await mkdir(join(root, dir), { recursive: true, mode: 0o700 });
  const modcache = Bun.spawnSync([bins.go, 'env', 'GOMODCACHE'], { env: { PATH: path, HOME: process.env.HOME }, cwd: repo }).stdout.toString().trim();
  env = { ...env, HOME: join(root, 'home'), USER: 'p1-fixture', LOGNAME: 'p1-fixture', SHELL: '/bin/sh', TERM: 'xterm-256color', TMPDIR: join(root, 'tmp'), XDG_CONFIG_HOME: join(root, 'config'), XDG_CACHE_HOME: join(root, 'cache'), XDG_DATA_HOME: join(root, 'data'), XDG_STATE_HOME: join(root, 'state'), XDG_RUNTIME_DIR: join(root, 'runtime'), HERDR_CONFIG_PATH: join(root, 'config/herdr/config.toml'), HERDR_SOCKET_PATH: join(root, 'config/herdr/herdr.sock'), PI_CODING_AGENT_DIR: join(root, 'pi'), PI_OFFLINE: '1', HERDR_P1_FIXTURE_ROOT: root, GOMODCACHE: modcache, GOPROXY: 'off', GOSUMDB: 'off', GOTOOLCHAIN: 'local', GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_TERMINAL_PROMPT: '0', BUN_RUNTIME_TRANSPILER_CACHE_PATH: '0' };
  receipt.isolation.socket = env.HERDR_SOCKET_PATH;
  assert(env.HERDR_SOCKET_PATH.startsWith(root + '/') && !await Bun.file(env.HERDR_SOCKET_PATH).exists(), 'isolation-unavailable: socket is not fresh');
  await writeFile(env.HERDR_CONFIG_PATH, '[update]\nversion_check = false\nmanifest_check = false\n', { mode: 0o600 });
  for (const [name, args] of Object.entries({ pi: ['--version'], herdr: ['--version'], bun: ['--version'], go: ['version'] })) receipt.versions[name] = normalize(await run([bins[name], ...args], `${name}-version`));
  for (const [source, target] of [['relay/pi-command-bridge/bridge.mjs', 'bridge.mjs'], ['tests/pi-scroll-evidence-extension.mjs', 'evidence.mjs'], ['tests/pi-scroll-capture.mjs', 'capture.mjs']]) {
    await copyFile(join(repo, source), join(root, 'pi', target));
    await chmod(join(root, 'pi', target), 0o600);
  }
  await writeFile(join(root, 'pi', 'settings.json'), JSON.stringify({ tuiMode: 'regular', enabledModels: [], lastChangelogVersion: receipt.versions.pi }), { mode: 0o600 });
  const timestamp = new Date().toISOString(), session = randomUUID();
  const entries = [{ type: 'session', version: 3, id: session, timestamp, cwd: join(root, 'work') }];
  entries.push({ type: 'message', id: '00000001', parentId: null, timestamp, message: { role: 'user', content: Array.from({ length: 96 }, (_, i) => `P1 TEST USER [${String(i + 1).padStart(3, '0')}] generated test content`).join('\n'), timestamp: Date.now() } });
  const sessionDirectory = join(root, 'pi', 'sessions', `--${join(root, 'work').slice(1).replaceAll('/', '-')}--`);
  await mkdir(sessionDirectory, { recursive: true, mode: 0o700 });
  const seed = join(sessionDirectory, `${timestamp.replaceAll(':', '-').replaceAll('.', '-')}_${session}.jsonl`);
  await writeFile(seed, entries.map(entry => JSON.stringify(entry)).join('\n') + '\n', { mode: 0o600 });
  const child = Bun.spawn([bins.herdr, 'server'], { env, cwd: repo, stdin: 'ignore', stdout: Bun.file(join(root, 'logs', 'herdr-server.log')), stderr: Bun.file(join(root, 'logs', 'herdr-server.stderr')) });
  server = { pid: child.pid, birth: await identity(child.pid), child };
  const deadline = Date.now() + 10000;
  while (!await Bun.file(env.HERDR_SOCKET_PATH).exists() && Date.now() < deadline && child.exitCode === null) await sleep(100);
  await proveIsolation();
  receipt.serverStatus = JSON.parse(await herdr('status', 'server', '--json'));
  const workspace = JSON.parse(await herdr('workspace', 'create', '--cwd', join(root, 'work'), '--label', 'p1-scroll-fixture', '--no-focus', '--env', `PI_CODING_AGENT_DIR=${env.PI_CODING_AGENT_DIR}`, '--env', 'PI_OFFLINE=1', '--env', `HERDR_P1_FIXTURE_ROOT=${root}`, '--env', `NODE_OPTIONS=--import=${join(root, 'pi', 'capture.mjs')}`));
  await writeFile(join(root, 'workspace.json'), JSON.stringify(workspace), { mode: 0o600 });
  const panes = JSON.parse(await herdr('pane', 'list'));
  await writeFile(join(root, 'panes.json'), JSON.stringify(panes), { mode: 0o600 });
  assert(panes.result?.type === 'pane_list', 'unexpected isolated pane-list response');
  const records = panes.result.panes;
  assert(records?.length === 1, 'isolated workspace must have exactly one pane');
  const pane = records[0].pane_id, terminal = records[0].terminal_id;
  assert(pane && terminal, 'missing pane/terminal identity');
  for (const mode of ['fullscreen', 'regular']) {
    const launch = await herdr('agent', 'start', 'p1-scroll-fixture', '--kind', 'pi', '--pane', pane, '--timeout', '30000', '--', '--offline', '--tui-mode', mode, '--session', seed, '--extension', join(root, 'pi', 'evidence.mjs'), '--extension', join(root, 'pi', 'bridge.mjs'));
    await writeFile(join(root, `launch-${mode}.json`), launch, { mode: 0o600 });
    const ready = await waitFile(join(root, `pi-${mode}.json`));
    activePi = { pid: ready.pid, birth: await identity(ready.pid) };
    assert(ready.session === session && ready.file === seed, 'Pi did not load the exact disposable transcript');
    receipt.modes.push({ mode, pid: ready.pid, session: ready.session });
    await sleep(1000);
    await herdr('pane', 'read', pane, '--source', 'visible', '--lines', '80', '--format', 'text');
    try {
      await fixture(mode, pane, terminal, ready.session);
    } catch (error) {
      receipt.tests.push({ name: `TestDisposablePi (${mode})`, status: 'failed', error: String(error) });
      receipt.error = String(error);
      process.exitCode = 1;
      await rename(join(root, 'evidence'), join(root, `evidence-${mode}`));
      await mkdir(join(root, 'evidence'), { mode: 0o700 });
    }
    await command(pane, '/quit');
    const quitDeadline = Date.now() + 5000;
    let running = true;
    while (running && Date.now() < quitDeadline) {
      try { running = await identity(activePi.pid) === activePi.birth; } catch { running = false; }
      if (running) await sleep(50);
    }
    assert(!running, 'owned Pi did not exit after /quit');
    receipt.processCleanup.push({ process: 'pi', mode, pid: activePi.pid, start: activePi.birth, action: 'exited after /quit in private pane' });
    activePi = undefined;
  }
  const received = [];
  for (const mode of ['fullscreen', 'regular']) {
    const events = (await readFile(join(root, 'logs', `pi-${mode}-events.jsonl`), 'utf8')).trim().split('\n').map(line => JSON.parse(line));
    assert(!events.some(event => event.event === 'forbidden_provider_request'), 'model request observed');
    const input = Buffer.concat(events.filter(event => event.event === 'input').map(event => Buffer.from(event.base64, 'base64')));
    received.push({ mode, inputBase64: input.toString('base64'), sgr: [...input.toString().matchAll(/\x1b\[<6[45];\d+;\d+M/g)].map(match => match[0]) });
    const widgets = events.filter(event => event.event === 'widget');
    assert(widgets.length > 0 && widgets.every(event => event.rows === 0 && event.inputHandler === 'undefined' && event.mouseHandler === 'undefined'), 'widget is not invisible/noncapturing');
    assert(mode !== 'regular' || !input.includes(Buffer.from('\x1b[<6')), 'regular mode received wheel input');
  }
  receipt.received = received;
  if (receipt.sgr) {
    assert(received[0].sgr.includes(receipt.sgr.up) && received[0].sgr.includes(receipt.sgr.down) && received[0].sgr.length === 8, 'centered Up/Down bytes not received exactly once by real Pi');
  }
  receipt.bracketedPasteEnabled = (await readFile(join(root, 'logs', 'pi-output.raw'))).includes(Buffer.from('\x1b[?2004h'));
  assert(receipt.bracketedPasteEnabled, 'Pi bracketed paste enable was not captured');
  if (receipt.tests.every(test => test.status === 'passed') && receipt.viewportMoved.up && receipt.viewportMoved.down) receipt.result = 'passed';
} catch (error) {
  receipt.error = String(error.stack || error);
  console.error(receipt.error);
  process.exitCode = 1;
} finally {
  if (activePi) {
    try {
      assert(await identity(activePi.pid) === activePi.birth, 'Pi identity changed; no signal sent');
      process.kill(activePi.pid, 'SIGTERM');
      receipt.processCleanup.push({ process: 'pi', pid: activePi.pid, start: activePi.birth, action: 'verified SIGTERM' });
    } catch (error) { receipt.processCleanup.push({ process: 'pi', error: String(error) }); }
  }
  if (server && isolated) {
    try {
      await herdr('server', 'stop');
      const deadline = Date.now() + 5000;
      while (server.child.exitCode === null && Date.now() < deadline) await sleep(50);
      assert(server.child.exitCode !== null, 'isolated server did not stop');
      receipt.processCleanup.push({ process: 'herdr', action: 'stopped through private socket', pid: server.pid, start: server.birth });
    } catch (error) { receipt.processCleanup.push({ process: 'herdr', error: String(error) }); receipt.result = 'failed'; process.exitCode = 1; }
  } else if (server) {
    receipt.processCleanup.push({ process: 'herdr', action: 'isolation-unavailable; not signalled' });
  }
  if (receipt.result !== 'passed') process.exitCode = 1;
  if (root) {
    const files = Bun.spawnSync(['git', 'diff', '--name-only', 'f51e206a289610de849043822296c484338b92a0'], { env, cwd: repo }).stdout.toString().trim().split('\n');
    for (const file of [...files, 'tests/pi-scroll-qualification.mjs']) {
      if (await Bun.file(join(repo, file)).exists()) receipt.sourceHashes[file] = createHash('sha256').update(await readFile(join(repo, file))).digest('hex');
    }
    await writeFile(join(root, 'receipt.json.tmp'), JSON.stringify(receipt, null, 2) + '\n', { mode: 0o600 });
    await rename(join(root, 'receipt.json.tmp'), join(root, 'receipt.json'));
    if (runDirectory) {
      await copyFile(join(root, 'receipt.json'), join(runDirectory, 'receipt.json'));
      for (const name of (await readdir(root)).filter(name => name === 'logs' || name.startsWith('evidence'))) {
        if ((await stat(join(root, name))).isDirectory()) await cp(join(root, name), join(runDirectory, name), { recursive: true });
      }
    }
    console.log(`receipt: ${runDirectory}/receipt.json`);
  }
  await rm(join(output, '.active'), { recursive: true });
}
