/**
 * Phones have no console. An uncaught exception on the phone previously
 * vanished, leaving symptoms like a button that "does nothing" with no way to
 * see why. This banner is raw DOM on purpose: when the crash is inside the
 * reactive runtime, any store-driven toast can be wedged along with the app.
 */
const seen = new Set<string>();
let banner: HTMLElement | null = null;

function show(message: string): void {
  const text = message.trim().slice(0, 300);
  if (!text || seen.has(text) || seen.size >= 3) return;
  if (text.includes('ResizeObserver loop')) return;
  seen.add(text);
  try {
    if (!banner) {
      banner = document.createElement('div');
      banner.setAttribute('role', 'alert');
      banner.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:2147483647;'
        + 'background:#7f1d1d;color:#fff;font:12px/1.4 monospace;padding:.5rem .75rem;'
        + 'white-space:pre-wrap;word-break:break-word;';
      banner.addEventListener('click', () => banner?.remove());
      document.body.append(banner);
    }
    banner.textContent = `${banner.textContent ? `${banner.textContent}\n` : 'App error (tap to dismiss)\n'}${text}`;
  } catch {
    // Reporting must never take the app down further.
  }
}

export function reportUncaughtErrors(): void {
  window.addEventListener('error', (event) => {
    show(String(event.error?.stack || event.message || event.error || 'Unknown error'));
  });
  window.addEventListener('unhandledrejection', (event) => {
    const reason = event.reason as { stack?: string } | undefined;
    show(`Unhandled rejection: ${String(reason?.stack || reason || 'unknown')}`);
  });
}
