import { describe, expect, it, vi } from 'vitest';
import { setPendingPointerObservers } from '$lib/pending-pointer-observers';

const types = ['pointermove', 'pointerup', 'pointercancel'];

describe('pending pointer observers', () => {
  it('uses the supplied Document receiver, ordered names, exact callback and capture on add/remove', () => {
    const owner = document.implementation.createHTMLDocument();
    const callback = vi.fn();
    const add = vi.spyOn(owner, 'addEventListener');
    const remove = vi.spyOn(owner, 'removeEventListener');
    setPendingPointerObservers(owner, callback, true);
    setPendingPointerObservers(owner, callback, false);
    for (const method of [add, remove]) {
      expect(method.mock.calls).toEqual(types.map(type => [type, callback, true]));
      expect(method.mock.contexts).toEqual([owner, owner, owner]);
    }
    expect(callback).not.toHaveBeenCalled();
    for (const type of types) {
      owner.dispatchEvent(new Event(type));
      document.dispatchEvent(new Event(type));
    }
    expect(callback).not.toHaveBeenCalled();
  });

  it('keeps callbacks independent and repeated add/remove idempotent without invoking on removal', () => {
    const owner = document.implementation.createHTMLDocument();
    const first = vi.fn();
    const second = vi.fn();
    setPendingPointerObservers(owner, first, true);
    setPendingPointerObservers(owner, first, true);
    setPendingPointerObservers(owner, second, true);
    for (const type of types) document.dispatchEvent(new Event(type));
    expect(first).not.toHaveBeenCalled();
    expect(second).not.toHaveBeenCalled();
    for (const type of types) owner.dispatchEvent(new Event(type));
    expect(first).toHaveBeenCalledTimes(3);
    expect(second).toHaveBeenCalledTimes(3);
    setPendingPointerObservers(owner, first, false);
    setPendingPointerObservers(owner, first, false);
    expect(first).toHaveBeenCalledTimes(3);
    expect(second).toHaveBeenCalledTimes(3);
    for (const type of types) owner.dispatchEvent(new Event(type));
    expect(first).toHaveBeenCalledTimes(3);
    expect(second).toHaveBeenCalledTimes(6);
    setPendingPointerObservers(owner, second, false);
    for (const type of types) owner.dispatchEvent(new Event(type));
    expect(second).toHaveBeenCalledTimes(6);
  });
});
