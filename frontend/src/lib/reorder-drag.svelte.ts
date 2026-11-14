import { displacedSlot, insertionSlot, slotGap, type ReorderSlot } from './reorder-geometry';
import type { createReorderHold } from './reorder-hold';

const EDGE_SCROLL_ZONE_PX = 72;
const EDGE_SCROLL_STEP_PX = 12;

interface ReorderDrag {
  owner: HTMLElement;
  scope: string;
  sourceId: string;
  pointerId: number;
  startY: number;
  deltaY: number;
  insertIdx: number;
  items: ReorderSlot[];
  gap: number;
}

// Slots are captured in document coordinates when the drag starts, so the
// preview stays correct while the page auto-scrolls under the pointer.
export function measureReorderSlots(node: HTMLElement, containerSelector: string, slotSelector: string, idKey: string): ReorderSlot[] {
  const container = node.closest(containerSelector);
  if (!container) return [];
  return [...container.querySelectorAll<HTMLElement>(slotSelector)].map((slot) => {
    const bounds = slot.getBoundingClientRect();
    return { id: slot.dataset[idKey] || '', top: bounds.top + window.scrollY, height: bounds.height };
  });
}

// Drag state shared by the reorder lists; `scope` separates lists that share one instance.
export function createReorderDrag() {
  let drag = $state<ReorderDrag | null>(null);

  return {
    isSource(scope: string, id: string): boolean {
      return drag?.scope === scope && drag.sourceId === id;
    },
    begin(owner: HTMLElement, hold: ReturnType<typeof createReorderHold>, scope: string, sourceId: string, items: ReorderSlot[]) {
      const sourceIdx = items.findIndex((item) => item.id === sourceId);
      drag = {
        owner,
        scope,
        sourceId,
        pointerId: hold.pointerId,
        startY: hold.startY + window.scrollY,
        deltaY: 0,
        insertIdx: Math.max(0, sourceIdx),
        items,
        gap: slotGap(items),
      };
    },
    clear(owner: HTMLElement) {
      if (drag?.owner === owner) drag = null;
    },
    track(event: PointerEvent, scope: string) {
      if (!drag || drag.pointerId !== event.pointerId || drag.scope !== scope) return;
      if (event.clientY < EDGE_SCROLL_ZONE_PX) window.scrollBy(0, -EDGE_SCROLL_STEP_PX);
      else if (event.clientY > window.innerHeight - EDGE_SCROLL_ZONE_PX) window.scrollBy(0, EDGE_SCROLL_STEP_PX);
      const pointerY = event.clientY + window.scrollY;
      const insertIdx = insertionSlot(drag.items, drag.sourceId, pointerY);
      drag = { ...drag, deltaY: pointerY - drag.startY, insertIdx };
    },
    // The dragged item follows the pointer; every other item translates by the
    // dragged item's height to preview the drop slot.
    shift(scope: string, id: string): string {
      if (!drag || drag.scope !== scope) return '';
      if (id === drag.sourceId) return `translateY(${drag.deltaY}px)`;
      return displacedSlot(drag.items, drag.sourceId, id, drag.insertIdx, drag.gap);
    },
    // Returns the finished drag only when the item landed in a different slot.
    finish(event: PointerEvent, scope: string): { sourceId: string; insertIdx: number; items: ReorderSlot[] } | null {
      if (!drag || drag.pointerId !== event.pointerId || drag.scope !== scope) return null;
      const completed = drag;
      drag = null;
      const sourceIdx = completed.items.findIndex((item) => item.id === completed.sourceId);
      if (sourceIdx < 0 || completed.insertIdx === sourceIdx) return null;
      return completed;
    },
  };
}
