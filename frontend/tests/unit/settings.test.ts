import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import SettingsView from '$components/SettingsView.svelte';
import {
  APP_ASSET_VERSION,
  APP_VERSION,
  PUSH_ENABLED_KEY,
  STATUS_LINE_KEY,
  TERMINAL_HISTORY_KEY,
} from '$lib/config';
import { relayStore } from '$lib/store';
import { appUpdateStatus } from '$lib/updates';

class MockWebSocket {
  static OPEN = 1;
  static instances: MockWebSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  sent: string[] = [];

  constructor(readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(payload: string) { this.sent.push(payload); }
  close() { this.readyState = 3; }
  open() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }
  server(message: unknown) {
    this.onmessage?.({ data: JSON.stringify(message) });
  }
}

describe('settings relay status', () => {
  const serviceWorkerDescriptor = Object.getOwnPropertyDescriptor(navigator, 'serviceWorker');

  beforeEach(() => {
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    vi.stubGlobal('Notification', { permission: 'granted', requestPermission: vi.fn().mockResolvedValue('granted') });
    vi.stubGlobal('PushManager', class {});
    Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: {} });
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    appUpdateStatus.set({
      state: 'current',
      currentVersion: APP_VERSION,
      currentAssets: APP_ASSET_VERSION,
      deployedVersion: APP_VERSION,
      deployedAssets: APP_ASSET_VERSION,
      upstreamVersion: APP_VERSION,
      upstreamAssets: APP_ASSET_VERSION,
      checkedAt: 123,
      error: '',
    });
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: 'secret' });
  });

  afterEach(() => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    vi.unstubAllGlobals();
    if (serviceWorkerDescriptor) Object.defineProperty(navigator, 'serviceWorker', serviceWorkerDescriptor);
    else Reflect.deleteProperty(navigator, 'serviceWorker');
  });

  it('updates connection and push state without remounting settings', async () => {
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    expect(screen.getByRole('img', { name: 'Fedora relay connecting' })).toBeInTheDocument();
    expect(screen.getByText('Push: waiting for relay…')).toBeInTheDocument();

    socket.open();
    relayStore.setPushStatus('fedora-wss-fedora-example', 'sent');
    await waitFor(() => expect(screen.getByRole('img', { name: 'Fedora relay connected' })).toBeInTheDocument());
    expect(screen.getByText('Push: syncing…')).toBeInTheDocument();

    socket.server({ type: 'push_subscribed', ok: true });
    await waitFor(() => expect(screen.getByText('Push: synced')).toBeInTheDocument());
  });

  it('shows the complete one-time update command for an older relay', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.server({
      type: 'push_config',
      protocol: 2,
      release_version: '0.6.0',
      capabilities: [],
      agent_profiles: [],
    });

    await user.click(await screen.findByRole('button', { name: 'How to update Fedora' }));
    const dialog = screen.getByRole('dialog', { name: 'Update Fedora' });
    expect(dialog).toHaveTextContent('Version 0.7.0 is a one-time manual update.');
    expect(within(dialog).getByText(/HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install/)).toHaveTextContent(
      'herdr plugin action invoke install-service --plugin herdr-mobile-relay.events',
    );
    expect(dialog).toHaveTextContent('preserves the configuration used by an existing stable service');
    expect(dialog).toHaveTextContent('Prefer to keep using a source checkout?');
    expect(screen.queryByText(/assets \d+/i)).not.toBeInTheDocument();
    expect(screen.getAllByRole('heading', { level: 3 }).at(-1)).toHaveTextContent('About');
  });

  it('requires confirmation before removing a relay', async () => {
    const user = userEvent.setup();
    render(SettingsView);

    await user.click(screen.getByRole('button', { name: 'Remove Fedora' }));
    const dialog = screen.getByRole('dialog', { name: 'Remove Fedora?' });
    expect(dialog).toHaveTextContent('You will need its setup link or connection details to add it again.');
    expect(screen.getByText('Fedora')).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.getByText('Fedora')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Remove Fedora' }));
    await user.click(within(screen.getByRole('dialog', { name: 'Remove Fedora?' })).getByRole('button', { name: 'Remove Relay' }));
    await waitFor(() => expect(screen.queryByText('Fedora')).not.toBeInTheDocument());
  });

  it('applies interface size from the accessible settings group', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const sizes = within(screen.getByRole('group', { name: 'Interface Size' }));

    await user.click(sizes.getByRole('button', { name: 'Large' }));
    expect(document.documentElement.dataset.interfaceSize).toBe('large');
    expect(localStorage.getItem('herdr_terminal_font_size')).toBe('large');

    await user.click(sizes.getByRole('button', { name: 'Compact' }));
    expect(document.documentElement.dataset.interfaceSize).toBe('compact');

    const statusLine = screen.getByRole('switch', { name: 'Show Agent Status Line' }) as HTMLInputElement;
    const nextStatusLine = !statusLine.checked;
    await user.click(statusLine);
    expect(statusLine.checked).toBe(nextStatusLine);
    expect(localStorage.getItem(STATUS_LINE_KEY)).toBe(String(nextStatusLine));
  });

  it('persists the selected terminal history size', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const history = within(screen.getByRole('group', { name: 'Terminal History' }));

    expect(history.getByRole('button', { name: '1000' })).toHaveAttribute('aria-pressed', 'true');
    await user.click(history.getByRole('button', { name: '10000' }));
    expect(history.getByRole('button', { name: '10000' })).toHaveAttribute('aria-pressed', 'true');
    expect(localStorage.getItem(TERMINAL_HISTORY_KEY)).toBe('10000');

    await user.click(history.getByRole('button', { name: '1000' }));
  });

  it('enables the finished-agent switch immediately after push is enabled', async () => {
    const user = userEvent.setup();
    localStorage.setItem(PUSH_ENABLED_KEY, 'false');
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();
    await waitFor(() => expect(screen.getByRole('img', { name: 'Fedora relay connected' })).toBeInTheDocument());
    socket.server({ type: 'push_config', protocol: 2, version: 'abc1234', vapid_public_key: 'test-key' });
    const finished = screen.getByRole('switch', { name: 'Notify When Agents Finish' });

    expect(finished).toHaveAttribute('type', 'checkbox');
    expect(finished).toBeDisabled();
    await user.click(await screen.findByRole('button', { name: 'Enable Push Notifications' }));

    await waitFor(() => expect(finished).toBeEnabled());
  });

  it('confirms an available relay update before sending the exact target', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.server({
      type: 'push_config',
      protocol: 2,
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'available',
        current_version: '0.7.0',
        current_revision: 'abc123',
        available_version: '0.8.0',
        available_revision: 'f'.repeat(12),
        target_revision: 'f'.repeat(40),
        can_install: true,
        mode: 'local',
      },
    });

    await user.click(await screen.findByRole('button', { name: 'Update Fedora to version 0.8.0' }));
    expect(screen.getByRole('dialog', { name: 'Update Relay' })).toBeInTheDocument();
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Update Relay' }));
    const command = socket.sent.map((payload) => JSON.parse(payload))
      .find((message) => message.type === 'install_update');

    expect(command).toMatchObject({
      expected_version: '0.8.0',
      expected_revision: 'f'.repeat(40),
    });
    socket.server({
      type: 'command_result',
      request_id: command.request_id,
      ok: true,
      phase: 'scheduled',
      data: { update: { state: 'scheduled', target_revision: 'f'.repeat(40) } },
    });
  });

  it('confirms a separate app deployment through its authorized relay', async () => {
    const user = userEvent.setup();
    appUpdateStatus.set({
      state: 'deployment-required',
      currentVersion: APP_VERSION,
      currentAssets: APP_ASSET_VERSION,
      deployedVersion: APP_VERSION,
      deployedAssets: APP_ASSET_VERSION,
      upstreamVersion: '9.0.0',
      upstreamAssets: 999,
      checkedAt: 123,
      error: '',
    });
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.server({
      type: 'push_config',
      protocol: 2,
      release_version: '9.0.0',
      revision: 'abc123',
      capabilities: ['self_update', 'app_deploy'],
      agent_profiles: [],
      app_deploy: {
        configured: true,
        origin: location.origin,
        project: 'herdr-app',
        branch: 'main',
        revision: 'f'.repeat(40),
        state: 'idle',
      },
    });

    await user.click(await screen.findByRole('button', { name: 'Deploy App' }));
    const dialog = screen.getByRole('dialog', { name: 'Deploy Phone App' });
    expect(dialog).toHaveTextContent(`Deploy app version 9.0.0 from Fedora to ${location.origin}?`);
    await user.click(within(dialog).getByRole('button', { name: 'Deploy App' }));
    const command = socket.sent.map((payload) => JSON.parse(payload))
      .find((message) => message.type === 'deploy_app_update');

    expect(command).toMatchObject({
      expected_version: '9.0.0',
      expected_revision: 'f'.repeat(40),
      expected_origin: location.origin,
    });
  });
});
