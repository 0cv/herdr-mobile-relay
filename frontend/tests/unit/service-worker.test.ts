import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import { describe, expect, it, vi } from 'vitest';

const source = readFileSync('public/sw.js', 'utf8');
const key = {
  device_id: 'device-a', server_session_id: 'primary', pane_id: 'pane-1',
  terminal_id: 'terminal-1', agent_session_id: 'session-1', generation: 1,
  event_id: 'event-1', interaction_revision: 2, category: 'attention',
};

function worker() {
  const handlers = new Map<string, (event: any) => void>();
  const showNotification = vi.fn().mockResolvedValue(undefined);
  const postMessage = vi.fn();
  const focus = vi.fn().mockResolvedValue({});
  const openWindow = vi.fn();
  runInNewContext(source, {
    URL,
    importScripts: vi.fn(),
    HERDR_NOTIFICATION_ICON: '/icon.png',
    HERDR_NOTIFICATION_BADGE: '/badge.png',
    self: {
      addEventListener: (name: string, handler: (event: any) => void) => handlers.set(name, handler),
      location: { origin: 'https://app.example' },
      registration: { getNotifications: vi.fn().mockResolvedValue([]), showNotification },
      clients: {
        matchAll: vi.fn().mockResolvedValue([{ url: 'https://app.example/', postMessage, focus }]),
        openWindow,
      },
    },
  });
  return { handlers, showNotification, postMessage, focus, openWindow };
}

describe('service worker notifications', () => {
  it('ignores payloads without a current event key', () => {
    const { handlers, showNotification } = worker();
    for (const payload of [
      { title: 'Blocked', body: 'Needs approval', url: './#notify=old', actions: [], action_urls: {} },
      { v: 1, title: 'Blocked', key: { ...key, terminal_id: '' } },
    ]) {
      const waitUntil = vi.fn();
      handlers.get('push')!({ data: { json: () => payload }, waitUntil });
      expect(waitUntil).not.toHaveBeenCalled();
    }
    expect(showNotification).not.toHaveBeenCalled();
  });

  it('shows a current notification and routes its click to the page', async () => {
    const { handlers, showNotification, postMessage, focus, openWindow } = worker();
    let pending = Promise.resolve();
    const waitUntil = (promise: Promise<void>) => { pending = promise; };
    handlers.get('push')!({
      data: { json: () => ({ v: 1, key, title: 'Herdr notification', body: 'Open the app', event_ref: 'signed-ref', url: './' }) },
      waitUntil,
    });
    await pending;
    expect(showNotification).toHaveBeenCalledOnce();
    const [title, options] = showNotification.mock.calls[0];
    expect(title).toBe('Herdr notification');
    expect(options.data).toMatchObject({ key, eventRef: 'signed-ref' });
    const close = vi.fn();
    handlers.get('notificationclick')!({ notification: { data: options.data, close }, action: '', waitUntil });
    await pending;
    expect(close).toHaveBeenCalledOnce();
    expect(postMessage).toHaveBeenCalledWith(expect.objectContaining({
      type: 'herdr_notification_click', key, event_ref: 'signed-ref', mode: 'thread', action: null,
    }));
    expect(new URL(postMessage.mock.calls[0][0].url).hash).toBe('#push=signed-ref&device=device-a');
    expect(focus).toHaveBeenCalledOnce();
    expect(openWindow).not.toHaveBeenCalled();
  });

  it('does not route a click without a current event key', () => {
    const { handlers, postMessage, openWindow } = worker();
    const waitUntil = vi.fn();
    handlers.get('notificationclick')!({
      notification: { close: vi.fn(), data: { legacyURL: 'https://app.example/#notify=old' } },
      action: '', waitUntil,
    });
    expect(waitUntil).not.toHaveBeenCalled();
    expect(postMessage).not.toHaveBeenCalled();
    expect(openWindow).not.toHaveBeenCalled();
  });
});
