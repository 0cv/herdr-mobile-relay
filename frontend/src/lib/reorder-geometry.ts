// Slots and pointer positions share document coordinates, captured by callers.
export interface ReorderSlot {
  id: string;
  top: number;
  height: number;
}

export function slotGap(items: ReorderSlot[]): number {
  return items.length > 1 ? Math.max(0, items[1].top - items[0].top - items[0].height) : 12;
}

// Insertion addresses the list with the source removed, not a wire index.
export function insertionSlot(items: ReorderSlot[], sourceId: string, pointerY: number): number {
  let insertIdx = 0;
  for (const item of items) {
    if (item.id === sourceId) continue;
    if (pointerY > item.top + item.height / 2) insertIdx += 1;
  }
  return insertIdx;
}

export function displacedSlot(items: ReorderSlot[], sourceId: string, itemId: string, insertIdx: number, gap: number): string {
  const sourceIdx = items.findIndex((item) => item.id === sourceId);
  const itemIdx = items.findIndex((item) => item.id === itemId);
  if (sourceIdx < 0 || itemIdx < 0) return '';
  const span = items[sourceIdx].height + gap;
  const othersIdx = itemIdx > sourceIdx ? itemIdx - 1 : itemIdx;
  const shift = (itemIdx > sourceIdx ? -span : 0) + (othersIdx >= insertIdx ? span : 0);
  return shift ? `translateY(${shift}px)` : '';
}
