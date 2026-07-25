import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createServer } from 'node:net';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

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

export default async function setup() {
  const runtime = await mkdtemp(join(tmpdir(), 'herdr-attention-browser-'));
  const cache = join(runtime, 'go-cache');
  const fakeBin = join(runtime, 'fake-herdr');
  const relayBin = join(runtime, 'herdr-mobile-relay');
  const scenarioPath = join(runtime, 'scenario.json');
  const operationsPath = join(runtime, 'operations.jsonl');
  const webRoot = join(runtime, 'web');
  await Promise.all([mkdir(cache), mkdir(webRoot)]);
  await writeFile(join(webRoot, 'index.html'), '<html>test</html>\n');

  const [
    approval,
    qoderNotes,
    codexNotes,
    codexFinalQuestion,
    codexSingleQuestion,
    codexPlanApproval,
    claudeApproval,
    claudeLaterQuestion,
    claudeCustomAnswer,
    claudeMultiSelect,
    claudeReview,
    openCodeSingle,
    openCodeCustom,
    openCodeReview,
    qoderStandalone,
    qoderSettings,
  ] = await Promise.all([
    readFile(join(captureRoot, 'qodercli-permission-required2.ansi'), 'utf8'),
    readFile(join(captureRoot, 'qodercli-multi-questions-and-notes.ansi'), 'utf8'),
    readFile(join(captureRoot, 'codex-first-question_with_notes.ansi'), 'utf8'),
    readFile(join(captureRoot, 'codex-middle-question.ansi'), 'utf8'),
    readFile(join(captureRoot, 'codex-single-question.ansi'), 'utf8'),
    readFile(join(captureRoot, 'codex-implement-plan.ansi'), 'utf8'),
    readFile(join(captureRoot, 'claude-single-approval.ansi'), 'utf8'),
    readFile(join(captureRoot, 'claude-plan-multi-question.ansi'), 'utf8'),
    readFile(join(captureRoot, 'claude-plan-one-question-notes.ansi'), 'utf8'),
    readFile(join(captureRoot, 'claude-multi-select-with-free-text.ansi'), 'utf8'),
    readFile(join(captureRoot, 'claude-plan-submit-answers.ansi'), 'utf8'),
    readFile(join(captureRoot, 'opencode-single-question.ansi'), 'utf8'),
    readFile(join(captureRoot, 'opencode-questions-multiple-choice-with-free-text-edited.ansi'), 'utf8'),
    readFile(join(captureRoot, 'opencode-questions-with-multiple-choice-answers-confirm.ansi'), 'utf8'),
    readFile(join(captureRoot, 'qodercli-single-question.ansi'), 'utf8'),
    readFile(join(captureRoot, 'qodercli-yes-no.ansi'), 'utf8'),
  ]);
  await writeFile(scenarioPath, JSON.stringify({
    panes: [
      {
        pane_id: 'qoder-approval', agent: 'qodercli', name: 'qoder-approval',
        agent_status: 'blocked', tab_id: 'tab-1', workspace_id: 'workspace-1',
        cwd: '/tmp/qoder-approval', revision: 1,
      },
      {
        pane_id: 'qoder-notes', agent: 'qodercli', name: 'qoder-notes',
        agent_status: 'blocked', tab_id: 'tab-2', workspace_id: 'workspace-1',
        cwd: '/tmp/qoder-notes', revision: 1,
      },
      {
        pane_id: 'codex-notes', agent: 'codex', name: 'codex-notes',
        agent_status: 'blocked', tab_id: 'tab-3', workspace_id: 'workspace-1',
        cwd: '/tmp/codex-notes', revision: 1,
      },
      {
        pane_id: 'codex-final-question', agent: 'codex', name: 'codex-final-question',
        agent_status: 'blocked', tab_id: 'attention-d', workspace_id: 'workspace-1',
        cwd: '/tmp/codex-final-question', revision: 1,
      },
      {
        pane_id: 'codex-single-question', agent: 'codex', name: 'codex-single-question',
        agent_status: 'blocked', tab_id: 'attention-e', workspace_id: 'workspace-1',
        cwd: '/tmp/codex-single-question', revision: 1,
      },
      {
        pane_id: 'codex-plan-approval', agent: 'codex', name: 'codex-plan-approval',
        agent_status: 'blocked', tab_id: 'attention-f', workspace_id: 'workspace-1',
        cwd: '/tmp/codex-plan-approval', revision: 1,
      },
      {
        pane_id: 'claude-approval', agent: 'claude', name: 'claude-approval',
        agent_status: 'blocked', tab_id: 'attention-g', workspace_id: 'workspace-1',
        cwd: '/tmp/claude-approval', revision: 1,
      },
      {
        pane_id: 'claude-later-question', agent: 'claude', name: 'claude-later-question',
        agent_status: 'blocked', tab_id: 'attention-h', workspace_id: 'workspace-1',
        cwd: '/tmp/claude-later-question', revision: 1,
      },
      {
        pane_id: 'claude-custom-answer', agent: 'claude', name: 'claude-custom-answer',
        agent_status: 'blocked', tab_id: 'attention-i', workspace_id: 'workspace-1',
        cwd: '/tmp/claude-custom-answer', revision: 1,
      },
      {
        pane_id: 'claude-multi-select', agent: 'claude', name: 'claude-multi-select',
        agent_status: 'blocked', tab_id: 'attention-j', workspace_id: 'workspace-1',
        cwd: '/tmp/claude-multi-select', revision: 1,
      },
      {
        pane_id: 'claude-review', agent: 'claude', name: 'claude-review',
        agent_status: 'blocked', tab_id: 'attention-k', workspace_id: 'workspace-1',
        cwd: '/tmp/claude-review', revision: 1,
      },
      {
        pane_id: 'opencode-single', agent: 'opencode', name: 'opencode-single',
        agent_status: 'blocked', tab_id: 'attention-l', workspace_id: 'workspace-1',
        cwd: '/tmp/opencode-single', revision: 1,
      },
      {
        pane_id: 'opencode-custom', agent: 'opencode', name: 'opencode-custom',
        agent_status: 'blocked', tab_id: 'attention-m', workspace_id: 'workspace-1',
        cwd: '/tmp/opencode-custom', revision: 1,
      },
      {
        pane_id: 'opencode-review', agent: 'opencode', name: 'opencode-review',
        agent_status: 'blocked', tab_id: 'attention-n', workspace_id: 'workspace-1',
        cwd: '/tmp/opencode-review', revision: 1,
      },
      {
        pane_id: 'qoder-standalone', agent: 'qodercli', name: 'qoder-standalone',
        agent_status: 'blocked', tab_id: 'attention-o', workspace_id: 'workspace-1',
        cwd: '/tmp/qoder-standalone', revision: 1,
      },
      {
        pane_id: 'qoder-settings', agent: 'qodercli', name: 'qoder-settings',
        agent_status: 'blocked', tab_id: 'attention-p', workspace_id: 'workspace-1',
        cwd: '/tmp/qoder-settings', revision: 1,
      },
    ],
    tabs: [
      { tab_id: 'tab-1', workspace_id: 'workspace-1', label: 'qoder-approval', number: 1, cwd: '/tmp/qoder-approval' },
      { tab_id: 'tab-2', workspace_id: 'workspace-1', label: 'qoder-notes', number: 2, cwd: '/tmp/qoder-notes' },
      { tab_id: 'tab-3', workspace_id: 'workspace-1', label: 'codex-notes', number: 3, cwd: '/tmp/codex-notes' },
      { tab_id: 'attention-d', workspace_id: 'workspace-1', label: 'codex-final-question', number: 4, cwd: '/tmp/codex-final-question' },
      { tab_id: 'attention-e', workspace_id: 'workspace-1', label: 'codex-single-question', number: 5, cwd: '/tmp/codex-single-question' },
      { tab_id: 'attention-f', workspace_id: 'workspace-1', label: 'codex-plan-approval', number: 6, cwd: '/tmp/codex-plan-approval' },
      { tab_id: 'attention-g', workspace_id: 'workspace-1', label: 'claude-approval', number: 7, cwd: '/tmp/claude-approval' },
      { tab_id: 'attention-h', workspace_id: 'workspace-1', label: 'claude-later-question', number: 8, cwd: '/tmp/claude-later-question' },
      { tab_id: 'attention-i', workspace_id: 'workspace-1', label: 'claude-custom-answer', number: 9, cwd: '/tmp/claude-custom-answer' },
      { tab_id: 'attention-j', workspace_id: 'workspace-1', label: 'claude-multi-select', number: 10, cwd: '/tmp/claude-multi-select' },
      { tab_id: 'attention-k', workspace_id: 'workspace-1', label: 'claude-review', number: 11, cwd: '/tmp/claude-review' },
      { tab_id: 'attention-l', workspace_id: 'workspace-1', label: 'opencode-single', number: 12, cwd: '/tmp/opencode-single' },
      { tab_id: 'attention-m', workspace_id: 'workspace-1', label: 'opencode-custom', number: 13, cwd: '/tmp/opencode-custom' },
      { tab_id: 'attention-n', workspace_id: 'workspace-1', label: 'opencode-review', number: 14, cwd: '/tmp/opencode-review' },
      { tab_id: 'attention-o', workspace_id: 'workspace-1', label: 'qoder-standalone', number: 15, cwd: '/tmp/qoder-standalone' },
      { tab_id: 'attention-p', workspace_id: 'workspace-1', label: 'qoder-settings', number: 16, cwd: '/tmp/qoder-settings' },
    ],
    content: {
      'qoder-approval': approval,
      'qoder-notes': qoderNotes,
      'codex-notes': codexNotes,
      'codex-final-question': codexFinalQuestion,
      'codex-single-question': codexSingleQuestion,
      'codex-plan-approval': codexPlanApproval,
      'claude-approval': claudeApproval,
      'claude-later-question': claudeLaterQuestion,
      'claude-custom-answer': claudeCustomAnswer,
      'claude-multi-select': claudeMultiSelect,
      'claude-review': claudeReview,
      'opencode-single': openCodeSingle,
      'opencode-custom': openCodeCustom,
      'opencode-review': openCodeReview,
      'qoder-standalone': qoderStandalone,
      'qoder-settings': qoderSettings,
    },
  }));

  build(fakeBin, './cmd/fake-herdr', cache);
  build(relayBin, './cmd/herdr-mobile-relay', cache);

  const [port, pluginPort] = await Promise.all([freePort(), freePort()]);
  let output = '';
  const relay = spawn(relayBin, [], {
    cwd: repoRoot,
    env: {
      ...process.env,
      HERDR_RELAY_PORT: String(port),
      HERDR_RELAY_PLUGIN_PORT: String(pluginPort),
      HERDR_RELAY_HOST: '127.0.0.1',
      HERDR_RELAY_TOKEN: 'attention-test-token',
      HERDR_RELAY_INSTANCE_ID: 'attention-browser-test',
      HERDR_RELAY_POLL_INTERVAL: '0.2',
      HERDR_BIN: fakeBin,
      HERDR_WEB_ROOT: webRoot,
      HERDR_SOCKET_PATH: join(runtime, 'herdr.sock'),
      FAKE_HERDR_SCENARIO: scenarioPath,
      FAKE_HERDR_OPERATIONS: operationsPath,
      XDG_CONFIG_HOME: join(runtime, 'config'),
      XDG_CACHE_HOME: join(runtime, 'cache'),
      XDG_DATA_HOME: join(runtime, 'data'),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  relay.stdout?.on('data', (chunk) => { output += String(chunk); });
  relay.stderr?.on('data', (chunk) => { output += String(chunk); });

  try {
    await waitForHealth(`http://127.0.0.1:${port}`, () => output);
  } catch (error) {
    await stopRelay(relay);
    await rm(runtime, { recursive: true, force: true });
    throw error;
  }

  process.env.HERDR_ATTENTION_WS_URL = `ws://127.0.0.1:${port}/ws`;
  process.env.HERDR_ATTENTION_OPERATIONS = operationsPath;

  return async () => {
    await stopRelay(relay);
    await rm(runtime, { recursive: true, force: true });
  };
}
