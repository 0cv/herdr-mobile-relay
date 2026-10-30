import { setPendingPointerObservers } from './pending-pointer-observers';

const LONG_PRESS_MS = 550;
const PRESS_SLOP_PX = 12;

// Shared pending-hold and lift mechanics; callers retain eligibility and drag commits.
export function createReorderHold(node: HTMLElement) {
  let timer = 0;
  let pointerId = -1;
  let startX = 0;
  let startY = 0;
  let dragging = false;

  function onPendingPointer(event: Event) {
    const pointer = event as PointerEvent;
    if (pointer.pointerId !== pointerId) return;
    if (event.type !== 'pointermove'
      || Math.hypot(pointer.clientX - startX, pointer.clientY - startY) > PRESS_SLOP_PX) reset();
  }

  function reset() {
    setPendingPointerObservers(node.ownerDocument, onPendingPointer, false);
    if (timer) clearTimeout(timer);
    timer = 0;
    pointerId = -1;
    dragging = false;
  }

  // Svelte's touchmove handlers are passive; prevent scrolling only after lift.
  function onTouchMove(event: TouchEvent) {
    if (dragging) event.preventDefault();
  }

  function onContextMenu(event: Event) {
    if (dragging || timer) event.preventDefault();
  }

  node.addEventListener('touchmove', onTouchMove, { passive: false });
  node.addEventListener('contextmenu', onContextMenu);
  return {
    get pointerId() { return pointerId; },
    get startY() { return startY; },
    listen(onPointerDown: (event: PointerEvent) => void, callbacks: {
      move: (event: PointerEvent) => void;
      finish: (event: PointerEvent) => void;
      cancel: () => void;
    }) {
      function onPointerMove(event: PointerEvent) {
        if (event.pointerId !== pointerId) return;
        if (!dragging) return;
        event.preventDefault();
        callbacks.move(event);
      }

      function onPointerUp(event: PointerEvent) {
        if (event.pointerId !== pointerId) return;
        if (dragging) callbacks.finish(event);
        reset();
      }

      function onPointerCancel(event: PointerEvent) {
        if (event.pointerId !== pointerId) return;
        callbacks.cancel();
        reset();
      }

      node.addEventListener('pointerdown', onPointerDown);
      node.addEventListener('pointermove', onPointerMove);
      node.addEventListener('pointerup', onPointerUp);
      node.addEventListener('pointercancel', onPointerCancel);
      return () => {
        node.removeEventListener('pointerdown', onPointerDown);
        node.removeEventListener('pointermove', onPointerMove);
        node.removeEventListener('pointerup', onPointerUp);
        node.removeEventListener('pointercancel', onPointerCancel);
      };
    },
    start(event: PointerEvent, onHold: () => void) {
      if (pointerId !== -1) return;
      pointerId = event.pointerId;
      startX = event.clientX;
      startY = event.clientY;
      setPendingPointerObservers(node.ownerDocument, onPendingPointer, true);
      timer = window.setTimeout(() => {
        setPendingPointerObservers(node.ownerDocument, onPendingPointer, false);
        timer = 0;
        onHold();
      }, LONG_PRESS_MS);
    },
    lift(onLift?: () => void) {
      dragging = true;
      onLift?.();
      navigator.vibrate?.(12);
      node.setPointerCapture?.(pointerId);
    },
    // Cancel effects now, but keep ownership until the original pointer ends anywhere.
    cancel() {
      if (pointerId === -1) return;
      const owner = pointerId;
      reset();
      pointerId = owner;
      node.ownerDocument.addEventListener('pointerup', onPendingPointer, true);
      node.ownerDocument.addEventListener('pointercancel', onPendingPointer, true);
    },
    reset,
    destroy() {
      node.removeEventListener('touchmove', onTouchMove);
      node.removeEventListener('contextmenu', onContextMenu);
      reset();
    },
  };
}
