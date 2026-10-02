import { describe, expect, it } from 'vitest';
import { InventoryFreshness, type InventoryBinding } from '$lib/inventory-freshness';

function fixture() {
  let now = 1_000;
  let sequence = 0;
  const parse = (value: unknown): readonly string[] | null => Array.isArray(value)
    && value.every((row) => typeof row === 'string') ? value : null;
  const freshness = new InventoryFreshness(parse, parse, () => now,
    () => `${String(sequence++).padStart(21, 'A')}A`);
  const binding: InventoryBinding = { connection: {}, path: {}, wake: 1 };
  const response = (request: Record<string, unknown>, agents: unknown = [], workspaces: unknown = []) => ({
    type: 'inventory_snapshot', snapshot_request_id: request.snapshot_request_id,
    inventory: { state: 'ready', stale: false }, agents, workspaces,
  });
  return { freshness, binding, response, advance: (ms: number) => { now += ms; } };
}

describe('correlated inventory freshness independent of persistence/metrics', () => {
  it('unsupported peers and uncorrelated legacy frames cannot restore authority', () => {
    const { freshness, binding } = fixture();
    expect(freshness.request(binding, false)).toBeNull();
    expect(freshness.current(binding)).toBeNull();
    expect(freshness.request(binding, true)).not.toBeNull();
    for (const message of [
      { type: 'agents', agents: [] }, { type: 'push_config', inventory: { state: 'ready' } },
      { type: 'workspaces', workspaces: [] }, { type: 'activity' }, { type: 'ping' },
    ]) expect(freshness.accept(binding, message)).toBe(false);
    expect(freshness.current(binding)).toBeNull();
  });

  it('accepts a genuinely empty matching snapshot exactly once', () => {
    const { freshness, binding, response } = fixture();
    const request = freshness.request(binding, true)!;
    expect(freshness.accept(binding, response(request))).toBe(true);
    expect(freshness.current(binding)).toEqual({ agents: [], workspaces: [] });
    expect(freshness.accept(binding, response(request, ['old']))).toBe(false);
    expect(freshness.current(binding)?.agents).toEqual([]);
  });

  it.each(['connection', 'path', 'wake'] as const)('does not promote a replaced %s', (field) => {
    const { freshness, binding, response } = fixture();
    const request = freshness.request(binding, true)!;
    const replaced = { ...binding, [field]: field === 'wake' ? 2 : {} };
    expect(freshness.accept(replaced, response(request))).toBe(false);
    expect(freshness.current(replaced)).toBeNull();
    freshness.invalidate();
    expect(freshness.accept(binding, response(request))).toBe(false);
  });

  it('an earlier request cannot authorize or revoke a newer generation', () => {
    const { freshness, binding, response } = fixture();
    const old = freshness.request(binding, true)!;
    const current = freshness.request(binding, true)!;
    expect(freshness.accept(binding, response(old, ['old']))).toBe(false);
    expect(freshness.accept(binding, response(current, ['current']))).toBe(true);
    expect(freshness.accept(binding, { ...response(old), inventory: { state: 'error', stale: true } })).toBe(false);
    expect(freshness.current(binding)?.agents).toEqual(['current']);
  });

  it('tracks workspace validation separately instead of inheriting agent freshness', () => {
    const { freshness, binding, response } = fixture();
    const request = freshness.request(binding, true)!;
    expect(freshness.accept(binding, response(request, ['agent'], ['workspace', 7]))).toBe(true);
    expect(freshness.current(binding)).toEqual({ agents: ['agent'], workspaces: null });
  });

  it.each([
    { state: 'ready', stale: true }, { state: 'error', stale: false }, { state: 'starting', stale: false },
  ])('rejects unready or stale status %j', (inventory) => {
    const { freshness, binding, response } = fixture();
    const request = freshness.request(binding, true)!;
    expect(freshness.accept(binding, { ...response(request), inventory })).toBe(false);
    expect(freshness.current(binding)).toBeNull();
  });

  it('expires pending requests and rejects clock rollback', () => {
    const { freshness, binding, response, advance } = fixture();
    const request = freshness.request(binding, true)!;
    advance(15_000);
    expect(freshness.accept(binding, response(request))).toBe(false);
    const next = freshness.request(binding, true)!;
    expect(freshness.accept(binding, response(next))).toBe(true);
    advance(-1);
    expect(freshness.current(binding)).toBeNull();
  });

  it('keeps separate relay authorities isolated', () => {
    const first = fixture();
    const second = fixture();
    const request = first.freshness.request(first.binding, true)!;
    second.freshness.request(second.binding, true);
    expect(second.freshness.accept(first.binding, first.response(request))).toBe(false);
    first.freshness.accept(first.binding, first.response(request));
    expect(first.freshness.current(first.binding)).not.toBeNull();
    expect(second.freshness.current(second.binding)).toBeNull();
  });
});
