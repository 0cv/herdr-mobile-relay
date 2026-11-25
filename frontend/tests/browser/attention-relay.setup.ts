import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createServer } from 'node:net';
import { access, appendFile, mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { parseArgs } from 'node:util';
import type { FullConfig } from '@playwright/test';
import { readerResultPane } from './attention-relay-fixture';

const repoRoot = resolve(import.meta.dirname, '../../..');
const captureRoot = join(repoRoot, 'internal/question/testdata/attention');

async function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      if (!address || typeof address === 'string') {
        server.close();
        reject(new Error('Could not allocate a loopback port'));
        return;
      }
      server.close((error) => {
        if (error) reject(error);
        else resolvePort(address.port);
      });
    });
  });
}

function build(bin: string, target: string, cache: string): void {
  const result = spawnSync('go', ['build', '-o', bin, target], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: { ...process.env, GOCACHE: cache },
  });
  if (result.status === 0) return;
  throw new Error(`Could not build ${target}:\n${result.stdout}\n${result.stderr}`);
}

async function waitForHealth(baseURL: string, relayOutput: () => string): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    try {
      const response = await fetch(`${baseURL}/health`);
      if (response.ok) return;
    } catch {
      // Relay startup can race the first few requests.
    }
    await new Promise((resolveWait) => setTimeout(resolveWait, 50));
  }
  throw new Error(`Attention relay did not become healthy:\n${relayOutput()}`);
}

async function stopRelay(relay: ChildProcess): Promise<void> {
  if (relay.exitCode !== null || relay.signalCode !== null) return;
  relay.kill('SIGINT');
  await Promise.race([
    new Promise<void>((resolveExit) => relay.once('exit', () => resolveExit())),
    new Promise<void>((resolveWait) => setTimeout(resolveWait, 3_000)),
  ]);
  if (relay.exitCode === null && relay.signalCode === null) relay.kill('SIGKILL');
}

interface PaneRow {
  name: string;
  agent: string;
  status: string;
  content: string;
  session?: string;
  cwd?: string;
  workspace?: string;
}

const attentionCaptures: [name: string, agent: string, capture: string][] = [
  ['qoder-approval', 'qodercli', 'qodercli-permission-required2'],
  ['qoder-notes', 'qodercli', 'qodercli-multi-questions-and-notes'],
  ['codex-notes', 'codex', 'codex-first-question_with_notes'],
  ['codex-final-question', 'codex', 'codex-middle-question'],
  ['codex-single-question', 'codex', 'codex-single-question'],
  ['codex-plan-approval', 'codex', 'codex-implement-plan'],
  ['claude-approval', 'claude', 'claude-single-approval'],
  ['claude-later-question', 'claude', 'claude-plan-multi-question'],
  ['claude-custom-answer', 'claude', 'claude-plan-one-question-notes'],
  ['claude-multi-select', 'claude', 'claude-multi-select-with-free-text'],
  ['claude-review', 'claude', 'claude-plan-submit-answers'],
  ['opencode-single', 'opencode', 'opencode-single-question'],
  ['opencode-custom', 'opencode', 'opencode-questions-multiple-choice-with-free-text-edited'],
  ['opencode-review', 'opencode', 'opencode-questions-with-multiple-choice-answers-confirm'],
  ['qoder-standalone', 'qodercli', 'qodercli-single-question'],
  ['qoder-settings', 'qodercli', 'qodercli-yes-no'],
  ['omp-plan-approval', 'omp', 'omp-plan-approval'],
  ['omp-partial-ask', 'omp', 'omp-partial-ask'],
];

export default async function setup(config: FullConfig) {
  const runtime = await mkdtemp(join(tmpdir(), 'herdr-attention-browser-'));
  const cache = process.env.GOCACHE || join(runtime, 'go-cache');
  const fakeBin = join(runtime, 'fake-herdr');
  const relayBin = join(runtime, 'herdr-mobile-relay');
  const scenarioPath = join(runtime, 'scenario.json');
  const operationsPath = join(runtime, 'operations.jsonl');
  const inputOperationsPath = join(runtime, 'terminal-input.jsonl');
  const metadataPath = join(runtime, 'metadata.json');
  const controlPath = join(runtime, 'control.sock');
  const socketPath = join(runtime, 'herdr.sock');
  const barrierDir = join(runtime, 'barriers');
  await writeFile(metadataPath, '{}', { mode: 0o600 });
  const webRoot = join(runtime, 'web');
  const conversationRoot = join(runtime, 'claude');
  // Playwright applies the CLI repeat override after global setup.
  const { values: { 'repeat-each': repeatOverride } } = parseArgs({
    options: { 'repeat-each': { type: 'string' } }, strict: false, allowPositionals: true,
  });
  const readerResults = config.projects.flatMap(({ name, repeatEach, retries }) =>
    Array.from({ length: repeatOverride === undefined ? repeatEach : Number(repeatOverride) }, (_, repeat) =>
      Array.from({ length: retries + 1 }, (_, retry) => readerResultPane(name, repeat, retry)),
    ).flat(),
  ).map((name, index) => ({
    name, workspace: name, agent: 'claude', status: 'succeeded', content: 'Fixture result',
    session: `123e4567-e89b-12d3-a456-${426614174003 + index}`, cwd: '/tmp/conversation',
  }));
  await Promise.all([mkdir(cache, { recursive: true }), mkdir(webRoot), mkdir(barrierDir), mkdir(join(conversationRoot, 'projects', '-tmp-conversation'), { recursive: true })]);
  await writeFile(join(webRoot, 'index.html'), '<html>test</html>\n');
  const history = [
    JSON.stringify({ type: 'user', uuid: 'history-user-1', timestamp: '2026-09-02T10:00:00Z', message: { content: 'history question' } }),
    JSON.stringify({ type: 'assistant', uuid: 'history-assistant-1', timestamp: '2026-09-02T10:00:01Z', message: { content: [{ type: 'text', text: 'history answer' }] } }),
  ].join('\n') + '\n';
  await Promise.all(readerResults.map(({ session }) =>
    writeFile(join(conversationRoot, 'projects', '-tmp-conversation', `${session}.jsonl`), history),
  ));

  for (const [project, session] of [['arbitrary-dialog', '123e4567-e89b-12d3-a456-426614174001'], ['orchestration-dialog', '123e4567-e89b-12d3-a456-426614174002'], ['same-target-new-directory', '123e4567-e89b-12d3-a456-426614174001']]) {
    const directory = join(conversationRoot, 'projects', `-tmp-${project}`);
    await mkdir(directory, { recursive: true });
    await writeFile(join(directory, `${session}.jsonl`), history);
  }

  // The literal-input dialogs have no input prompt line, so the relay cannot classify them: they need
  // inspection, the composer cannot send, and the Keyboard button is offered.
  const literalDialog = [
    'scrollback-anchor-000',
    ...Array.from({ length: 199 }, (_, index) => `history-entry-${index + 1}`),
    'Destination for /violet-comet',
    'Custom answer: Where shall we go?',
    'enter or ctrl+q submit  esc cancel  ctrl+g external editor',
  ].join('\n');
  const rows: PaneRow[] = [
    { name: 'arbitrary-dialog', agent: 'claude', status: 'blocked', content: literalDialog, session: '123e4567-e89b-12d3-a456-426614174001' },
    { name: 'orchestration-dialog', agent: 'claude', status: 'blocked', content: 'Guidance for /orchestrate-answer\nCustom answer: Add guidance\nenter or ctrl+q submit  esc cancel  ctrl+g external editor', session: '123e4567-e89b-12d3-a456-426614174002' },
    ...await Promise.all(attentionCaptures.map(async ([name, agent, capture]) => ({
      name, agent, status: 'blocked', content: await readFile(join(captureRoot, `${capture}.ansi`), 'utf8'),
    }))),
    ...readerResults,
  ];
  const scenario = {
    socket_read_panes: readerResults.map(({ name }) => name),
    responses: Object.fromEntries([30, 100, 500, 1_000, 10_000].flatMap(lines =>
      ['visible', 'recent', 'recent-unwrapped'].map(source => [
        ['pane', 'read', 'arbitrary-dialog', '--lines', String(lines), '--source', source, '--format', 'ansi'].join('\0'),
        literalDialog.split('\n').slice(-lines).join('\n'),
      ]))),
    focused_workspace_id: 'workspace-1',
    workspaces: [
      { workspace_id: 'workspace-1', number: 1, label: 'Captured', pane_count: rows.length - readerResults.length, tab_count: rows.length - readerResults.length, cwd: '/tmp' },
      ...readerResults.map(({ workspace, cwd }, index) => ({
        workspace_id: workspace, number: index + 2, label: `Result fixture ${workspace}`, pane_count: 1, tab_count: 1, cwd,
      })),
    ],
    panes: rows.map(({ name, agent, status, session, cwd, workspace }) => ({
      pane_id: name, terminal_id: `terminal-${name}`, agent, name, agent_status: status, tab_id: `${name}-tab`,
      workspace_id: workspace ?? 'workspace-1', cwd: cwd ?? `/tmp/${name}`, revision: 1,
      ...(session ? { agent_session: { value: session, kind: 'uuid' } } : {}),
    })),
    tabs: rows.map(({ name, cwd, workspace }, index) => ({
      tab_id: `${name}-tab`, workspace_id: workspace ?? 'workspace-1', label: name, number: workspace ? 1 : index + 1, cwd: cwd ?? `/tmp/${name}`,
    })),
    content: Object.fromEntries(rows.map(({ name, content }) => [name, content])),
  };
  await writeFile(scenarioPath, JSON.stringify(scenario));

  build(fakeBin, './cmd/fake-herdr', cache);
  build(relayBin, './cmd/herdr-mobile-relay', cache);

  const startFakeHerdr = async (mode: string, env: NodeJS.ProcessEnv, readyPath: string, logKind: string): Promise<ChildProcess> => {
    const child = spawn(fakeBin, [mode], { cwd: repoRoot, env, stdio: ['ignore', 'ignore', 'pipe'] });
    if (process.env.HERDR_ATTENTION_PROCESS_LOG) await appendFile(process.env.HERDR_ATTENTION_PROCESS_LOG, JSON.stringify({ kind: logKind, pid: child.pid, bin: fakeBin, runtime }) + '\n', { mode: 0o600 });
    let output = '';
    child.stderr?.on('data', chunk => { output += String(chunk); });
    for (let attempt = 0; attempt < 100; attempt++) {
      try { await access(readyPath); return child; }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
      await new Promise(resolveWait => setTimeout(resolveWait, 20));
    }
    await stopRelay(child);
    throw new Error(`Fake Herdr ${mode} did not start: ${output}`);
  };
  const fakeEnv = { ...process.env, FAKE_HERDR_SCENARIO: scenarioPath, FAKE_HERDR_BARRIER_DIR: barrierDir };
  const controlServer = await startFakeHerdr('control-server', { ...fakeEnv, FAKE_HERDR_CONTROL: controlPath }, controlPath, 'fixture-control');
  let socketServer: ChildProcess;
  try {
    socketServer = await startFakeHerdr('socket-server', {
      ...fakeEnv,
      FAKE_HERDR_SOCKET: socketPath,
      FAKE_HERDR_PANE_OVERRIDES: metadataPath,
      FAKE_HERDR_INPUT_LOG: inputOperationsPath,
    }, socketPath, 'fixture-socket');
  } catch (error) {
    await stopRelay(controlServer);
    throw error;
  }

  const [port, pluginPort] = await Promise.all([freePort(), freePort()]);
  let output = '';
  const relay = spawn(relayBin, [], {
    cwd: repoRoot,
    env: {
      ...process.env,
      HERDR_RELAY_PORT: String(port),
      HERDR_RELAY_PLUGIN_PORT: String(pluginPort),
      HERDR_RELAY_HOST: '127.0.0.1',
      HERDR_RELAY_TOKEN: 'attention-browser-test-key-32byt',
      HERDR_RELAY_INSTANCE_ID: 'attention-browser-test',
      HERDR_RELAY_POLL_INTERVAL: '0.2',
      HERDR_BIN: fakeBin,
      HERDR_WEB_ROOT: webRoot,
      HERDR_CLAUDE_CONFIG_DIRS: conversationRoot,
      HERDR_SOCKET_PATH: socketPath,
      FAKE_HERDR_SCENARIO: scenarioPath,
      FAKE_HERDR_OPERATIONS: operationsPath,
      FAKE_HERDR_BARRIER_DIR: barrierDir,
      HOME: runtime,
      PI_CODING_AGENT_DIR: join(runtime, 'pi'),
      XDG_CONFIG_HOME: join(runtime, 'config'),
      XDG_CACHE_HOME: join(runtime, 'cache'),
      XDG_DATA_HOME: join(runtime, 'data'),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (process.env.HERDR_ATTENTION_PROCESS_LOG) await appendFile(process.env.HERDR_ATTENTION_PROCESS_LOG, JSON.stringify({ kind: 'fixture-relay', pid: relay.pid, bin: relayBin, runtime }) + '\n', { mode: 0o600 });
  relay.stdout?.on('data', (chunk) => { output += String(chunk); });
  relay.stderr?.on('data', (chunk) => { output += String(chunk); });

  try {
    await waitForHealth(`http://127.0.0.1:${port}`, () => output);
  } catch (error) {
    await stopRelay(relay);
    await stopRelay(controlServer);
    await stopRelay(socketServer);
    await rm(runtime, { recursive: true, force: true });
    throw error;
  }

  process.env.HERDR_ATTENTION_WS_URL = `ws://127.0.0.1:${port}`;
  process.env.HERDR_ATTENTION_OPERATIONS = operationsPath;
  process.env.HERDR_ATTENTION_INPUT_OPERATIONS = inputOperationsPath;
  process.env.HERDR_ATTENTION_CREDENTIALS = join(runtime, 'browser-credential.json');
  process.env.HERDR_ATTENTION_METADATA = metadataPath;
  process.env.HERDR_ATTENTION_CONTROL = controlPath;
  process.env.HERDR_ATTENTION_BARRIERS = barrierDir;

  return async () => {
    await stopRelay(relay);
    await stopRelay(controlServer);
    await stopRelay(socketServer);
    await rm(runtime, { recursive: true, force: true });
  };
}
