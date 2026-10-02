import { writable } from 'svelte/store';

/** Loading is independent of intent. Completion never navigates or runs actions. */
export function deferredModule<T>(load: () => Promise<T>) {
  const state = writable<{ value: T | null; status: 'idle' | 'loading' | 'ready' | 'failed' | 'changed' }>({ value: null, status: 'idle' });
  let value: T | null = null;
  let pending: Promise<void> | null = null;
  let surface: string | null = null;
  let context: string | null = null;
  let observedContext: string | null = null;
  let status: 'idle' | 'loading' | 'ready' | 'failed' | 'changed' = 'idle';
  const publish = () => state.set({ value, status });
  function start() {
    if (!surface || context === null) return;
    if (value) { status = 'ready'; publish(); return; }
    status = 'loading'; publish();
    if (pending) return;
    pending = Promise.resolve().then(load).then((module) => {
      value = module;
      if (surface && context !== null && status === 'loading') status = 'ready';
    }).catch(() => {
      if (surface && status === 'loading') status = 'failed';
    }).finally(() => { pending = null; publish(); });
  }
  return {
    subscribe: state.subscribe,
    select(nextSurface: string | null, nextContext: string | null) {
      if (nextSurface !== surface) {
        surface = nextSurface; context = nextContext; observedContext = nextContext;
        status = 'idle';
        if (surface && context !== null) start();
        else publish();
        return;
      }
      if (!surface || nextContext === observedContext) return;
      observedContext = nextContext;
      if (status === 'ready' && nextContext !== null) { context = nextContext; return; }
      context = null; status = 'changed'; publish();
    },
    retry(nextContext: string | null) {
      context = nextContext; observedContext = nextContext;
      if (context !== null) start();
    },
  };
}
