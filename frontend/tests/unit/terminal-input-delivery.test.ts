import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { TerminalInputDelivery, terminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
import type { Agent } from '$lib/types';

let now = 10;
let sequence = 0;
const agent = (): Agent => { const pane = `pane-${++sequence}`; return { relay_id: 'delivery', relay_label: 'Delivery', raw_pane_id: pane, pane_id: `delivery::${pane}`, server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent: 'pi' }; };

beforeEach(() => { now = 10; vi.spyOn(performance, 'now').mockImplementation(() => now); });
afterEach(() => vi.restoreAllMocks());

it('permits one dispatch and requires a read started strictly after uncertainty', () => {
  const delivery = new TerminalInputDelivery();
  expect(delivery.begin()).toBe(true);
  expect(delivery.begin()).toBe(false);
  delivery.finish(true);
  expect(delivery.uncertain).toBe(true);
  expect(delivery.begin()).toBe(false);
  for (const timestamp of [undefined, NaN, 9, 10]) expect(delivery.inspect(timestamp)).toBe(false);
  now = 11;
  expect(delivery.inspect(now)).toBe(true);
  expect(delivery.uncertain).toBe(false);
  expect(delivery.begin()).toBe(true);
  expect(delivery.inspect(12)).toBe(false);
  delivery.finish(false);
});

it('retains pending and late uncertainty while no view is mounted', () => {
  const target = agent();
  const original = terminalInputDelivery(target);
  original.retain();
  original.begin();
  original.release();
  expect(terminalInputDelivery({ ...target })).toBe(original);
  original.finish(true);
  const restored = terminalInputDelivery({ ...target, status: 'idle' });
  expect(restored).toBe(original);
  expect(restored.uncertain).toBe(true);
  restored.retain();
  now += 1;
  expect(restored.inspect(now)).toBe(true);
  restored.release();
  expect(terminalInputDelivery(target)).not.toBe(original);
});

it.each(['cwd', 'agent', 'project', 'pane_id'] as const)('retains pending and uncertain state across same-target %s changes', field => {
  const target = agent();
  const original = terminalInputDelivery(target);
  original.begin();
  const changed = { ...target, [field]: 'changed-metadata' };
  expect(terminalInputDelivery(changed)).toBe(original);
  expect(terminalInputDelivery(changed).pending).toBe(true);
  original.finish(true);
  expect(terminalInputDelivery(changed).uncertain).toBe(true);
  expect(original.begin()).toBe(false);
  now += 1;
  expect(original.inspect(now)).toBe(true);
});

it('retains an unresolved exact target when switching away and back', () => {
  const target = agent();
  const original = terminalInputDelivery(target);
  original.begin();
  const replacement = { ...target, generation: 2 };
  expect(terminalInputDelivery(replacement)).not.toBe(original);
  original.finish(true);
  expect(terminalInputDelivery(target)).toBe(original);
  expect(terminalInputDelivery(target).uncertain).toBe(true);
  now += 1;
  original.inspect(now);
});

it('removes idle confirmed state after the last view releases it', () => {
  const target = agent();
  const delivery = terminalInputDelivery(target);
  delivery.retain();
  delivery.begin();
  delivery.finish(false);
  expect(terminalInputDelivery(target)).toBe(delivery);
  delivery.release();
  expect(terminalInputDelivery(target)).not.toBe(delivery);
});

it('keeps multiple view leases until every view releases confirmed state', () => {
  const target = agent();
  const delivery = terminalInputDelivery(target);
  delivery.retain();
  delivery.retain();
  delivery.release();
  expect(terminalInputDelivery(target)).toBe(delivery);
  delivery.release();
  expect(terminalInputDelivery(target)).not.toBe(delivery);
});

it.each(['generation', 'terminal_id', 'agent_session_id', 'server_session_id', 'relay_id', 'raw_pane_id'] as const)('isolates %s replacement and ignores old completion cleanup', field => {
  const target = agent();
  const original = terminalInputDelivery(target);
  original.retain();
  original.begin();
  original.release();
  const replacement = { ...target, [field]: field === 'generation' ? 2 : 'replacement' };
  const current = terminalInputDelivery(replacement);
  expect(current).not.toBe(original);
  current.retain();
  current.begin();
  current.finish(true);
  original.finish(false);
  expect(terminalInputDelivery(replacement)).toBe(current);
  expect(current.uncertain).toBe(true);
  now += 1;
  current.inspect(now);
  current.release();
});
