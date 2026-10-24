import { test, expect } from 'bun:test';
import { createConnection } from 'node:net';
import { rm } from 'node:fs/promises';
import { createPromptIngress, validPromptText, startBridge } from '../relay/pi-command-bridge/bridge.mjs';

function fixture(options = {}) {
  let clock = 1000;
  let session = { id: 'session', file: '/synthetic/session.jsonl' };
  const calls = [];
  const identity = { instance: 'a'.repeat(64), pane: 'pane', session: 'session', pid: process.pid };
  const incarnation = options.incarnation ?? 'b'.repeat(36);
  const api = { sendUserMessage(...args) { calls.push(args); return options.invoke?.(...args); } };
  const ingress = createPromptIngress({ pi: api, identity, sessionFile: session.file, incarnation, currentSession: () => session, now: () => clock, limit: options.limit ?? 4096 });
  const base = { ...identity, challenge: 'c'.repeat(32), connection: 'connection', target_key: 'exact-target' };
  const capability = ingress({ ...base, operation: 'prompt_capability' });
  const request = { ...base, operation: 'send_prompt_unconfirmed', incarnation, token: capability.token, request_id: `${incarnation}_req-1`, text: '/orchestrate-answer synthetic guidance' };
  return { ingress, calls, request, capability, api, identity, sessionFile: session.file, setSession(value) { session = value; }, advance() { clock += 20000; } };
}

test.each(['idle', 'streaming'])('expands extension commands with race-safe followUp delivery when %s', state => {
  const f = fixture();
  f.api.state = state;
  expect(f.ingress(f.request)).toMatchObject({ status: 'unconfirmed', sent_at: 1000 });
  expect(f.calls).toEqual([[f.request.text, { expandPromptTemplates: true, deliverAs: 'followUp' }]]);
});

test('does not mistake a pending API return for an admission receipt', () => {
  const f = fixture({ invoke() { return new Promise(() => {}); } });
  expect(f.ingress(f.request).status).toBe('unconfirmed');
  expect(f.calls).toHaveLength(1);
});

test('duplicates and reconnect retries never invoke twice, including after errors', () => {
  const f = fixture({ invoke() { throw new Error('Synthetic runtime failure'); } });
  expect(f.ingress(f.request).status).toBe('unconfirmed');
  for (let n = 0; n < 5; n++) expect(f.ingress({ ...f.request, challenge: String(n).repeat(32) }).status).toBe('unconfirmed');
  expect(f.ingress({ ...f.request, text: 'different' }).status).toBe('not_started');
  expect(f.ingress({ ...f.request, connection: 'new-connection' }).status).toBe('not_started');
  expect(f.calls).toHaveLength(1);
  const reloaded = fixture({ incarnation: 'd'.repeat(36) });
  expect(reloaded.ingress({ ...reloaded.request, request_id: f.request.request_id }).status).toBe('not_started');
  expect(reloaded.calls).toHaveLength(0);
  f.advance();
  expect(f.ingress(f.request).status).toBe('unconfirmed');
  expect(f.calls).toHaveLength(1);
});

test('rejects stale session, pane, process, instance, incarnation, lease and connection identity', () => {
  for (const field of ['instance', 'pane', 'session', 'pid', 'incarnation', 'token', 'connection', 'target_key']) {
    const f = fixture();
    expect(f.ingress({ ...f.request, [field]: field === 'pid' ? process.pid + 1 : 'wrong' }).status).toBe('not_started');
    expect(f.calls).toHaveLength(0);
  }
  const f = fixture();
  f.setSession({ id: 'replacement', file: f.sessionFile });
  expect(f.ingress(f.request).status).toBe('not_started');
  expect(f.calls).toHaveLength(0);
});

test('accepts exact session-file alias but rejects drift and expired or exhausted admission', () => {
  const f = fixture({ limit: 1 });
  f.advance();
  expect(f.ingress(f.request).status).toBe('not_started');
  expect(f.ingress({ ...f.request, operation: 'prompt_capability', session: f.sessionFile }).status).toBe('not_started');
  const fresh = fixture({ limit: 1 });
  expect(fresh.ingress(fresh.request).status).toBe('unconfirmed');
  expect(fresh.ingress({ ...fresh.request, request_id: `${fresh.request.incarnation}_req-2` }).status).toBe('not_started');
  expect(fresh.calls).toHaveLength(1);
  const alias = fixture();
  const cap = alias.ingress({ ...alias.request, operation: 'prompt_capability', session: alias.sessionFile });
  expect(alias.ingress({ ...alias.request, session: alias.sessionFile, token: cap.token }).status).toBe('unconfirmed');
  alias.setSession({ id: 'session', file: 'changed' });
  expect(alias.ingress(alias.request).status).toBe('not_started');
});

test('excludes attachments, secret-shaped fields, control bytes and oversized text', () => {
  for (const text of ['', ' ', '\x1b[A', 'x'.repeat(100001), '\ud800', 'Attachment: ref', '[attachment:ref]']) expect(validPromptText(text)).toBe(false);
  expect(validPromptText('界\nnormal\ttext')).toBe(true);
  for (const field of ['secret', 'no_echo', 'attachments', 'images']) {
    const f = fixture();
    expect(f.ingress({ ...f.request, [field]: true }).status).toBe('not_started');
    expect(f.calls).toHaveLength(0);
  }
});

test('existing private catalog socket carries capability and one mutation without replay', async () => {
  const directory = '.d-prompt-ipc';
  const f = fixture();
  const close = await startBridge({ ...f.identity, sessionFile: f.sessionFile, directory,
    catalog: async () => ({ commands: [], status: 'available' }), prompt: { pi: f.api, currentSession: () => ({ id: 'session', file: f.sessionFile }) } });
  const exchange = request => new Promise((resolve, reject) => {
    const socket = createConnection(`${directory}/${process.pid}.sock`);
    let data = '';
    socket.on('connect', () => socket.end(JSON.stringify(request) + '\n'));
    socket.on('data', chunk => { data += chunk; });
    socket.on('error', reject);
    socket.on('end', () => { try { resolve(JSON.parse(data)); } catch(error) { reject(error); } });
  });
  try {
    const cap = await exchange({ ...f.request, operation: 'prompt_capability' });
    const request = { ...f.request, request_id: `${cap.incarnation}_req-1`, text: '界'.repeat(1000), token: cap.token, incarnation: cap.incarnation };
    expect((await exchange(request)).status).toBe('unconfirmed');
    expect((await exchange(request)).status).toBe('unconfirmed');
    expect(f.calls).toHaveLength(1);
  } finally { await close(); await rm(directory, { recursive: true, force: true }); }
});
