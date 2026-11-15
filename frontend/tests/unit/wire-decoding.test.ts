import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { defaultDevicePushPolicy } from '$lib/push-policy';
import { relayStore } from '$lib/store';
import { MockWebSocket, connectRelay, makeAgent } from '../support/relay';

let socket: MockWebSocket;
let relayId: string;

beforeEach(() => {
  MockWebSocket.instances = [];
  vi.stubGlobal('WebSocket', MockWebSocket);
  localStorage.clear();
  sessionStorage.clear();
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  relayStore.addRelay({ label: 'Owner', url: 'ws://127.0.0.1:8871', token: '' });
  relayId = get(relayStore.relayConfigs)[0].id;
  socket = MockWebSocket.instances.at(-1)!;
});

afterEach(() => {
  relayStore.destroy();
  relayStore.relayConfigs.set([]);
  vi.unstubAllGlobals();
});

const status = () => relayStore.connection(relayId)!.herdrStatus;

describe('plan version in a conversation history reply', () => {
  const entries = [
    { id: 'entry-1', timestamp: '2026-01-01T00:00:00Z', role: 'user', text: 'first' },
    { id: 'entry-2', timestamp: '2026-01-01T00:00:01Z', role: 'assistant', text: 'second' },
  ];

  async function planVersion(version: unknown) {
    connectRelay({}, socket);
    const history = relayStore.getConversationHistory(makeAgent({ relay_id: relayId }));
    const request = socket.messages().find(frame => frame.type === 'get_conversation_history')!;
    socket.message({
      type: 'command_result',
      request_id: request.request_id,
      ok: true,
      phase: 'completed',
      data: {
        available: true, has_more: false, state: 'ready', mode: 'recent', entries,
        omo_plan: { available: true, version, phases: [] },
      },
    });
    return history;
  }

  it.each([
    ['an integer', 7, 7],
    ['the largest safe integer', Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER],
    ['a fraction', 1.5, undefined],
    ['a number above the safe range', Number.MAX_SAFE_INTEGER + 1, undefined],
    ['a word', 'seven', undefined],
  ])('decodes %s without dropping the entries', async (_name, version, expected) => {
    const page = await planVersion(version);
    expect(page.omoPlan?.version).toBe(expected);
    expect(page.entries.map(entry => entry.id)).toEqual(['entry-1', 'entry-2']);
  });
});

describe('relay status numbers', () => {
  it.each([
    ['an integer', 4, 4],
    ['the largest safe integer', Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER],
    ['a fraction', 1.5, 0],
    ['a number above the safe range', Number.MAX_SAFE_INTEGER + 1, 0],
    ['a word', 'four', 0],
  ])('decodes %s from the connection status', (_name, value, expected) => {
    connectRelay({ herdr_status: { server_protocol: value, generation: value } }, socket);
    expect(status()).toMatchObject({ server_protocol: expected, generation: expected });
  });

  it.each([
    ['an integer', 4, 4],
    ['a fraction', 1.5, 0],
    ['a number above the safe range', Number.MAX_SAFE_INTEGER + 1, 0],
    ['a word', 'four', 0],
  ])('decodes %s from a live status', (_name, value, expected) => {
    connectRelay({ herdr_status: { generation: 0 } }, socket);
    socket.message({ type: 'herdr_status', status: { server_protocol: value, generation: 1 } });
    expect(status()).toMatchObject({ server_protocol: expected, generation: 1 });
  });

  it('ignores a live status whose generation is equal or lower', () => {
    connectRelay({ herdr_status: { server_version: 'connected', generation: 4 } }, socket);
    socket.message({ type: 'herdr_status', status: { server_version: 'equal', generation: 4 } });
    socket.message({ type: 'herdr_status', status: { server_version: 'lower', generation: 3 } });
    socket.message({ type: 'herdr_status', status: { server_version: 'word', generation: 'newer' } });
    expect(status()).toMatchObject({ server_version: 'connected', generation: 4 });
    socket.message({ type: 'herdr_status', status: { server_version: 'newer', generation: 5 } });
    expect(status()).toMatchObject({ server_version: 'newer', generation: 5 });
  });
});

describe('push policy frames', () => {
  const defaults = defaultDevicePushPolicy('device-1');

  function policyFrom(policy: Record<string, unknown>) {
    socket.message({ type: 'push_policy', policy: { device_id: 'device-1', ...policy } });
    return get(relayStore.pushPolicies).get(relayId);
  }

  beforeEach(() => connectRelay({}, socket));

  it('decodes a complete policy', () => {
    expect(policyFrom({
      locale: 'fr',
      categories: { attention: false, finished: true },
      settle_ms: 500,
      cooldown_ms: 0,
      snoozed: true,
      snooze_until: '2026-01-01T00:00:00Z',
      update_once: false,
    })).toEqual({
      device_id: 'device-1',
      locale: 'fr',
      categories: { ...defaults.categories, attention: false, finished: true },
      settle_ms: 500,
      cooldown_ms: 0,
      snoozed: true,
      snooze_until: '2026-01-01T00:00:00Z',
      update_once: false,
    });
  });

  it('drops categories the app does not know', () => {
    expect(policyFrom({ categories: { attention: false, unknown: true } })?.categories)
      .toEqual({ ...defaults.categories, attention: false });
  });

  it('keeps the default of a category that is not a boolean', () => {
    expect(policyFrom({ categories: { attention: 'no', question: 0, brief: null } })?.categories)
      .toEqual(defaults.categories);
  });

  it.each([-1, 1.5, 'soon'])('keeps the default timing for %j', (value) => {
    expect(policyFrom({ settle_ms: value, cooldown_ms: value }))
      .toMatchObject({ settle_ms: defaults.settle_ms, cooldown_ms: defaults.cooldown_ms });
  });

  it('drops a snooze_until that is not a date', () => {
    expect(policyFrom({ snoozed: true, snooze_until: 'tomorrow' })).not.toHaveProperty('snooze_until');
  });

  it.each([[undefined, true], [true, true], [0, true], [false, false]])('reads update_once %j as %j', (value, expected) => {
    expect(policyFrom({ update_once: value })?.update_once).toBe(expected);
  });

  it('leaves the stored policy alone when the frame has no device_id', () => {
    const stored = policyFrom({ settle_ms: 500 });
    socket.message({ type: 'push_policy', policy: { settle_ms: 900 } });
    socket.message({ type: 'push_policy', policy: { device_id: '  ', settle_ms: 900 } });
    expect(get(relayStore.pushPolicies).get(relayId)).toBe(stored);
    expect(get(relayStore.pushPolicies).get(relayId)?.settle_ms).toBe(500);
  });
});
