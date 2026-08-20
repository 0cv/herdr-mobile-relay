import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import SettingsView from '$components/SettingsView.svelte';
import {
  APP_ASSET_VERSION,
  APP_VERSION,
  PUSH_ENABLED_KEY,
  TERMINAL_HISTORY_KEY,
  TERMINAL_REFRESH_KEY,
} from '$lib/config';
import { relayStore } from '$lib/store';
import type { RelayTransport, TransportHandlers, TransportStatus, TransportStatusDetail } from '$lib/transports';
import type { RelayConfig } from '$lib/types';
import { appUpdateStatus, MANAGED_UPDATE_COMMAND } from '$lib/updates';

type TransportFactory = (relay: RelayConfig, handlers: TransportHandlers) => RelayTransport;

/** Lets one test drive a relayed or direct path the mock socket cannot produce. */
const transportHijack = vi.hoisted(() => ({ current: null as TransportFactory | null }));

vi.mock('$lib/transports', async (importOriginal) => {
  const actual = await importOriginal() as { createRelayTransport: TransportFactory };
  return {
    ...actual,
    createRelayTransport: (relay: RelayConfig, handlers: TransportHandlers) =>
      (transportHijack.current ?? actual.createRelayTransport)(relay, handlers),
  };
});

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
    transportHijack.current = null;
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
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: '' });
  });

  afterEach(() => {
    transportHijack.current = null;
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

  it('shows every potential gateway in priority order', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({
      label: 'Fedora',
      url: '',
      token: 'relay-secret',
      transport: 'hybrid',
      gatewayUrl: 'wss://own.example.test',
      gatewayUrls: [
        'wss://own.example.test',
        'wss://community-a.example.test',
        'wss://community-b.example.test',
      ],
    });

    render(SettingsView);

    const candidates = screen.getByRole('list', { name: 'Gateway candidates for Fedora' });
    expect(within(candidates).getAllByRole('listitem').map((item) => item.textContent)).toEqual([
      'wss://own.example.test',
      'wss://community-a.example.test',
      'wss://community-b.example.test',
    ]);
  });

  it('names the gateway carrying each relay, and the direct path that replaces it', async () => {
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    let deliver: (message: Record<string, unknown>) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        report = handlers.onStatus;
        deliver = handlers.onMessage;
        handlers.onStatus('connecting');
      },
      send: () => true,
      close: () => {},
    });
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({
      label: 'Fedora',
      url: '',
      token: 'relay-secret',
      transport: 'hybrid',
      gatewayUrl: 'wss://own.example.test',
      gatewayUrls: ['wss://own.example.test', 'wss://community-a.example.test'],
    });
    render(SettingsView);

    // The configured head was skipped, so the list order is not the answer to
    // "which gateway am I on": the live session names itself.
    report('connected', { path: 'gateway', gatewayUrl: 'wss://community-a.example.test' });
    deliver({ type: 'push_config', protocol: 2, capabilities: [], agent_profiles: [] });

    expect(await screen.findByText('Connection: gateway community-a.example.test')).toBeInTheDocument();

    report('connected', { path: 'webrtc', gatewayUrl: 'wss://community-a.example.test' });
    expect(await screen.findByText('Connection: direct, via community-a.example.test')).toBeInTheDocument();
  });

  it('names the relay URL when no gateway carries the connection', async () => {
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();

    expect(await screen.findByText('Connection: relay URL fedora.example')).toBeInTheDocument();
  });

  it('shows active and latest gateway versions without regressing to an older upstream', async () => {
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.server({
      type: 'push_config',
      protocol: 2,
      release_version: '0.15.0',
      capabilities: [],
      agent_profiles: [],
      update: { state: 'available', available_version: '0.16.0', upstream_version: '0.16.0' },
      hybrid: {
        gateway_url: 'wss://own.example.test',
        gateway_urls: ['wss://own.example.test'],
        gateway_version: '0.15.0',
        gateway_revision: 'gateway-revision',
        gateway_available_version: '0.16.0',
      },
    });

    expect(await screen.findByText('Gateway: 0.15.0 · Latest: 0.16.0')).toBeInTheDocument();
    socket.server({
      type: 'update_status',
      update: { state: 'current', upstream_version: '0.14.0' },
    });
    expect(await screen.findByText('Gateway: 0.15.0 · Latest: 0.16.0')).toBeInTheDocument();
    socket.server({
      type: 'push_config',
      protocol: 2,
      release_version: '0.17.0',
      capabilities: [],
      agent_profiles: [],
      update: { state: 'current', upstream_version: '0.16.0' },
      hybrid: {
        gateway_url: 'wss://own.example.test',
        gateway_urls: ['wss://own.example.test'],
        gateway_version: '0.17.0',
        gateway_available_version: '0.17.0',
      },
    });
    expect(await screen.findByText('Gateway: 0.17.0 · Latest: 0.17.0')).toBeInTheDocument();
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
    expect(dialog).toHaveTextContent('one-time Terminal update before phone-driven updates can continue');
    expect(within(dialog).getByText(/HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install/)).not.toHaveTextContent(
      'plugin action invoke install-service',
    );
    expect(dialog).toHaveTextContent('preserves the configuration used by an existing stable service');
    expect(dialog).toHaveTextContent('Prefer to keep using a source checkout?');
    expect(screen.queryByText(/assets \d+/i)).not.toBeInTheDocument();
    expect(screen.getAllByRole('heading', { level: 3 }).at(-1)).toHaveTextContent('About');
  });
  it('routes a legacy app deployment owner to Update Help before scheduling', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.server({
      type: 'push_config',
      protocol: 2,
      release_version: '0.13.2',
      capabilities: ['self_update', 'app_deploy'],
      agent_profiles: [],
      update: {
        state: 'available',
        current_version: '0.13.2',
        available_version: APP_VERSION,
        available_revision: 'f'.repeat(12),
        target_revision: 'f'.repeat(40),
        can_install: true,
      },
      app_deploy: {
        configured: true,
        origin: location.origin,
        project: 'herdr-app',
        branch: 'main',
        revision: 'abc1234',
        state: 'idle',
      },
    });

    expect(await screen.findByText('Manual update required')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Check Fedora for updates' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'How to update Fedora' }));
    const dialog = screen.getByRole('dialog', { name: 'Update Fedora' });
    expect(dialog).toHaveTextContent('one-time Terminal update before phone-driven updates can continue');
    expect(dialog).toHaveTextContent(MANAGED_UPDATE_COMMAND);
    expect(socket.sent.map((payload) => JSON.parse(payload)).some((message) => message.type === 'install_update')).toBe(false);
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

  });

  it('persists the selected terminal history size', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const history = within(screen.getByRole('group', { name: 'Terminal History' }));

    expect(history.getByRole('button', { name: '1000' })).toHaveAttribute('aria-pressed', 'true');
    await user.click(history.getByRole('button', { name: '500' }));
    expect(history.getByRole('button', { name: '500' })).toHaveAttribute('aria-pressed', 'true');
    expect(localStorage.getItem(TERMINAL_HISTORY_KEY)).toBe('500');

    await user.click(history.getByRole('button', { name: '1000' }));
  });

  it('persists the selected terminal refresh interval', async () => {
    const user = userEvent.setup();
    render(SettingsView);
    const refresh = within(screen.getByRole('group', { name: 'Terminal Refresh' }));

    expect(refresh.getByRole('button', { name: '250 ms' })).toHaveAttribute('aria-pressed', 'true');
    await user.click(refresh.getByRole('button', { name: '100 ms' }));
    expect(refresh.getByRole('button', { name: '100 ms' })).toHaveAttribute('aria-pressed', 'true');
    expect(localStorage.getItem(TERMINAL_REFRESH_KEY)).toBe('100');

    await user.click(refresh.getByRole('button', { name: '250 ms' }));
  });

  it('does not expose removed terminal width choices', () => {
    render(SettingsView);

    expect(screen.queryByRole('group', { name: 'Terminal Width' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Fit to Phone' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Original Columns' })).not.toBeInTheDocument();
    expect(screen.getByText(/Resize Session automatically leases/)).toBeInTheDocument();
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

    await user.click(await screen.findByRole('button', { name: 'Update Relays' }));
    expect(screen.getByRole('dialog', { name: 'Update Relays' })).toHaveTextContent('Update Fedora first');
    expect(screen.queryByRole('button', { name: 'Update Fedora to version 0.8.0' })).not.toBeInTheDocument();
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Start Update' }));
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

    await user.click(await screen.findByRole('button', { name: 'Update Herdr' }));
    const dialog = screen.getByRole('dialog', { name: 'Update Herdr' });
    expect(dialog).toHaveTextContent('Publish the phone app from Fedora');
    await user.click(within(dialog).getByRole('button', { name: 'Start Update' }));
    const command = socket.sent.map((payload) => JSON.parse(payload))
      .find((message) => message.type === 'deploy_app_update');

    expect(command).toMatchObject({
      expected_version: '9.0.0',
      expected_revision: 'f'.repeat(40),
      expected_origin: location.origin,
    });
    socket.server({
      type: 'command_result',
      request_id: command.request_id,
      ok: true,
      phase: 'scheduled',
      data: {
        app_deploy: {
          configured: true,
          origin: location.origin,
          project: 'herdr-app',
          branch: 'main',
          revision: 'f'.repeat(40),
          state: 'scheduled',
          target_version: '9.0.0',
        },
      },
    });
    const publishing = /Publishing v9\.0\.0 from Fedora and waiting for this app origin to update\. This can take up to two minutes\./;
    expect(await screen.findByText(publishing)).toBeInTheDocument();

    socket.server({
      type: 'app_deploy_status',
      app_deploy: {
        configured: true,
        origin: location.origin,
        project: 'herdr-app',
        branch: 'main',
        revision: 'f'.repeat(40),
        state: 'deploying',
        target_version: '9.0.0',
      },
    });
    await waitFor(() => expect(screen.getByText(publishing)).toBeInTheDocument());
  });
  it('deploys the owner app before updating its relay', async () => {
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
      release_version: '8.0.0',
      revision: 'abc123',
      capabilities: ['self_update', 'app_deploy'],
      agent_profiles: [],
      update: {
        state: 'available',
        current_version: '8.0.0',
        current_revision: 'abc123',
        available_version: '9.0.0',
        available_revision: 'f'.repeat(12),
        target_revision: 'f'.repeat(40),
        can_install: true,
        mode: 'plugin',
      },
      app_deploy: {
        configured: true,
        origin: location.origin,
        project: 'herdr-app',
        branch: 'main',
        revision: 'abc123',
        state: 'idle',
      },
    });

    await user.click(await screen.findByRole('button', { name: 'Update Herdr' }));
    const dialog = screen.getByRole('dialog', { name: 'Update Herdr' });
    expect(dialog).toHaveTextContent('Publish the phone app first, then update Fedora');
    await user.click(within(dialog).getByRole('button', { name: 'Start Update' }));
    const commands = socket.sent.map((payload) => JSON.parse(payload));
    const command = commands.find((message) => message.type === 'install_update');

    expect(command).toMatchObject({
      expected_version: '9.0.0',
      expected_revision: 'f'.repeat(40),
      expected_origin: location.origin,
    });
    expect(commands.some((message) => message.type === 'deploy_app_update')).toBe(false);
  });

});
