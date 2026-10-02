import { get } from 'svelte/store';
import { describe, expect, it, vi } from 'vitest';
import { deferredModule } from '$lib/deferred-module';

function pending() {
  let resolve!: (value: { component: string }) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<{ component: string }>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const settle = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };

describe('on-demand workspace module loading', () => {
  it('does not preload, deduplicates overlapping requests, and mounts only the requested surface', async () => {
    const gate = pending(); const load = vi.fn(() => gate.promise);
    const tools = deferredModule(load);
    expect(load).not.toHaveBeenCalled();
    tools.select('workspaces', 'scope-a'); await settle();
    tools.select('launch', 'scope-b'); await settle();
    expect(load).toHaveBeenCalledOnce();
    gate.resolve({ component: 'loaded' }); await settle();
    expect(get(tools).status).toBe('ready');
    tools.select(null, null);
    expect(get(tools).status).toBe('idle');
    tools.select('manage', 'scope-c');
    expect(get(tools).status).toBe('ready');
    expect(load).toHaveBeenCalledOnce();
  });
  it('reports failure without automatic retries or queued commands, and retries only on request', async () => {
    const gate = pending(); const load = vi.fn(() => gate.promise);
    const tools = deferredModule(load);
    tools.select('inspect', 'scope-a'); await settle();
    gate.reject(new Error('chunk unavailable')); await settle();
    expect(get(tools).status).toBe('failed');
    tools.select('inspect', 'scope-a'); await settle();
    expect(load).toHaveBeenCalledOnce();
    load.mockResolvedValue({ component: 'loaded' });
    tools.retry('scope-a'); await settle();
    expect(get(tools).status).toBe('ready');
    expect(load).toHaveBeenCalledTimes(2);
  });
  it.each(['cancel', 'navigation', 'lock', 'target', 'role', 'connection'])(
    'does not revive a pending intent after %s', async (change) => {
      const gate = pending(); const tools = deferredModule(() => gate.promise);
      tools.select('inspect', 'scope-a'); await settle();
      if (change === 'cancel' || change === 'navigation') tools.select(null, null);
      else tools.select('inspect', change === 'lock' ? null : `changed-${change}`);
      gate.resolve({ component: 'loaded' }); await settle();
      expect(get(tools).status).not.toBe('ready');
      if (change === 'lock') { tools.select('inspect', 'scope-a'); expect(get(tools).status).toBe('changed'); }
      if (change !== 'cancel' && change !== 'navigation') {
        tools.retry('scope-current'); expect(get(tools).status).toBe('ready');
      }
    },
  );
});
