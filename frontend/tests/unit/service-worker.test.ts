import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import { describe, expect, it, vi } from 'vitest';

const source = readFileSync('public/sw.js', 'utf8');
const key = {
  device_id: 'device-a', server_session_id: 'primary', pane_id: 'pane-1',
  terminal_id: 'terminal-1', agent_session_id: 'session-1', generation: 1,
  event_id: 'event-1', interaction_revision: 2, category: 'attention',
};

function worker(scope = 'https://app.example/push/relay-a/') {
  const handlers = new Map<string, (event: any) => void>();
  type Displayed = NotificationOptions & { title: string; close: () => void; tag: string };
  const visible = new Map<string, Displayed>();
  const showNotification = vi.fn(async (title: string, options: NotificationOptions) => {
    const tag = options.tag!;
    const notification = {
      ...options, title, tag,
      close: () => { if (visible.get(tag) === notification) visible.delete(tag); },
    };
    visible.set(tag, notification);
  });
  const getNotifications = vi.fn(async ({tag}: { tag?: string } = {}) =>
    [...visible.values()].filter(notification => !tag || tag === notification.tag));
  const fetch = vi.fn();
  const postMessage = vi.fn();
  const focus = vi.fn().mockResolvedValue({});
  const openWindow = vi.fn();
  runInNewContext(source, {
    URL,
    fetch,
    importScripts: vi.fn(),
    HERDR_NOTIFICATION_ICON: '/icon.png',
    HERDR_NOTIFICATION_BADGE: '/badge.png',
    self: {
      addEventListener: (name: string, handler: (event: any) => void) => handlers.set(name, handler),
      location: { origin: 'https://app.example' },
      registration: { scope, getNotifications, showNotification },
      clients: {
        matchAll: vi.fn().mockResolvedValue([{ url: 'https://app.example/', postMessage, focus }]),
        openWindow,
      },
    },
  });
  function dispatch(event: string, details: object) {
    let pending = Promise.resolve();
    handlers.get(event)!({ ...details, waitUntil: (promise: Promise<void>) => { pending = promise; } });
    return pending;
  }
  return {
    handlers, showNotification, postMessage, focus, openWindow, visible, getNotifications, fetch,
    push: (payload: object) => dispatch('push', { data: { json: () => ({ v: 1, ...payload }) } }),
    click: (notification: Displayed) => dispatch('notificationclick', { notification, action: '' }),
  };
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

  it('displays only the newest waiting alert and preserves its click after late pushes', async () => {
    const phone = worker();
    const old = { key, title: 'Approval A', body: 'Review A', event_ref: 'signed-a', waiting_seq: '9007199254740992' };
    const current = {
      key: { ...key, event_id: 'new-event', interaction_revision: 0, category: 'question' },
      title: 'Question B', body: 'Review B', event_ref: 'signed-b', waiting_seq: '9007199254740993',
    };
    const visible = () => [...phone.visible.values()];
    await phone.push(old);
    expect(visible().map(({title}) => title)).toEqual(['Approval A']);

    await phone.push(current);
    expect(visible().map(({title}) => title)).toEqual(['Question B']);
    await phone.push(old);
    expect(visible().map(({title}) => title)).toEqual(['Question B']);
    expect(phone.showNotification).toHaveBeenLastCalledWith('Question B', expect.objectContaining({
      body: current.body, renotify: false,
      data: expect.objectContaining({ key: current.key, eventRef: current.event_ref }),
    }));
    await phone.push({ ...old, retract: true });
    expect(visible().map(({title}) => title)).toEqual(['Question B']);
    await phone.click(visible()[0]);
    expect(phone.postMessage).toHaveBeenCalledWith(expect.objectContaining({
      key: current.key, event_ref: current.event_ref, action: null,
    }));
    expect(new URL(phone.postMessage.mock.calls[0][0].url).hash).toBe('#push=signed-b&device=device-a');

    await phone.push(current);
    await phone.push({ ...current, retract: true });
    expect(visible()).toHaveLength(0);
    await phone.push(old);
    await phone.push({ ...current, retract: true });
    expect(visible()).toHaveLength(0);
    expect(phone.fetch).not.toHaveBeenCalled();
  });

  it('serializes push handling without touching other panes, relays or finished notices', async () => {
    const phone = worker();
    const otherRelay = worker('https://app.example/push/relay-b/');
    const old = { key, title: 'Old', waiting_seq: '1' };
    const current = { key: { ...key, event_id: 'event-2' }, title: 'Current', waiting_seq: '2' };
    await otherRelay.push({ ...old, title: 'Other relay' });
    await phone.push({ ...old, key: { ...key, pane_id: 'pane-2' }, title: 'Other pane' });
    await phone.push({ key: { ...key, category: 'finished' }, title: 'Finished' });
    await phone.push({ ...old, title: 'Exact-key alert', waiting_seq: undefined });
    phone.getNotifications.mockClear();

    let release!: () => void;
    const waiting = new Promise<void>(resolve => { release = resolve; });
    const [prior] = [...phone.visible.values()].filter(({title}) => title === 'Exact-key alert');
    phone.getNotifications.mockImplementationOnce(async () => {
      await waiting;
      return [prior];
    });
    const pending = phone.push(old);
    const replacement = phone.push(current);
    const lateRetract = phone.push({ ...old, retract: true });
    await Promise.resolve();
    expect(phone.getNotifications).toHaveBeenCalledTimes(1);
    release();
    await Promise.all([pending, replacement, lateRetract]);

    expect([...phone.visible.values()].map(({title}) => title).sort()).toEqual(['Current', 'Finished', 'Other pane']);
    expect([...otherRelay.visible.values()].map(({title}) => title)).toEqual(['Other relay']);
    const displayed = [...phone.visible.values()].find(({title}) => title === 'Current')!;
    expect(otherRelay.visible.has(displayed.tag)).toBe(false);
    await phone.push({ ...current, retract: true });
    expect([...phone.visible.values()].map(({title}) => title)).toEqual(['Other pane', 'Finished']);
    expect(phone.fetch).not.toHaveBeenCalled();
  });

  it('recovers from failed display and keeps ordinary pushes on their exact key', async () => {
    const phone = worker();
    const second = { ...key, interaction_revision: key.interaction_revision + 1 };
    phone.showNotification.mockRejectedValueOnce(new Error('display failed'));
    await expect(phone.push({ key, title: 'Failed', waiting_seq: '1' })).rejects.toThrow('display failed');
    await phone.push({ key, title: 'Exact A' });
    await phone.push({ key: second, title: 'Exact B' });
    await phone.push({ key, retract: true });
    expect([...phone.visible.values()].map(({title}) => title)).toEqual(['Exact B']);
    expect(phone.fetch).not.toHaveBeenCalled();
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
