import { tick } from 'svelte';
import { cleanup, render } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import WorkspaceManager from '$components/WorkspaceManager.svelte';
import { relayStore } from '$lib/store';
import type { Agent, CommandResult, RelayConnectionView, RelayWorkspace } from '$lib/types';

const connection = {
  status: 'connected', inventory: { state: 'ready' },
  capabilities: ['tab_reorder', 'workspace_management', 'workspace_reorder_block'],
} as unknown as RelayConnectionView;
const agents: Agent[] = [1, 2, 3].map((n) => ({
  relay_id: 'test', relay_label: 'Test', raw_pane_id: `p${n}`, pane_id: `test::p${n}`,
  workspace_id: 'w1', project: 'project', agent: 'codex', status: 'working',
  tab_id: `t${n}`, tab_number: n, tab_order: n, tab_label: `Tab ${n}`,
}));
const workspaces: RelayWorkspace[] = [1, 2, 3].map((n) => ({
  relay_id: 'test', relay_label: 'Test', workspace_id: `w${n}`, number: n, label: `Workspace ${n}`,
  focused: false, pane_count: 1, tab_count: 1, active_tab_id: '', agent_status: '', cwd: `/work/${n}`,
}));

async function pointer(node: HTMLElement, type: string, clientY: number, options: { pointerId?: number; pointerType?: string; clientX?: number; isPrimary?: boolean; button?: number } = {}) {
  const event = new MouseEvent(type, { bubbles: true, cancelable: true, clientY, clientX: options.clientX ?? 100, button: options.button ?? 0 });
  Object.defineProperties(event, { pointerId: { value: options.pointerId ?? 1 }, pointerType: { value: options.pointerType ?? 'mouse' }, isPrimary: { value: options.isPrimary ?? true } });
  node.dispatchEvent(event);
  await tick();
  return event;
}

function pointerClick(node: HTMLElement) {
  const event = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
  node.dispatchEvent(event);
  return event;
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  relayStore.workspaces.set([]);
  relayStore.connections.set(new Map());
  relayStore.relayConfigs.set([]);
});

for (const kind of ['tabs', 'workspaces'] as const) {
  describe(`${kind} actual drag geometry (synthetic dispatch)`, () => {
    for (const scenario of ['forward', 'backward', 'noop', 'missing-source', 'missing-other'] as const) {
      it(scenario, async () => {
        vi.useFakeTimers();
        vi.stubGlobal('scrollY', 300);
        vi.spyOn(window, 'scrollBy').mockImplementation(() => {});
        const tabMove = vi.spyOn(relayStore, 'reorderTab').mockReturnValue(new Promise<CommandResult>(() => {}));
        const workspaceMove = vi.spyOn(relayStore, 'reorderWorkspaceBlock').mockReturnValue(new Promise<CommandResult>(() => {}));
        relayStore.relayConfigs.set([{ id: 'test', label: 'Test', url: 'ws://test', token: '' }]);
        relayStore.connections.set(new Map([['test', connection as never]]));
        relayStore.workspaces.set(workspaces);
        const { container } = kind === 'tabs'
          ? render(AgentList, { agents, relays: [], connections: new Map([['test', connection]]), responding: new Set<string>(), onopen: vi.fn() })
          : render(WorkspaceManager);
        await tick();
        const slots = [...container.querySelectorAll<HTMLElement>(kind === 'tabs' ? '.workspace-tab' : '.workspace-management-slot')];
        expect(slots).toHaveLength(3);
        // Document tops 400, 450, 540; unequal heights, first gap 10, second gap 20.
        slots.forEach((slot, i) => vi.spyOn(slot, 'getBoundingClientRect').mockReturnValue({ top: [100, 150, 240][i], height: [40, 70, 30][i] } as DOMRect));
        const source = scenario === 'backward' ? 2 : 0;
        const handle = kind === 'tabs' ? slots[source].querySelector<HTMLElement>('.agent-open')! : slots[source];
        if (scenario === 'missing-source' || scenario === 'missing-other') {
          delete slots[scenario === 'missing-source' ? source : 1].dataset[kind === 'tabs' ? 'tabId' : 'workspaceKey'];
        }
        await pointer(handle, 'pointerdown', 120);
        await vi.advanceTimersByTimeAsync(550);
        await tick();
        const transform = (i: number) => slots[i].style.transform;
        if (scenario === 'missing-source' && kind === 'workspaces') {
          expect(transform(source)).toBe('');
        } else {
          expect(transform(source)).toBe('translateY(0px)');
        }
        // Change scroll after measurement: client 135 now means document 555,
        // exactly the last midpoint. Strict > must leave it in the middle slot.
        vi.stubGlobal('scrollY', 420);
        if (scenario === 'forward') {
          await pointer(handle, 'pointermove', 135);
          expect(transform(1)).toBe('translateY(-50px)');
          expect(transform(2)).toBe('');
        }
        const target = scenario === 'backward' ? -1 : scenario === 'noop' ? 0 : 136;
        await pointer(handle, 'pointermove', target);
        if (scenario === 'forward') {
          expect(transform(0)).toBe('translateY(136px)');
          expect(transform(1)).toBe('translateY(-50px)');
          expect(transform(2)).toBe('translateY(-50px)');
        } else if (scenario === 'backward') {
          expect(transform(0)).toBe('translateY(40px)');
          expect(transform(1)).toBe('translateY(40px)');
        } else if (scenario === 'missing-other') {
          expect(transform(1)).toBe('');
          expect(transform(2)).toBe('translateY(-50px)');
        }
        await pointer(handle, 'pointerup', target);
        const move = kind === 'tabs' ? tabMove : workspaceMove;
        if (scenario === 'noop' || scenario === 'missing-source') {
          expect(move).not.toHaveBeenCalled();
        } else if (kind === 'tabs') {
          expect(tabMove).toHaveBeenCalledExactlyOnceWith(agents[source], scenario === 'backward' ? 0 : 3);
        } else {
          expect(workspaceMove).toHaveBeenCalledExactlyOnceWith('test', [workspaces[source].workspace_id], scenario === 'backward' ? 'w1' : '', scenario === 'backward' ? 0 : 3);
        }
      });
    }
  });
}

async function gestureFixture(kind: 'tabs' | 'workspaces', readOnly = false) {
  vi.useFakeTimers();
  vi.spyOn(window, 'scrollBy').mockImplementation(() => {});
  const vibrate = vi.fn();
  vi.stubGlobal('navigator', { ...navigator, vibrate });
  const tabMove = vi.spyOn(relayStore, 'reorderTab').mockReturnValue(new Promise<CommandResult>(() => {}));
  const workspaceMove = vi.spyOn(relayStore, 'reorderWorkspaceBlock').mockReturnValue(new Promise<CommandResult>(() => {}));
  relayStore.relayConfigs.set([{ id: 'test', label: 'Test', url: 'ws://test', token: '' }]);
  relayStore.connections.set(new Map([['test', connection as never]]));
  relayStore.workspaces.set(workspaces);
  const onopen = vi.fn();
  const view = kind === 'tabs'
    ? render(AgentList, { agents, relays: [], connections: new Map([['test', connection]]), responding: new Set<string>(), onopen })
    : render(WorkspaceManager, { readOnlyRelayIds: new Set(readOnly ? ['test'] : []) });
  await tick();
  const slots = [...view.container.querySelectorAll<HTMLElement>(kind === 'tabs' ? '.workspace-tab' : '.workspace-management-slot')];
  slots.forEach((slot, i) => vi.spyOn(slot, 'getBoundingClientRect').mockReturnValue({ top: 100 + i * 60, height: 50 } as DOMRect));
  const handles = slots.map(slot => kind === 'tabs' ? slot.querySelector<HTMLElement>('.agent-open')! : slot);
  const capture = vi.fn();
  handles[0].setPointerCapture = capture;
  const hold = async () => {
    await vi.advanceTimersByTimeAsync(550);
    await tick();
  };
  const event = (type: string) => {
    const event = new Event(type, { bubbles: true, cancelable: true });
    handles[0].dispatchEvent(event);
    return event;
  };
  return { ...view, slots, handles, hold, event, capture, vibrate, onopen, move: kind === 'tabs' ? tabMove : workspaceMove };
}

function tabPreview(slots: HTMLElement[]) {
  return slots.map(slot => ({ dragging: slot.classList.contains('tab-dragging'), transform: slot.style.transform }));
}

function workspacePreview(container: HTMLElement) {
  return [...container.querySelectorAll<HTMLElement>('.workspace-management-slot')].map(slot => ({
    dragging: slot.classList.contains('workspace-dragging'), transform: slot.style.transform,
  }));
}

async function addWorkspaceRelay(otherWorkspaces = workspaces.map(workspace => ({ ...workspace, relay_id: 'other', relay_label: 'Other' }))) {
  relayStore.relayConfigs.set([
    { id: 'test', label: 'Test', url: 'ws://test', token: '' },
    { id: 'other', label: 'Other', url: 'ws://other', token: '' },
  ]);
  relayStore.connections.set(new Map([['test', connection as never], ['other', connection as never]]));
  relayStore.workspaces.set([...workspaces, ...otherWorkspaces]);
  await tick();
}

function changeWorkspaceRelay(container: HTMLElement, id: string) {
  const select = container.querySelector<HTMLSelectElement>('#workspace-relay')!;
  select.value = id;
  select.dispatchEvent(new Event('change', { bubbles: true }));
}

const firstPointer = { pointerId: 11, pointerType: 'touch' };
const secondPointer = { pointerId: 1, pointerType: 'mouse' };
const nextPointer = { pointerId: 12, pointerType: 'pen' };

for (const kind of ['tabs', 'workspaces'] as const) {
  describe(`${kind} overlapping pointer ownership (synthetic dispatch)`, () => {
    it.each(['pointermove', 'pointerup', 'pointercancel'])('ignores a second pointer and its outside %s while pending', async type => {
      const f = await gestureFixture(kind);
      const timers = vi.spyOn(window, 'setTimeout');
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await vi.advanceTimersByTimeAsync(100);
      await pointer(f.handles[0], 'pointerdown', 360, secondPointer);
      expect(timers.mock.calls.filter(([, delay]) => delay === 550)).toHaveLength(1);
      expect((await pointer(document.body, type, 400, secondPointer)).defaultPrevented).toBe(false);
      await vi.advanceTimersByTimeAsync(449);
      expect(f.capture).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      await tick();
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
      await pointer(f.handles[0], 'pointermove', 150, firstPointer);
      expect(f.slots[0].style.transform).toBe('translateY(30px)');
      await pointer(f.handles[0], 'pointermove', 280, firstPointer);
      await pointer(f.handles[0], 'pointerup', 280, firstPointer);
      await pointer(f.handles[0], 'pointerup', 280, firstPointer);
      if (kind === 'tabs') expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
      else expect(f.move).toHaveBeenCalledExactlyOnceWith('test', ['w1'], '', 3);
      await vi.advanceTimersByTimeAsync(1200);
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
      expect(f.move).toHaveBeenCalledTimes(1);
    });

    it.each([
      { name: 'vertical move', type: 'pointermove', y: 133, x: 100 },
      { name: 'horizontal move', type: 'pointermove', y: 120, x: 113 },
      { name: 'release', type: 'pointerup', y: 120, x: 100 },
      { name: 'cancel', type: 'pointercancel', y: 120, x: 100 },
    ])('original outside $name cancels both overlapping holds without late effects', async ({ type, y, x }) => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await vi.advanceTimersByTimeAsync(100);
      await pointer(f.handles[0], 'pointerdown', 360, { ...secondPointer, clientX: 400 });
      expect((await pointer(document.body, type, y, { ...firstPointer, clientX: x })).defaultPrevented).toBe(false);
      // Drain both possible 550ms holds without running the component's periodic clock forever.
      await vi.advanceTimersByTimeAsync(1200);
      await tick();
      expect(f.capture).not.toHaveBeenCalled();
      expect(f.vibrate).not.toHaveBeenCalled();
      expect(f.slots[0].style.transform).toBe('');
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
      expect(f.move).not.toHaveBeenCalled();
      // The canceled lifetime does not block a subsequent normal gesture.
      await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
      await f.hold();
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(12);
      await pointer(f.handles[0], 'pointermove', 280, nextPointer);
      await pointer(f.handles[0], 'pointerup', 280, nextPointer);
      if (kind === 'tabs') expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
      else expect(f.move).toHaveBeenCalledExactlyOnceWith('test', ['w1'], '', 3);
      await vi.advanceTimersByTimeAsync(1200);
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(12);
      expect(f.move).toHaveBeenCalledTimes(1);
    });

    it('ignores a duplicate down for the same pointer during the pending lifetime', async () => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await vi.advanceTimersByTimeAsync(100);
      await pointer(f.handles[0], 'pointerdown', 360, firstPointer);
      await vi.advanceTimersByTimeAsync(450);
      await tick();
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      await pointer(f.handles[0], 'pointermove', 150, firstPointer);
      expect(f.slots[0].style.transform).toBe('translateY(30px)');
      await pointer(f.handles[0], 'pointercancel', 150, firstPointer);
      await vi.advanceTimersByTimeAsync(1200);
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
      expect(f.move).not.toHaveBeenCalled();
    });

    it.each([false, true])('destroy after overlapping downs leaves no late effects (lifted=%s)', async lifted => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      if (lifted) await f.hold();
      else await vi.advanceTimersByTimeAsync(100);
      await pointer(f.handles[0], 'pointerdown', 360, secondPointer);
      f.unmount();
      await vi.advanceTimersByTimeAsync(1200);
      expect(f.capture).toHaveBeenCalledTimes(lifted ? 1 : 0);
      if (lifted) expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(f.vibrate).toHaveBeenCalledTimes(lifted ? 1 : 0);
      expect(f.event('touchmove').defaultPrevented).toBe(false);
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
      await pointer(f.handles[0], 'pointermove', 280, firstPointer);
      await pointer(f.handles[0], 'pointerup', 280, firstPointer);
      expect(f.move).not.toHaveBeenCalled();
    });

    it.each(['different', 'same'] as const)('active drag ignores a %s-pointer down, preserving coordinates and click policy', async overlap => {
      const f = await gestureFixture(kind);
      const timers = vi.spyOn(window, 'setTimeout');
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await f.hold();
      await pointer(f.handles[0], 'pointermove', 150, firstPointer);
      expect(f.slots[0].style.transform).toBe('translateY(30px)');
      await pointer(f.handles[0], 'pointerdown', 360, overlap === 'same' ? firstPointer : secondPointer);
      if (overlap === 'different') {
        expect((await pointer(f.handles[0], 'pointermove', 400, secondPointer)).defaultPrevented).toBe(false);
        await pointer(f.handles[0], 'pointerup', 400, secondPointer);
        await pointer(f.handles[0], 'pointercancel', 400, secondPointer);
        expect(f.move).not.toHaveBeenCalled();
        expect(f.slots[0].style.transform).toBe('translateY(30px)');
      }
      expect(timers.mock.calls.filter(([, delay]) => delay === 550)).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(550);
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
      await pointer(f.handles[0], 'pointermove', 280, firstPointer);
      expect(f.slots[0].style.transform).toBe('translateY(160px)');
      await pointer(f.handles[0], 'pointerup', 280, firstPointer);
      await pointer(f.handles[0], 'pointerup', 280, firstPointer);
      if (kind === 'tabs') {
        expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
        f.handles[0].click();
        expect(f.onopen).not.toHaveBeenCalled();
        f.handles[0].click();
        expect(f.onopen).toHaveBeenCalledTimes(1);
      } else expect(f.move).toHaveBeenCalledExactlyOnceWith('test', ['w1'], '', 3);
      await vi.advanceTimersByTimeAsync(1200);
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(f.move).toHaveBeenCalledTimes(1);
    });

    it('allows the next gesture after an overlapped lifetime completes without a reorder', async () => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await f.hold();
      await pointer(f.handles[0], 'pointerdown', 360, secondPointer);
      await pointer(f.handles[0], 'pointerup', 120, firstPointer);
      expect(f.move).not.toHaveBeenCalled();
      await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
      await f.hold();
      expect(f.capture.mock.calls).toEqual([[11], [12]]);
      await pointer(f.handles[0], 'pointermove', 280, nextPointer);
      await pointer(f.handles[0], 'pointerup', 280, nextPointer);
      if (kind === 'tabs') expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
      else expect(f.move).toHaveBeenCalledExactlyOnceWith('test', ['w1'], '', 3);
      await vi.advanceTimersByTimeAsync(1200);
      expect(f.capture.mock.calls).toEqual([[11], [12]]);
      expect(f.move).toHaveBeenCalledTimes(1);
    });

    it('does not lock a different action node', async () => {
      const f = await gestureFixture(kind);
      const secondCapture = vi.fn();
      f.handles[1].setPointerCapture = secondCapture;
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await pointer(f.handles[1], 'pointerdown', 180, secondPointer);
      await f.hold();
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
      expect(secondCapture).toHaveBeenCalledExactlyOnceWith(1);
      expect(f.vibrate).toHaveBeenCalledTimes(2);
      await pointer(f.handles[0], 'pointercancel', 120, firstPointer);
      await pointer(f.handles[1], 'pointercancel', 180, secondPointer);
      expect(f.move).not.toHaveBeenCalled();
    });
  });
}

it.each([1, 2])('retains the lifted tab owner click suppression across %i overlapping clicks', async count => {
  const f = await gestureFixture('tabs');
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await f.hold();
  for (const other of [secondPointer, nextPointer].slice(0, count)) {
    await pointer(f.handles[0], 'pointerdown', 120, other);
    await pointer(f.handles[0], 'pointerup', 120, other);
    const click = pointerClick(f.handles[0]);
    expect(f.onopen).not.toHaveBeenCalled();
    expect(click.defaultPrevented).toBe(true);
  }
  await pointer(f.handles[0], 'pointermove', 280, firstPointer);
  expect(f.slots[0].style.transform).toBe('translateY(160px)');
  await pointer(f.handles[0], 'pointerup', 280, firstPointer);
  expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
  const ownerClick = pointerClick(f.handles[0]);
  expect(f.onopen).not.toHaveBeenCalled();
  expect(ownerClick.defaultPrevented).toBe(true);
  await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
  await pointer(f.handles[0], 'pointerup', 120, nextPointer);
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(false);
  expect(f.onopen).toHaveBeenCalledTimes(1);
});

it('keeps a canceled lifted tab owner protected against overlapping clicks until release', async () => {
  const f = await gestureFixture('tabs');
  const empty = f.slots.map(() => ({ dragging: false, transform: '' }));
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await f.hold();
  await pointer(f.handles[0], 'pointermove', 280, firstPointer);
  expect(f.slots[0].style.transform).toBe('translateY(160px)');
  await f.rerender({ connections: new Map([['test', { ...connection, capabilities: ['workspace_management'] }]]) });
  expect(tabPreview(f.slots)).toEqual(empty);
  expect(f.event('touchmove').defaultPrevented).toBe(false);
  expect(f.event('contextmenu').defaultPrevented).toBe(false);
  expect((await pointer(f.handles[0], 'pointermove', 350, firstPointer)).defaultPrevented).toBe(false);
  await vi.advanceTimersByTimeAsync(1200);
  expect(tabPreview(f.slots)).toEqual(empty);
  expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
  expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
  expect(f.move).not.toHaveBeenCalled();
  await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
  await pointer(f.handles[0], 'pointerup', 120, secondPointer);
  for (let i = 0; i < 2; i++) {
    expect(pointerClick(f.handles[0]).defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
  }
  await pointer(f.handles[0], 'pointerup', 350, firstPointer);
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(true);
  expect(f.onopen).not.toHaveBeenCalled();
  await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
  await pointer(f.handles[0], 'pointerup', 120, nextPointer);
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(false);
  expect(f.onopen).toHaveBeenCalledTimes(1);
  expect(f.move).not.toHaveBeenCalled();
});

it.each(['pointerup', 'pointercancel'])('canceled lifted owner ignores wrong IDs and ends on outside %s after repeated eligibility changes', async terminal => {
  const f = await gestureFixture('tabs');
  const empty = f.slots.map(() => ({ dragging: false, transform: '' }));
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await f.hold();
  await pointer(f.handles[0], 'pointermove', 280, firstPointer);
  const add = vi.spyOn(document, 'addEventListener');
  const remove = vi.spyOn(document, 'removeEventListener');
  const ineligible = new Map([['test', { ...connection, capabilities: ['workspace_management'] }]]);
  const eligible = new Map([['test', connection]]);
  await f.rerender({ connections: ineligible });
  await f.rerender({ connections: eligible });
  await f.rerender({ connections: ineligible });
  await f.rerender({ connections: eligible });
  const terminalObservers = add.mock.calls.filter(([type, , capture]) =>
    (type === 'pointerup' || type === 'pointercancel') && capture === true).slice(-2);
  for (const type of ['pointermove', 'pointerup', 'pointercancel']) {
    expect((await pointer(document.body, type, 400, secondPointer)).defaultPrevented).toBe(false);
    expect((await pointer(f.handles[0], type, 400, secondPointer)).defaultPrevented).toBe(false);
  }
  expect((await pointer(document.body, 'pointermove', 400, firstPointer)).defaultPrevented).toBe(false);
  expect((await pointer(f.handles[0], 'pointermove', 400, firstPointer)).defaultPrevented).toBe(false);
  await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
  await pointer(f.handles[0], 'pointerup', 120, secondPointer);
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(true);
  expect(f.onopen).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(1200);
  expect(tabPreview(f.slots)).toEqual(empty);
  expect(f.capture).toHaveBeenCalledExactlyOnceWith(11);
  expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
  expect(f.move).not.toHaveBeenCalled();
  expect(terminalObservers).toHaveLength(2);
  const removalsBeforeTerminal = remove.mock.calls.length;
  expect((await pointer(document.body, terminal, 400, firstPointer)).defaultPrevented).toBe(false);
  const terminalRemovals = remove.mock.calls.slice(removalsBeforeTerminal);
  for (const [type, listener] of terminalObservers) {
    expect(terminalRemovals.some(([t, l, capture]) => t === type && l === listener && capture === true)).toBe(true);
  }
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(true);
  await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
  await f.hold();
  await pointer(f.handles[0], 'pointermove', 280, nextPointer);
  await pointer(f.handles[0], 'pointerup', 280, nextPointer);
  expect(f.capture.mock.calls).toEqual([[11], [12]]);
  expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
});

it.each(['pointerup', 'pointercancel'])('pending invalidation cannot relift and releases ownership on outside %s', async terminal => {
  const f = await gestureFixture('tabs');
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await vi.advanceTimersByTimeAsync(100);
  await f.rerender({ connections: new Map([['test', { ...connection, capabilities: ['workspace_management'] }]]) });
  await f.rerender({ connections: new Map([['test', connection]]) });
  await pointer(document.body, 'pointermove', 400, firstPointer);
  await pointer(document.body, 'pointercancel', 400, secondPointer);
  await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
  await pointer(f.handles[0], 'pointerup', 120, secondPointer);
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(true);
  f.handles[0].click(); // Keyboard/programmatic activation remains available before lift.
  expect(f.onopen).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1200);
  expect(tabPreview(f.slots)).toEqual(f.slots.map(() => ({ dragging: false, transform: '' })));
  expect(f.capture).not.toHaveBeenCalled();
  expect(f.vibrate).not.toHaveBeenCalled();
  expect(f.move).not.toHaveBeenCalled();
  await pointer(document.body, terminal, 400, firstPointer);
  await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
  await f.hold();
  await pointer(f.handles[0], 'pointermove', 280, nextPointer);
  await pointer(f.handles[0], 'pointerup', 280, nextPointer);
  expect(f.capture).toHaveBeenCalledExactlyOnceWith(12);
  expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
  expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
});

it.each([false, true])('destroying a canceled tab owner removes terminal observers and late effects (lifted=%s)', async lifted => {
  const f = await gestureFixture('tabs');
  const sibling = { ...agents[0], raw_pane_id: 'sibling', pane_id: 'test::sibling' };
  await f.rerender({ agents: [agents[0], sibling, ...agents.slice(1)] });
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  if (lifted) await f.hold();
  else await vi.advanceTimersByTimeAsync(100);
  const add = vi.spyOn(document, 'addEventListener');
  const remove = vi.spyOn(document, 'removeEventListener');
  await f.rerender({ connections: new Map([['test', { ...connection, capabilities: ['workspace_management'] }]]) });
  // Clearing the preview can update the action again; inspect the last registered pair.
  const terminalObservers = add.mock.calls.filter(([type, , capture]) =>
    (type === 'pointerup' || type === 'pointercancel') && capture === true).slice(-2);
  expect(terminalObservers).toHaveLength(2);
  const removalsBeforeDestroy = remove.mock.calls.length;
  await f.rerender({ agents: [sibling, ...agents.slice(1)] });
  expect(f.handles[0].isConnected).toBe(false);
  const destroyRemovals = remove.mock.calls.slice(removalsBeforeDestroy);
  for (const [type, listener] of terminalObservers) {
    expect(destroyRemovals.some(([t, l, capture]) => t === type && l === listener && capture === true)).toBe(true);
  }
  await pointer(document.body, 'pointerup', 280, firstPointer);
  await vi.advanceTimersByTimeAsync(1200);
  expect(tabPreview(f.slots)).toEqual(f.slots.map(() => ({ dragging: false, transform: '' })));
  expect(f.capture).toHaveBeenCalledTimes(lifted ? 1 : 0);
  expect(f.vibrate).toHaveBeenCalledTimes(lifted ? 1 : 0);
  expect(f.move).not.toHaveBeenCalled();
});

it('blocks a pending overlap click without blocking scrolling or non-pointer activation', async () => {
  const f = await gestureFixture('tabs');
  f.handles[0].click();
  expect(f.onopen).toHaveBeenCalledTimes(1);
  expect((await pointer(f.handles[0], 'pointerdown', 120, firstPointer)).defaultPrevented).toBe(false);
  f.handles[0].click();
  f.handles[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
  f.handles[0].dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 0 }));
  expect(f.onopen).toHaveBeenCalledTimes(3);
  expect((await pointer(f.handles[0], 'pointerdown', 120, secondPointer)).defaultPrevented).toBe(false);
  expect((await pointer(f.handles[0], 'pointerup', 120, secondPointer)).defaultPrevented).toBe(false);
  expect(f.event('touchmove').defaultPrevented).toBe(false);
  const overlapClick = pointerClick(f.handles[0]);
  expect(f.onopen).toHaveBeenCalledTimes(3);
  expect(overlapClick.defaultPrevented).toBe(true);
  await pointer(f.handles[0], 'pointerup', 120, firstPointer);
  expect(pointerClick(f.handles[0]).defaultPrevented).toBe(false);
  expect(f.onopen).toHaveBeenCalledTimes(4);
  await vi.advanceTimersByTimeAsync(1200);
  expect(f.capture).not.toHaveBeenCalled();
  expect(f.move).not.toHaveBeenCalled();
});

it('does not erase tab click suppression on an overlapping down, but resets it for the next inactive press', async () => {
  const f = await gestureFixture('tabs');
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await f.hold();
  await pointer(f.handles[0], 'pointerdown', 360, secondPointer);
  await pointer(f.handles[0], 'pointerup', 120, firstPointer);
  f.handles[0].click();
  expect(f.onopen).not.toHaveBeenCalled();
  await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
  await f.hold();
  await pointer(f.handles[0], 'pointerup', 120, nextPointer);
  await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
  await pointer(f.handles[0], 'pointerup', 120, secondPointer);
  f.handles[0].click();
  expect(f.onopen).toHaveBeenCalledTimes(1);
});

for (const kind of ['tabs', 'workspaces'] as const) {
  describe(`${kind} gesture lifecycle (synthetic dispatch)`, () => {
    it('gates nonprimary/right pointers, ignores wrong identity and commits once', async () => {
      const f = await gestureFixture(kind);
      for (const options of [{ isPrimary: false }, { button: 2 }]) {
        await pointer(f.handles[0], 'pointerdown', 120, options);
        await f.hold();
        expect(f.capture).not.toHaveBeenCalled();
      }
      await pointer(f.handles[0], 'pointerdown', 120);
      await pointer(f.handles[0], 'pointermove', 200, { pointerId: 2 });
      await f.hold();
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(1);
      expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
      await pointer(f.handles[0], 'pointercancel', 120, { pointerId: 2 });
      await pointer(f.handles[0], 'pointerup', 120, { pointerId: 2 });
      expect(f.move).not.toHaveBeenCalled();
      expect((await pointer(f.handles[0], 'pointermove', 280)).defaultPrevented).toBe(true);
      await pointer(f.handles[0], 'pointerup', 280);
      await pointer(f.handles[0], 'pointerup', 280);
      expect(f.move).toHaveBeenCalledTimes(1);
      // Pending remote mutation keeps the caller's moving guard active.
      await pointer(f.handles[0], 'pointerdown', 120);
      await f.hold();
      expect(f.capture).toHaveBeenCalledTimes(1);
    });

    it('allows pre-hold scrolling and cancels beyond 12px slop', async () => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120);
      expect(f.event('contextmenu').defaultPrevented).toBe(true);
      expect(f.event('touchmove').defaultPrevented).toBe(false);
      expect((await pointer(f.handles[0], 'pointermove', 133)).defaultPrevented).toBe(false);
      await f.hold();
      expect(f.capture).not.toHaveBeenCalled();
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
      expect(f.event('touchmove').defaultPrevented).toBe(false);
      await pointer(f.handles[0], 'pointerup', 280);
      expect(f.move).not.toHaveBeenCalled();
    });

    it('lifts at 550ms with exactly 12px movement; cancel never commits', async () => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120);
      await pointer(f.handles[0], 'pointermove', 132);
      await vi.advanceTimersByTimeAsync(549);
      expect(f.capture).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      await tick();
      expect(f.capture).toHaveBeenCalledTimes(1);
      expect(f.event('touchmove').defaultPrevented).toBe(true);
      expect(f.event('contextmenu').defaultPrevented).toBe(true);
      await pointer(f.handles[0], 'pointermove', 280);
      await pointer(f.handles[0], 'pointercancel', 280);
      await pointer(f.handles[0], 'pointerup', 280);
      expect(f.move).not.toHaveBeenCalled();
      expect(f.slots[0].style.transform).toBe('');
      expect(f.event('touchmove').defaultPrevented).toBe(false);
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
    });

    it.each([false, true])('destroy clears listeners/timer (lifted=%s)', async lifted => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120);
      if (lifted) await f.hold();
      f.unmount();
      await f.hold();
      expect(f.capture).toHaveBeenCalledTimes(lifted ? 1 : 0);
      expect(f.event('touchmove').defaultPrevented).toBe(false);
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
      await pointer(f.handles[0], 'pointermove', 280);
      await pointer(f.handles[0], 'pointerup', 280);
      expect(f.move).not.toHaveBeenCalled();
    });

    it.each(['pointermove', 'pointerup', 'pointercancel'])('outside %s cancels only the initiating hold without prevention', async type => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120);
      await pointer(document.body, type, 133, { pointerId: 2 });
      expect(f.event('contextmenu').defaultPrevented).toBe(true);
      expect((await pointer(document.body, type, type === 'pointermove' ? 133 : 120)).defaultPrevented).toBe(false);
      await f.hold();
      expect(f.capture).not.toHaveBeenCalled();
      expect(f.slots[0].style.transform).toBe('');
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
      expect(f.move).not.toHaveBeenCalled();
      // A subsequent valid hold still lifts; no stale observer cancels it.
      await pointer(f.handles[0], 'pointerdown', 120);
      await pointer(document.body, 'pointermove', 132);
      await f.hold();
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(1);
      await pointer(f.handles[0], 'pointercancel', 120);
    });

    it.each(['reset', 'lift', 'failed-lift', 'destroy'])('removes pending document observers on %s', async ending => {
      const f = await gestureFixture(kind);
      const add = vi.spyOn(document, 'addEventListener');
      const remove = vi.spyOn(document, 'removeEventListener');
      await pointer(f.handles[0], 'pointerdown', 120);
      const observers = add.mock.calls.filter(([type]) => ['pointermove', 'pointerup', 'pointercancel'].includes(type));
      expect(observers).toHaveLength(3);
      if (ending === 'reset') await pointer(document.body, 'pointerup', 120);
      else if (ending === 'destroy') f.unmount();
      else {
        if (ending === 'failed-lift') {
          f.slots.slice(1).forEach(slot => slot.classList.remove(kind === 'tabs' ? 'workspace-tab' : 'workspace-management-slot'));
        }
        await f.hold();
      }
      for (const [type, listener] of observers) {
        expect(remove).toHaveBeenCalledWith(type, listener, true);
      }
      await pointer(document.body, 'pointercancel', 120);
      await f.hold();
      expect(f.capture).toHaveBeenCalledTimes(ending === 'lift' ? 1 : 0);
      if (ending === 'lift') {
        // Pending observer is gone: document dispatch cannot cancel captured drag.
        expect(f.slots[0].style.transform).toBe('translateY(0px)');
        await pointer(f.handles[0], 'pointercancel', 120);
      }
      expect(f.move).not.toHaveBeenCalled();
    });

    it('pointercancel before hold prevents later lift', async () => {
      const f = await gestureFixture(kind);
      await pointer(f.handles[0], 'pointerdown', 120);
      await pointer(f.handles[0], 'pointercancel', 120);
      await f.hold();
      expect(f.capture).not.toHaveBeenCalled();
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
    });
  });
}

it('agent taps open; held clicks are suppressed component-wide until click or next down', async () => {
  const f = await gestureFixture('tabs');
  await pointer(f.handles[0], 'pointerdown', 120);
  await pointer(f.handles[0], 'pointerup', 120);
  f.handles[0].click();
  expect(f.onopen).toHaveBeenCalledTimes(1);
  await pointer(f.handles[0], 'pointerdown', 120);
  await f.hold();
  await pointer(f.handles[0], 'pointerup', 120);
  f.handles[1].click();
  expect(f.onopen).toHaveBeenCalledTimes(1);
  f.handles[1].click();
  expect(f.onopen).toHaveBeenCalledTimes(2);
  await pointer(f.handles[0], 'pointerdown', 120);
  await f.hold();
  await pointer(f.handles[0], 'pointercancel', 120);
  await pointer(f.handles[1], 'pointerdown', 180);
  await pointer(f.handles[1], 'pointerup', 180);
  f.handles[1].click();
  expect(f.onopen).toHaveBeenCalledTimes(3);
});

it('workspace missing-source lift removes pending observers before measurement and resets', async () => {
  const f = await gestureFixture('workspaces');
  const add = vi.spyOn(document, 'addEventListener');
  const remove = vi.spyOn(document, 'removeEventListener');
  await pointer(f.handles[0], 'pointerdown', 120);
  const observers = add.mock.calls.filter(([type]) => ['pointermove', 'pointerup', 'pointercancel'].includes(type));
  expect(observers).toHaveLength(3);
  delete f.slots[0].dataset.workspaceKey;
  vi.mocked(f.slots[0].getBoundingClientRect).mockImplementation(() => {
    for (const [type, listener] of observers) expect(remove).toHaveBeenCalledWith(type, listener, true);
    return { top: 100, height: 50 } as DOMRect;
  });
  await f.hold();
  expect(f.capture).not.toHaveBeenCalled();
  expect(f.event('contextmenu').defaultPrevented).toBe(false);
  expect(f.event('touchmove').defaultPrevented).toBe(false);
  expect(f.slots[0].style.transform).toBe('');
  await pointer(document.body, 'pointercancel', 120);
  await pointer(f.handles[0], 'pointerup', 280);
  expect(f.move).not.toHaveBeenCalled();
  f.slots[0].dataset.workspaceKey = 'w1';
  await pointer(f.handles[0], 'pointerdown', 120);
  await f.hold();
  expect(f.capture).toHaveBeenCalledExactlyOnceWith(1);
  await pointer(f.handles[0], 'pointercancel', 120);
});

it('workspace read-only and interactive controls cannot start a hold', async () => {
  const f = await gestureFixture('workspaces', true);
  await pointer(f.handles[0], 'pointerdown', 120);
  await f.hold();
  expect(f.capture).not.toHaveBeenCalled();
  await f.rerender({ readOnlyRelayIds: new Set<string>() });
  const control = f.slots[0].querySelector<HTMLElement>('button:not(.workspace-drag-handle)')!;
  expect(control).toBeTruthy();
  await pointer(control, 'pointerdown', 120);
  await f.hold();
  expect(f.capture).not.toHaveBeenCalled();
  await pointer(f.slots[0].querySelector<HTMLElement>('.workspace-drag-handle')!, 'pointerdown', 120);
  await f.hold();
  expect(f.capture).toHaveBeenCalledTimes(1);
});

for (const when of ['hold', 'drag'] as const) {
  it(`tab commit uses the latest agent after an update during ${when}`, async () => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120);
    if (when === 'drag') await f.hold();
    const updated = { ...agents[0], project: `updated during ${when}` };
    await f.rerender({ agents: [updated, ...agents.slice(1)] });
    if (when === 'hold') await f.hold();
    await pointer(f.handles[0], 'pointermove', 280);
    await pointer(f.handles[0], 'pointerup', 280);
    expect(f.move).toHaveBeenCalledExactlyOnceWith(updated, 3);
  });

  it(`tab capability removal during ${when} cancels the lifetime and rendered preview without commit`, async () => {
    const f = await gestureFixture('tabs');
    const add = vi.spyOn(document, 'addEventListener');
    const remove = vi.spyOn(document, 'removeEventListener');
    const empty = f.slots.map(() => ({ dragging: false, transform: '' }));
    await pointer(f.handles[0], 'pointerdown', 120);
    const observers = add.mock.calls.filter(([type]) => ['pointermove', 'pointerup', 'pointercancel'].includes(type));
    expect(observers).toHaveLength(3);
    if (when === 'drag') {
      await f.hold();
      await pointer(f.handles[0], 'pointermove', 280);
      expect(f.capture).toHaveBeenCalledExactlyOnceWith(1);
      expect(f.vibrate).toHaveBeenCalledExactlyOnceWith(12);
      expect(tabPreview(f.slots)).toEqual([
        { dragging: true, transform: 'translateY(160px)' },
        { dragging: false, transform: 'translateY(-60px)' },
        { dragging: false, transform: 'translateY(-60px)' },
      ]);
    } else await vi.advanceTimersByTimeAsync(100);
    await f.rerender({ connections: new Map([['test', { ...connection, capabilities: ['workspace_management'] }]]) });
    const immediately = tabPreview(f.slots);
    const removedOnInvalidation = observers.map(([type, listener]) => remove.mock.calls.some(
      ([removedType, removedListener, capture]) => removedType === type && removedListener === listener && capture === true,
    ));
    const contextPrevented = f.event('contextmenu').defaultPrevented;
    const touchPrevented = f.event('touchmove').defaultPrevented;
    await vi.advanceTimersByTimeAsync(1200);
    await tick();
    const afterDeadline = tabPreview(f.slots);
    const move = await pointer(f.handles[0], 'pointermove', 280);
    await pointer(f.handles[0], 'pointerup', 280);
    await pointer(f.handles[0], 'pointercancel', 280);
    const afterEvents = tabPreview(f.slots);
    expect(f.move).not.toHaveBeenCalled();
    expect(immediately).toEqual(empty);
    expect(afterDeadline).toEqual(empty);
    expect(afterEvents).toEqual(empty);
    expect(move.defaultPrevented).toBe(false);
    expect(contextPrevented).toBe(false);
    expect(touchPrevented).toBe(false);
    expect(f.capture).toHaveBeenCalledTimes(when === 'drag' ? 1 : 0);
    expect(f.vibrate).toHaveBeenCalledTimes(when === 'drag' ? 1 : 0);
    expect(removedOnInvalidation).toEqual([true, true, true]);
    const click = pointerClick(f.handles[0]);
    expect(click.defaultPrevented).toBe(when === 'drag');
    expect(f.onopen).toHaveBeenCalledTimes(when === 'drag' ? 0 : 1);
    if (when === 'drag') {
      pointerClick(f.handles[0]);
      expect(f.onopen).toHaveBeenCalledTimes(1);
    }
  });

  it(`tab ${when} invalidation leaves no old preview after restored capability and same-ID pending cancel`, async () => {
    const f = await gestureFixture('tabs');
    const empty = f.slots.map(() => ({ dragging: false, transform: '' }));
    await pointer(f.handles[0], 'pointerdown', 120);
    if (when === 'drag') {
      await f.hold();
      await pointer(f.handles[0], 'pointermove', 280);
    } else await vi.advanceTimersByTimeAsync(100);
    await f.rerender({ connections: new Map([['test', { ...connection, capabilities: ['workspace_management'] }]]) });
    await vi.advanceTimersByTimeAsync(1200);
    await pointer(f.handles[0], 'pointerup', 280);
    await f.rerender({ connections: new Map([['test', connection]]) });
    const beforeCancel = tabPreview(f.slots);
    await pointer(f.handles[0], 'pointerdown', 120);
    await pointer(f.handles[0], 'pointercancel', 120);
    await vi.advanceTimersByTimeAsync(1200);
    await tick();
    const afterCancel = tabPreview(f.slots);
    expect(f.move).not.toHaveBeenCalled();
    expect(afterCancel).toEqual(empty);
    expect(beforeCancel).toEqual(empty);
    expect(f.capture).toHaveBeenCalledTimes(when === 'drag' ? 1 : 0);
    expect(f.vibrate).toHaveBeenCalledTimes(when === 'drag' ? 1 : 0);
    await pointer(f.handles[0], 'pointerdown', 120);
    await f.hold();
    await pointer(f.handles[0], 'pointermove', 280);
    await pointer(f.handles[0], 'pointerup', 280);
    expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
    expect(tabPreview(f.slots)).toEqual(empty);
    expect(f.capture).toHaveBeenCalledTimes(when === 'drag' ? 2 : 1);
    expect(f.vibrate).toHaveBeenCalledTimes(when === 'drag' ? 2 : 1);
  });

  it(`workspace read-only transition during ${when} prevents commit without cancelling lift`, async () => {
    const f = await gestureFixture('workspaces');
    await pointer(f.handles[0], 'pointerdown', 120);
    if (when === 'drag') await f.hold();
    await f.rerender({ readOnlyRelayIds: new Set(['test']) });
    if (when === 'hold') await f.hold();
    expect(f.capture).toHaveBeenCalledExactlyOnceWith(1);
    await pointer(f.handles[0], 'pointermove', 280);
    await pointer(f.handles[0], 'pointerup', 280);
    expect(f.move).not.toHaveBeenCalled();
  });

  it(`workspace commit uses the latest tree after an update during ${when}`, async () => {
    const f = await gestureFixture('workspaces');
    const repo = { repo_key: 'repo', repo_name: 'project', repo_root: '/work/1', checkout_path: '/work/1', is_linked_worktree: false };
    const parent = { ...workspaces[0], worktree: repo };
    relayStore.workspaces.set([parent, ...workspaces.slice(1)]);
    await tick();
    await pointer(f.handles[0], 'pointerdown', 120);
    if (when === 'drag') await f.hold();
    const child = { ...workspaces[0], workspace_id: 'w4', number: 4, worktree: { ...repo, is_linked_worktree: true } };
    relayStore.workspaces.set([parent, ...workspaces.slice(1), child]);
    await tick();
    if (when === 'hold') await f.hold();
    await pointer(f.handles[0], 'pointermove', 280);
    await pointer(f.handles[0], 'pointerup', 280);
    expect(f.move).toHaveBeenCalledExactlyOnceWith('test', ['w1', 'w4'], '', 4);
  });

  // The old characterization allowed a write to the newly selected relay. A reused
  // workspace-ID node must instead cancel its original physical pointer lifetime.
  for (const transition of ['manual', 'fallback'] as const) {
    it(`workspace ${when} cancels across ${transition} relay changes and permits a fresh gesture`, async () => {
      const f = await gestureFixture('workspaces');
      await addWorkspaceRelay();
      const add = vi.spyOn(document, 'addEventListener');
      const remove = vi.spyOn(document, 'removeEventListener');
      const empty = workspaces.map(() => ({ dragging: false, transform: '' }));
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      if (when === 'drag') {
        await f.hold();
        await pointer(f.handles[0], 'pointermove', 280, firstPointer);
        expect(workspacePreview(f.container)).toEqual([
          { dragging: true, transform: 'translateY(160px)' },
          { dragging: false, transform: 'translateY(-60px)' },
          { dragging: false, transform: 'translateY(-60px)' },
        ]);
      } else await vi.advanceTimersByTimeAsync(100);

      const switchTo = async (id: 'test' | 'other') => {
        if (transition === 'fallback') {
          relayStore.connections.set(new Map([
            ['test', (id === 'other' ? { ...connection, status: 'disconnected' } : connection) as never],
            ['other', connection as never],
          ]));
          await tick();
          if (id === 'test') changeWorkspaceRelay(f.container, id);
        } else changeWorkspaceRelay(f.container, id);
        await tick();
        expect(f.container.querySelector<HTMLSelectElement>('#workspace-relay')!.value).toBe(id);
        expect(f.container.querySelector('.workspace-management-slot')).toBe(f.slots[0]);
        return workspacePreview(f.container);
      };
      const previews = [await switchTo('other'), await switchTo('test'), await switchTo('other')];
      const endOnOriginal = (transition === 'manual') === (when === 'drag');
      if (endOnOriginal) previews.push(await switchTo('test'));
      const terminalObservers = add.mock.calls.filter(([type, , capture]) =>
        (type === 'pointerup' || type === 'pointercancel') && capture === true).slice(-2);
      await vi.advanceTimersByTimeAsync(1200);
      const afterDeadline = workspacePreview(f.container);
      await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
      await pointer(document.body, 'pointerup', 120, secondPointer);
      await pointer(document.body, 'pointercancel', 120, secondPointer);
      await f.hold();
      const capturesBeforeRelease = f.capture.mock.calls.length;
      const move = await pointer(f.handles[0], 'pointermove', 280, firstPointer);
      await pointer(f.handles[0], transition === 'manual' ? 'pointerup' : 'pointercancel', 280, firstPointer);
      expect(f.move).not.toHaveBeenCalled();
      expect(previews).toEqual(previews.map(() => empty));
      expect(afterDeadline).toEqual(empty);
      expect(workspacePreview(f.container)).toEqual(empty);
      expect(move.defaultPrevented).toBe(false);
      expect(capturesBeforeRelease).toBe(when === 'drag' ? 1 : 0);
      expect(f.vibrate).toHaveBeenCalledTimes(when === 'drag' ? 1 : 0);
      expect(terminalObservers).toHaveLength(2);
      for (const [type, listener] of terminalObservers) expect(remove).toHaveBeenCalledWith(type, listener, true);
      expect(f.event('contextmenu').defaultPrevented).toBe(false);
      expect(f.event('touchmove').defaultPrevented).toBe(false);

      if (endOnOriginal) await switchTo('other');
      await pointer(f.handles[0], 'pointerdown', 120, nextPointer);
      await f.hold();
      await pointer(f.handles[0], 'pointermove', 280, nextPointer);
      await pointer(f.handles[0], 'pointerup', 280, nextPointer);
      expect(f.move).toHaveBeenCalledExactlyOnceWith('other', ['w1'], '', 3);
      expect(workspacePreview(f.container)).toEqual(empty);
    });
  }

  it(`workspace ${when} cannot revive after same-turn switch/back before action updates`, async () => {
    const f = await gestureFixture('workspaces');
    await addWorkspaceRelay();
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    if (when === 'drag') {
      await f.hold();
      await pointer(f.handles[0], 'pointermove', 280, firstPointer);
    }
    changeWorkspaceRelay(f.container, 'other');
    changeWorkspaceRelay(f.container, 'test');
    if (when === 'hold') vi.advanceTimersByTime(550);
    await pointer(f.handles[0], 'pointermove', 280, firstPointer);
    await pointer(f.handles[0], 'pointerup', 280, firstPointer);
    expect(f.move).not.toHaveBeenCalled();
    expect(f.capture).toHaveBeenCalledTimes(when === 'drag' ? 1 : 0);
    expect(workspacePreview(f.container)).toEqual(workspaces.map(() => ({ dragging: false, transform: '' })));
  });
}

it.each(['pending', 'active'] as const)('removing a workspace %s source clears its owned preview and listeners', async state => {
  const f = await gestureFixture('workspaces');
  const add = vi.spyOn(document, 'addEventListener');
  const remove = vi.spyOn(document, 'removeEventListener');
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  const observers = add.mock.calls.filter(([type, , capture]) =>
    ['pointermove', 'pointerup', 'pointercancel'].includes(type) && capture === true);
  expect(observers).toHaveLength(3);
  if (state === 'active') {
    await f.hold();
    await pointer(f.handles[0], 'pointermove', 280, firstPointer);
    expect(f.slots[1].style.transform).toBe('translateY(-60px)');
  }
  relayStore.workspaces.set(workspaces.slice(1));
  await tick();
  expect(f.slots[0].isConnected).toBe(false);
  expect(workspacePreview(f.container)).toEqual(workspaces.slice(1).map(() => ({ dragging: false, transform: '' })));
  for (const [type, listener] of observers) expect(remove).toHaveBeenCalledWith(type, listener, true);
  await vi.advanceTimersByTimeAsync(1200);
  await pointer(f.handles[0], 'pointerup', 280, firstPointer);
  expect(f.capture).toHaveBeenCalledTimes(state === 'active' ? 1 : 0);
  expect(f.move).not.toHaveBeenCalled();
});

it.each(['pending', 'active'] as const)('switching to disjoint workspace IDs destroys the %s source cleanly', async state => {
  const f = await gestureFixture('workspaces');
  await addWorkspaceRelay(workspaces.map((workspace, i) => ({
    ...workspace, relay_id: 'other', relay_label: 'Other', workspace_id: `other-${i}`,
  })));
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  if (state === 'active') {
    await f.hold();
    await pointer(f.handles[0], 'pointermove', 280, firstPointer);
  }
  changeWorkspaceRelay(f.container, 'other');
  await tick();
  expect(f.slots[0].isConnected).toBe(false);
  expect(workspacePreview(f.container)).toEqual(workspaces.map(() => ({ dragging: false, transform: '' })));
  await vi.advanceTimersByTimeAsync(1200);
  await pointer(f.handles[0], 'pointerup', 280, firstPointer);
  expect(f.capture).toHaveBeenCalledTimes(state === 'active' ? 1 : 0);
  expect(f.move).not.toHaveBeenCalled();
});

it.each(['cancel', 'destroy'] as const)('%s of one workspace action preserves another action\'s preview', async termination => {
  const f = await gestureFixture('workspaces');
  // Same-ID multi-node dispatch is synthetic: only preview ownership is tested, not native concurrency.
  await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await f.hold();
  await pointer(f.handles[1], 'pointerdown', 180, firstPointer);
  await f.hold();
  await pointer(f.handles[1], 'pointermove', 280, firstPointer);
  expect(f.slots[1].classList.contains('workspace-dragging')).toBe(true);
  expect(f.slots[1].style.transform).toBe('translateY(100px)');
  if (termination === 'cancel') await pointer(f.handles[0], 'pointercancel', 120, firstPointer);
  else {
    relayStore.workspaces.set(workspaces.slice(1));
    await tick();
  }
  expect(f.slots[1].classList.contains('workspace-dragging')).toBe(true);
  expect(f.slots[1].style.transform).toBe('translateY(100px)');
  await pointer(f.handles[1], 'pointerup', 280, firstPointer);
  expect(f.move).toHaveBeenCalledExactlyOnceWith('test', ['w2'], '', termination === 'cancel' ? 3 : 2);
});

it.each(['pending', 'active'] as const)('invalidating one %s tab action preserves a different live action preview', async state => {
  const f = await gestureFixture('tabs');
  const otherAgents = agents.map(agent => ({
    ...agent, relay_id: 'other', relay_label: 'Other', pane_id: `other::${agent.raw_pane_id}`,
    workspace_id: 'w2', tab_id: `other-${agent.tab_id}`,
  }));
  await f.rerender({ agents: [...agents, ...otherAgents], connections: new Map([['test', connection], ['other', connection]]) });
  expect(f.container.querySelector('[data-tab-id="t1"] .agent-open')).toBe(f.handles[0]);
  const otherSlots = otherAgents.map(agent => f.container.querySelector<HTMLElement>(`[data-tab-id="${agent.tab_id}"]`)!);
  otherSlots.forEach((slot, i) => vi.spyOn(slot, 'getBoundingClientRect').mockReturnValue({ top: 100 + i * 60, height: 50 } as DOMRect));
  const otherHandle = otherSlots[0].querySelector<HTMLElement>('.agent-open')!;
  const otherCapture = vi.fn();
  otherHandle.setPointerCapture = otherCapture;
  // Same-ID multi-node dispatch is synthetic: it exercises ownership without claiming native concurrency support.
  if (state === 'active') {
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
  }
  await pointer(otherHandle, 'pointerdown', 120, firstPointer);
  await f.hold();
  await pointer(otherHandle, 'pointermove', 280, firstPointer);
  expect(otherSlots[0].classList.contains('tab-dragging')).toBe(true);
  expect(otherSlots[0].style.transform).toBe('translateY(160px)');
  if (state === 'pending') await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
  await f.rerender({ connections: new Map([['test', { ...connection, capabilities: ['workspace_management'] }], ['other', connection]]) });
  await vi.advanceTimersByTimeAsync(1200);
  await pointer(f.handles[0], 'pointermove', 280, firstPointer);
  await pointer(f.handles[0], 'pointerup', 280, firstPointer);
  await pointer(f.handles[0], 'pointercancel', 280, firstPointer);
  expect(f.move).not.toHaveBeenCalled();
  expect(otherSlots[0].classList.contains('tab-dragging')).toBe(true);
  expect(otherSlots[0].style.transform).toBe('translateY(160px)');
  expect(f.capture).toHaveBeenCalledTimes(state === 'active' ? 1 : 0);
  expect(otherCapture).toHaveBeenCalledExactlyOnceWith(11);
  await pointer(otherHandle, 'pointerup', 280, firstPointer);
  expect(f.move).toHaveBeenCalledExactlyOnceWith(otherAgents[0], 3);
});

it('destroying a lifted tab action clears its preview when another agent keeps the tab rendered', async () => {
  const f = await gestureFixture('tabs');
  const sibling = { ...agents[0], raw_pane_id: 'sibling', pane_id: 'test::sibling' };
  await f.rerender({ agents: [agents[0], sibling, ...agents.slice(1)] });
  await pointer(f.handles[0], 'pointerdown', 120);
  await f.hold();
  await pointer(f.handles[0], 'pointermove', 280);
  expect(f.slots[0].classList.contains('tab-dragging')).toBe(true);
  expect(f.slots[0].style.transform).toBe('translateY(160px)');
  await f.rerender({ agents: [sibling, ...agents.slice(1)] });
  await vi.advanceTimersByTimeAsync(1200);
  await pointer(f.handles[0], 'pointerup', 280);
  expect(f.move).not.toHaveBeenCalled();
  expect(tabPreview(f.slots)).toEqual(f.slots.map(() => ({ dragging: false, transform: '' })));
});

it('workspace clicks are not globally suppressed after a hold', async () => {
  const f = await gestureFixture('workspaces');
  const click = vi.fn();
  f.handles[0].addEventListener('click', click);
  await pointer(f.handles[0], 'pointerdown', 120);
  await f.hold();
  await pointer(f.handles[0], 'pointercancel', 120);
  f.handles[0].click();
  expect(click).toHaveBeenCalledTimes(1);
});

type ClickIdentity = { pointerId?: number; pointerType?: string; isPrimary?: boolean };

// Separate from pointerClick: the historical helper remains an unattributed detail=1 MouseEvent.
function identifiableClick(node: HTMLElement, identity: ClickIdentity, detail = 1) {
  const event = new MouseEvent('click', { bubbles: true, cancelable: true, detail });
  for (const [key, value] of Object.entries(identity)) Object.defineProperty(event, key, { value });
  node.dispatchEvent(event);
  expect(event).toMatchObject({ ...identity, detail, isTrusted: false });
  return event;
}

describe('bounded identifiable lifted tab click ownership (synthetic dispatch)', () => {
  it.each([1, 2])('protects A after reset across %i withheld B clicks and commits exactly once', async count => {
    const f = await gestureFixture('tabs');
    const node = f.handles[0];
    await pointer(node, 'pointerdown', 120, firstPointer);
    await f.hold();
    const others = [secondPointer, nextPointer].slice(0, count);
    for (const other of others) {
      await pointer(node, 'pointerdown', 120, other);
      await pointer(node, 'pointerup', 120, other); // Deliberately withhold B's click.
    }
    await pointer(node, 'pointermove', 280, firstPointer);
    await pointer(node, 'pointerup', 280, firstPointer);
    expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
    for (const other of others) {
      expect(identifiableClick(node, other).defaultPrevented).toBe(true);
      expect(f.onopen).not.toHaveBeenCalled();
    }
    expect(identifiableClick(node, firstPointer).defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
    expect(f.move).toHaveBeenCalledExactlyOnceWith(agents[0], 3);
    expect(identifiableClick(node, secondPointer).defaultPrevented).toBe(false); // Ticket retired.
    expect(f.onopen).toHaveBeenCalledTimes(1);
  });

  it.each([0, 1])('owner-only detail=%i retires identity and fallback without requiring click isPrimary', async detail => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(f.handles[0], 'pointerup', 120, firstPointer);
    expect(identifiableClick(f.handles[0], { ...firstPointer, isPrimary: false }, detail).defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
    expect(pointerClick(f.handles[0]).defaultPrevented).toBe(false);
    f.handles[0].click();
    expect(f.onopen).toHaveBeenCalledTimes(2);
    expect(f.move).not.toHaveBeenCalled();
  });

  it('does not retire the ticket during the active lifetime', async () => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    for (const identity of [firstPointer, secondPointer, firstPointer]) {
      expect(identifiableClick(f.handles[0], identity, 0).defaultPrevented).toBe(true);
    }
    await pointer(f.handles[0], 'pointerup', 120, firstPointer);
    expect(identifiableClick(f.handles[0], secondPointer).defaultPrevented).toBe(true);
    expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
    expect(f.move).not.toHaveBeenCalled();
  });

  it.each(['pointerup', 'pointercancel'])('retains an invalidated owner through outside %s and delayed overlaps', async terminal => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    await f.rerender({ connections: new Map([['test', { ...connection, capabilities: [] }]]) });
    await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
    await pointer(f.handles[0], 'pointerup', 120, secondPointer);
    expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(true);
    await pointer(document.body, terminal, 120, firstPointer);
    expect(identifiableClick(f.handles[0], secondPointer).defaultPrevented).toBe(true);
    expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
    expect(tabPreview(f.slots)).toEqual(f.slots.map(() => ({ dragging: false, transform: '' })));
    expect(f.move).not.toHaveBeenCalled();
  });

  it.each(['identified-B/legacy-A', 'legacy-B/identified-A'])('keeps independent fallback and ticket: %s', async ordering => {
    const f = await gestureFixture('tabs');
    const node = f.handles[0];
    await pointer(node, 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(node, 'pointerdown', 120, secondPointer);
    await pointer(node, 'pointerup', 120, secondPointer);
    await pointer(node, 'pointerup', 120, firstPointer);
    const first = ordering === 'identified-B/legacy-A'
      ? identifiableClick(node, secondPointer) : pointerClick(node);
    expect(first.defaultPrevented).toBe(true);
    const owner = ordering === 'identified-B/legacy-A'
      ? pointerClick(node) : identifiableClick(node, firstPointer);
    expect(owner.defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
    expect(pointerClick(node).defaultPrevented).toBe(false);
    expect(f.onopen).toHaveBeenCalledTimes(1);
    expect(f.move).not.toHaveBeenCalled();
  });

  it('matches ID AND type, not identical click detail', async () => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(f.handles[0], 'pointerup', 120, firstPointer);
    for (const identity of [{ ...firstPointer, pointerType: 'pen' }, { ...firstPointer, pointerId: 99 }]) {
      expect(identifiableClick(f.handles[0], identity).defaultPrevented).toBe(true);
    }
    expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(true);
    expect(f.onopen).not.toHaveBeenCalled();
  });

  const unqualified: ClickIdentity[] = [
    {}, { pointerId: 0, pointerType: 'mouse' }, { pointerId: -1, pointerType: 'mouse' },
    { pointerId: 1.5, pointerType: 'touch' }, { pointerId: 11 }, { pointerType: 'touch' },
    { pointerId: 11, pointerType: '' }, { pointerId: 11, pointerType: 'keyboard' },
    { pointerId: 11, pointerType: 'unknown' },
  ];
  it.each(unqualified)('unqualified click %j consumes only bounded legacy fallback', async identity => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(f.handles[0], 'pointerup', 120, firstPointer);
    expect(identifiableClick(f.handles[0], identity).defaultPrevented).toBe(true);
    expect(identifiableClick(f.handles[0], identity).defaultPrevented).toBe(false);
    expect(f.onopen).toHaveBeenCalledTimes(1);
    expect(identifiableClick(f.handles[0], secondPointer).defaultPrevented).toBe(true);
    expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(true);
    expect(f.onopen).toHaveBeenCalledTimes(1);
    f.handles[0].click();
    expect(f.onopen).toHaveBeenCalledTimes(2);
  });

  it.each([
    { pointerId: 0, pointerType: 'mouse' }, { pointerId: -2, pointerType: 'touch' },
    { pointerId: 11, pointerType: '' }, { pointerId: 11, pointerType: 'keyboard' },
  ])('unqualified lifted down %j does not create an identifiable blacklist', async identity => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, identity);
    await f.hold();
    await pointer(f.handles[0], 'pointerup', 120, identity);
    expect(identifiableClick(f.handles[0], identity).defaultPrevented).toBe(true);
    expect(identifiableClick(f.handles[0], identity).defaultPrevented).toBe(false);
    expect(f.onopen).toHaveBeenCalledTimes(1);
    expect(f.move).not.toHaveBeenCalled();
  });

  it('preserves pending detail0 activation without legacy suppression', async () => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    expect(identifiableClick(f.handles[0], firstPointer, 0).defaultPrevented).toBe(false);
    expect(f.onopen).toHaveBeenCalledTimes(1);
    await pointer(document.body, 'pointercancel', 120, firstPointer);
    expect(f.capture).not.toHaveBeenCalled();
  });

  it('retires before next inactive down eligibility checks, allowing same-ID reuse', async () => {
    const f = await gestureFixture('tabs');
    const node = f.handles[0];
    await pointer(node, 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(node, 'pointerup', 120, firstPointer);
    // Even a down ineligible for a new hold is the established retirement boundary.
    await pointer(node, 'pointerdown', 120, { ...firstPointer, isPrimary: false });
    await pointer(node, 'pointerup', 120, firstPointer);
    expect(identifiableClick(node, firstPointer).defaultPrevented).toBe(false);
    expect(f.onopen).toHaveBeenCalledTimes(1);
    expect(f.capture).toHaveBeenCalledTimes(1);
  });

  it('repeats no-op holds on one component, then ordinary and keyboard-style activation', async () => {
    const f = await gestureFixture('tabs');
    for (let cycle = 0; cycle < 3; cycle++) {
      await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
      await f.hold();
      await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
      await pointer(f.handles[0], 'pointerup', 120, secondPointer);
      await pointer(f.handles[0], 'pointerup', 120, firstPointer);
      expect(identifiableClick(f.handles[0], secondPointer).defaultPrevented).toBe(true);
      expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(true);
      expect(f.onopen).not.toHaveBeenCalled();
    }
    expect(f.capture).toHaveBeenCalledTimes(3);
    expect(f.move).not.toHaveBeenCalled(); // Existing unresolved commit fixture is deliberately not reused for commits.
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await pointer(f.handles[0], 'pointerup', 120, firstPointer);
    expect(identifiableClick(f.handles[0], firstPointer).defaultPrevented).toBe(false);
    f.handles[0].click();
    expect(identifiableClick(f.handles[0], {}, 0).defaultPrevented).toBe(false);
    expect(f.onopen).toHaveBeenCalledTimes(3);
  });

  it('removes local capture click handling on destruction', async () => {
    const f = await gestureFixture('tabs');
    const node = f.handles[0];
    await pointer(node, 'pointerdown', 120, firstPointer);
    await f.hold();
    f.unmount();
    expect(identifiableClick(node, secondPointer).defaultPrevented).toBe(false);
    expect(identifiableClick(node, firstPointer).defaultPrevented).toBe(false);
    expect(f.move).not.toHaveBeenCalled();
  });

  it('explicitly retains two-legacy-click ambiguity, not universal attribution', async () => {
    const f = await gestureFixture('tabs');
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(f.handles[0], 'pointerdown', 120, secondPointer);
    await pointer(f.handles[0], 'pointerup', 120, secondPointer);
    await pointer(f.handles[0], 'pointerup', 120, firstPointer);
    expect(pointerClick(f.handles[0]).defaultPrevented).toBe(true); // Legacy B consumes fallback.
    expect(pointerClick(f.handles[0]).defaultPrevented).toBe(false); // Legacy A is indistinguishable.
    expect(f.onopen).toHaveBeenCalledTimes(1);
    expect(f.move).not.toHaveBeenCalled();
  });
});


// Command admission owns optimistic/busy state, not independent gesture starts.
async function commandAdmissionFixture(kind: 'tabs' | 'workspaces') {
  const f = await gestureFixture(kind);
  f.handles.forEach(handle => { handle.setPointerCapture = f.capture; });
  const pending: { resolve: (result: CommandResult) => void; reject: (error: Error) => void }[] = [];
  f.move.mockImplementation(() => new Promise<CommandResult>((resolve, reject) => pending.push({ resolve, reject })));
  let currentAgents = agents;
  const slots = () => [...f.container.querySelectorAll<HTMLElement>(kind === 'tabs' ? '.workspace-tab' : '.workspace-management-slot')];
  const order = () => slots().map(slot => Number((kind === 'tabs' ? slot.dataset.tabId : slot.dataset.workspaceKey)!.slice(1)));
  const layout = () => slots().forEach((slot, i) => vi.mocked(slot.getBoundingClientRect).mockReturnValue({ top: 100 + i * 60, height: 50 } as DOMRect));
  const key = async (id: number, arrow = 'ArrowUp') => {
    const slot = f.slots[id - 1];
    const handle = slot.querySelector<HTMLElement>(kind === 'tabs' ? '.agent-open' : '.workspace-drag-handle')!;
    handle.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, cancelable: true, altKey: true, key: arrow }));
    await tick();
  };
  const snapshot = async (ids: number[]) => {
    currentAgents = ids.map((id, i) => ({ ...agents[id - 1], tab_order: i + 1 }));
    if (kind === 'tabs') await f.rerender({ agents: currentAgents });
    else relayStore.workspaces.set(ids.map((id, i) => ({ ...workspaces[id - 1], number: i + 1 })));
    await tick();
    layout();
    expect(order()).toEqual(ids);
  };
  const settle = async (index: number, ok: boolean) => {
    if (ok) pending[index].resolve({ type: 'command_result', request_id: `owned-${index}`, ok: true });
    else pending[index].reject(new Error('Selected command refused'));
    await Promise.resolve();
    await tick();
  };
  const args = (id: number, insertIndex: number, before = '') => kind === 'tabs'
    ? [currentAgents.find(agent => agent.tab_id === `t${id}`)!, insertIndex]
    : ['test', [`w${id}`], before, insertIndex];
  return { ...f, pending, order, layout, key, snapshot, settle, args };
}

for (const kind of ['tabs', 'workspaces'] as const) {
  for (const first of ['pointer', 'keyboard'] as const) {
    for (const ok of [true, false]) {
      it(`${kind}: command admission retains first ${first} owner until ${ok ? 'success' : 'rejection'}`, async () => {
        const f = await commandAdmissionFixture(kind);
        await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
        await f.hold();
        await pointer(f.handles[0], 'pointermove', 280, firstPointer);
        expect(f.slots[0].style.transform).toBe('translateY(160px)');
        const firstOrder = first === 'pointer' ? [2, 3, 1] : [2, 1, 3];
        const firstArgs = first === 'pointer' ? f.args(1, 3) : f.args(2, 0, 'w1');
        if (first === 'pointer') {
          // B starts BEFORE command busy, but its timer lifts AFTER A commits.
          await pointer(f.handles[1], 'pointerdown', 180, secondPointer);
          await pointer(f.handles[0], 'pointerup', 280, firstPointer);
          expect(f.move.mock.calls).toEqual([firstArgs]);
          expect(f.order()).toEqual(firstOrder);
          f.layout(); // Real layout moves with A's optimistic order before B measures.
          await f.hold();
          await pointer(f.handles[1], 'pointermove', 280, secondPointer);
          expect(f.slots[1].style.transform).not.toBe('');
          await pointer(f.handles[1], 'pointerup', 280, secondPointer);
        } else {
          // Actual handle/currentTarget; middle item ArrowUp really changes order.
          await f.key(2);
          expect(f.move.mock.calls).toEqual([firstArgs]);
          expect(f.order()).toEqual(firstOrder);
          await pointer(f.handles[0], 'pointerup', 280, firstPointer);
        }
        expect(f.capture.mock.calls).toEqual(first === 'pointer' ? [[firstPointer.pointerId], [secondPointer.pointerId]] : [[firstPointer.pointerId]]);
        expect(f.vibrate.mock.calls).toEqual(first === 'pointer' ? [[12], [12]] : [[12]]);
        expect(f.move.mock.calls).toEqual([firstArgs]);
        expect(f.pending).toHaveLength(1);
        expect(f.order()).toEqual(firstOrder);
        expect(f.slots.every(slot => slot.style.transform === '')).toBe(true);
        expect(f.event('touchmove').defaultPrevented).toBe(false);
        expect(f.event('contextmenu').defaultPrevented).toBe(false);
        await f.key(3); // Still busy: cannot admit an otherwise valid keyboard move.
        expect(f.move.mock.calls).toEqual([firstArgs]);

        await f.settle(0, ok);
        if (!ok) expect(f.order()).toEqual([1, 2, 3]);
        else await f.snapshot(firstOrder);
        await f.snapshot([3, 1, 2]); // Later authoritative snapshot is no longer masked.

        const ordinaryArgs = f.args(3, 3);
        await pointer(f.handles[2], 'pointerdown', 120, nextPointer);
        await f.hold();
        await pointer(f.handles[2], 'pointermove', 280, nextPointer);
        await pointer(f.handles[2], 'pointerup', 280, nextPointer);
        expect(f.move.mock.calls).toEqual([firstArgs, ordinaryArgs]);
        expect(f.order()).toEqual([1, 2, 3]);
        await f.settle(1, true);
        await f.snapshot([1, 2, 3]);

        const keyboardArgs = f.args(2, 0, 'w1');
        await f.key(2);
        expect(f.move.mock.calls).toEqual([firstArgs, ordinaryArgs, keyboardArgs]);
        expect(f.order()).toEqual([2, 1, 3]);
        await f.settle(2, false);
        expect(f.order()).toEqual([1, 2, 3]);
        f.unmount();
        await pointer(f.handles[0], 'pointerup', 280, firstPointer);
        await pointer(f.handles[1], 'pointercancel', 280, secondPointer);
        await f.hold(); // Bounded deadline only, never drain recurring store clocks.
        expect(f.move.mock.calls).toEqual([firstArgs, ordinaryArgs, keyboardArgs]);
      });
    }
  }

  it(`${kind}: command admission allows a still-held completion after first settlement`, async () => {
    const f = await commandAdmissionFixture(kind);
    await pointer(f.handles[0], 'pointerdown', 120, firstPointer);
    await f.hold();
    await pointer(f.handles[0], 'pointermove', 280, firstPointer);
    const keyboardArgs = f.args(2, 0, 'w1');
    await f.key(2);
    expect(f.move.mock.calls).toEqual([keyboardArgs]);
    await f.settle(0, true);
    // Reconcile without changing the held gesture's captured geometry.
    await f.snapshot([2, 1, 3]);
    const heldArgs = f.args(1, 3);
    await pointer(f.handles[0], 'pointerup', 280, firstPointer);
    expect(f.move.mock.calls).toEqual([keyboardArgs, heldArgs]);
    expect(f.order()).toEqual([2, 3, 1]);
    await f.settle(1, true);
    await f.snapshot([2, 3, 1]);
    expect(f.capture.mock.calls).toEqual([[firstPointer.pointerId]]);
    expect(f.vibrate.mock.calls).toEqual([[12]]);
    expect(f.slots.every(slot => slot.style.transform === '')).toBe(true);
    f.unmount();
    await f.hold();
    expect(f.move.mock.calls).toEqual([keyboardArgs, heldArgs]);
  });
}
