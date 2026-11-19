import { promptHint } from './composer-policy';
import { demoComputers, demoEpochKey, minuteMs, sampleDeviceIds } from './scene';

function memoryStorage(initial: Record<string, string> = {}): Storage {
  const entries = new Map(Object.entries(initial));
  return {
    get length() { return entries.size; },
    key: (index) => [...entries.keys()][index >>> 0] ?? null,
    getItem: (key) => entries.get(String(key)) ?? null,
    setItem: (key, value) => { entries.set(String(key), String(value)); },
    removeItem: (key) => { entries.delete(String(key)); },
    clear: () => { entries.clear(); },
  };
}

const epoch = Date.now();
const relays = demoComputers.map(({ id, label }) => ({ id, label, token: '', url: `wss://${id}.invalid` }));
const credentials = Object.fromEntries(demoComputers.map(({ id, role }) => {
  const { deviceId, credentialId } = sampleDeviceIds(id);
  return [id, {
    kind: 'credential', id: credentialId, deviceId, version: 1,
    secret: 'A'.repeat(43), locale: 'en', issuedAt: epoch - 90 * minuteMs, role,
  }];
}));
Object.defineProperties(window, {
  localStorage: { configurable: true, value: memoryStorage({
    herdr_relays: JSON.stringify(relays),
    herdr_device_auth_v1: JSON.stringify({ version: 1, relays: credentials }),
    herdr_theme: 'nord',
    herdr_terminal_font_size: 'compact',
    herdr_home_workspace_layout: 'mixed',
    herdr_speech_enabled: 'false',
    herdr_default_agent_view: 'conversation',
    'herdr-conversation-view': 'activity',
  }) },
  sessionStorage: { configurable: true, value: memoryStorage({ [demoEpochKey]: String(epoch) }) },
  Notification: { configurable: true, value: undefined },
});
Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: undefined });
history.scrollRestoration = 'manual';
history.replaceState(null, '', location.pathname);

const notices = [
  { selector: '.app-header', className: 'demo-label', text: 'Demo — sample data', heading: true },
  { selector: '.app-dialog .dialog-content, .workspace-inspector > header > div:first-child', className: 'demo-label', text: 'Demo — sample data' },
  { selector: '.conversation-input-area', className: 'demo-interaction-hint', text: 'Copy controls and Read Aloud are unavailable in the demo.' },
  { selector: '.prompt-composer:has(.term-input textarea:not(:disabled))', className: 'demo-interaction-hint', text: promptHint },
];
const app = document.getElementById('app')!;
new MutationObserver((records) => {
  for (const { addedNodes } of records) {
    for (const node of addedNodes) {
      if (!(node instanceof Element)) continue;
      for (const { selector, className, text, heading } of notices) {
        const containers = node.matches(selector) ? [node] : node.querySelectorAll(selector);
        for (const container of containers) {
          if (container.querySelector(`:scope > .${className}`)) continue;
          const label = document.createElement('p');
          label.className = className;
          label.textContent = text;
          if (heading) {
            label.setAttribute('role', 'heading');
            label.setAttribute('aria-level', '2');
          }
          container.prepend(label);
        }
      }
    }
  }
}).observe(app, { childList: true, subtree: true });

const appEntry = '/assets/demo.js';
void import(appEntry).then(() => document.getElementById('demo-startup')?.remove(), () => {});
