export function setPendingPointerObservers(ownerDocument: Document, callback: EventListener, add: boolean): void {
  for (const type of ['pointermove', 'pointerup', 'pointercancel']) {
    ownerDocument[add ? 'addEventListener' : 'removeEventListener'](type, callback, true);
  }
}
