import { describe, expect, it } from 'vitest';
import { displacedSlot, insertionSlot, slotGap } from '$lib/reorder-geometry';

const items = [
  { id: 'a', top: 400, height: 40 },
  { id: 'b', top: 450, height: 70 },
  { id: 'c', top: 540, height: 30 },
];

describe('pure vertical reorder geometry', () => {
  it('uses the first gap, clamped at zero, and the single/empty fallback', () => {
    expect(slotGap(items)).toBe(10);
    expect(slotGap([])).toBe(12);
    expect(slotGap(items.slice(0, 1))).toBe(12);
    expect(slotGap([items[1], items[0]])).toBe(0);
  });

  it('uses strict document-coordinate midpoints and excludes the source', () => {
    expect(insertionSlot(items, 'a', 485)).toBe(0);
    expect(insertionSlot(items, 'a', 486)).toBe(1);
    expect(insertionSlot(items, 'a', 555)).toBe(1);
    expect(insertionSlot(items, 'a', 556)).toBe(2);
    expect(insertionSlot(items, 'c', 419)).toBe(0);
    expect(insertionSlot(items, 'missing', 556)).toBe(3);
    expect(insertionSlot([], 'a', 556)).toBe(0);
    expect(insertionSlot([...items, items[0]], 'a', 556)).toBe(2);
  });

  it('displaces by source height plus gap, not destination height', () => {
    expect(displacedSlot(items, 'a', 'b', 2, 10)).toBe('translateY(-50px)');
    expect(displacedSlot(items, 'a', 'c', 1, 10)).toBe('');
    expect(displacedSlot(items, 'c', 'a', 0, 10)).toBe('translateY(40px)');
    expect(displacedSlot(items, 'b', 'c', 2, 10)).toBe('translateY(-80px)');
    expect(displacedSlot(items, 'a', 'b', 0, 10)).toBe('');
    expect(displacedSlot(items, 'missing', 'b', 0, 10)).toBe('');
    expect(displacedSlot(items, 'a', 'missing', 2, 10)).toBe('');
    expect(displacedSlot([{ id: 'a', top: 0, height: 0 }, items[1]], 'a', 'b', 1, 0)).toBe('');
  });
});
