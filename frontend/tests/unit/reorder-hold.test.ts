import { afterEach, describe, expect, it, vi } from 'vitest';
import { createReorderHold } from '$lib/reorder-hold';

function pointer(node: HTMLElement, type: string, pointerId: number) {
  const event = new MouseEvent(type, { bubbles: true, cancelable: true });
  Object.defineProperty(event, 'pointerId', { value: pointerId });
  node.dispatchEvent(event);
  return event;
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('reorder hold dispatch contract', () => {
  it('filters and gates dispatch, prevents before move, finishes before reset, and removes the same listeners', () => {
    vi.useFakeTimers();
    const owner = document.implementation.createHTMLDocument();
    const node = owner.createElement('div');
    owner.body.append(node);
    const hold = createReorderHold(node);
    const add = vi.spyOn(node, 'addEventListener');
    const remove = vi.spyOn(node, 'removeEventListener');
    const move = vi.fn((event: PointerEvent) => {
      expect(event.defaultPrevented).toBe(true);
      expect(hold.pointerId).toBe(11);
    });
    const finish = vi.fn((event: PointerEvent) => {
      expect(event.pointerId).toBe(11);
      expect(hold.pointerId).toBe(11);
    });
    const cancel = vi.fn();
    const down = (event: PointerEvent) => hold.start(event, () => hold.lift());
    const stopListening = hold.listen(down, { move, finish, cancel });
    const listeners = add.mock.calls;
    expect(listeners.map(([type]) => type)).toEqual(['pointerdown', 'pointermove', 'pointerup', 'pointercancel']);

    pointer(node, 'pointerdown', 11);
    expect(pointer(node, 'pointermove', 11).defaultPrevented).toBe(false);
    pointer(node, 'pointerup', 12);
    pointer(node, 'pointercancel', 12);
    expect(move).not.toHaveBeenCalled();
    expect(finish).not.toHaveBeenCalled();
    expect(cancel).not.toHaveBeenCalled();
    vi.advanceTimersByTime(550);
    expect(pointer(node, 'pointermove', 12).defaultPrevented).toBe(false);
    expect(pointer(node, 'pointermove', 11).defaultPrevented).toBe(true);
    expect(move).toHaveBeenCalledTimes(1);
    pointer(node, 'pointerup', 12);
    expect(finish).not.toHaveBeenCalled();
    pointer(node, 'pointerup', 11);
    expect(finish).toHaveBeenCalledTimes(1);
    expect(hold.pointerId).toBe(-1);
    pointer(node, 'pointerup', 11);
    expect(finish).toHaveBeenCalledTimes(1);

    stopListening();
    expect(remove.mock.calls).toEqual(listeners);
    pointer(node, 'pointerdown', 12);
    vi.advanceTimersByTime(550);
    expect(hold.pointerId).toBe(-1);
    hold.destroy();
  });

  it('cancels the matching active owner before reset and clears an invalidated owner during document capture', () => {
    vi.useFakeTimers();
    const owner = document.implementation.createHTMLDocument();
    const node = owner.createElement('div');
    owner.body.append(node);
    const hold = createReorderHold(node);
    const finish = vi.fn();
    const cancel = vi.fn(() => expect(hold.pointerId).toBe(11));
    const stopListening = hold.listen(
      event => hold.start(event, () => hold.lift()),
      { move: vi.fn(), finish, cancel },
    );
    pointer(node, 'pointerdown', 11);
    vi.advanceTimersByTime(550);
    pointer(node, 'pointercancel', 12);
    expect(cancel).not.toHaveBeenCalled();
    pointer(node, 'pointercancel', 11);
    expect(cancel).toHaveBeenCalledTimes(1);
    expect(hold.pointerId).toBe(-1);
    expect(finish).not.toHaveBeenCalled();

    pointer(node, 'pointerdown', 12);
    vi.advanceTimersByTime(550);
    hold.cancel();
    const observed: number[] = [];
    node.addEventListener('pointerup', () => observed.push(hold.pointerId));
    expect(pointer(node, 'pointermove', 12).defaultPrevented).toBe(false);
    pointer(node, 'pointerup', 11);
    expect(hold.pointerId).toBe(12);
    pointer(node, 'pointerup', 12);
    expect(observed).toEqual([12, -1]);
    expect(hold.pointerId).toBe(-1);
    expect(cancel).toHaveBeenCalledTimes(1);
    expect(finish).not.toHaveBeenCalled();
    stopListening();
    hold.destroy();
  });
});
