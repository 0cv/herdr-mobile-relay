import type { RelayConfig } from './types';

export const RELAYS_KEY = 'herdr_relays';
export const THEME_KEY = 'herdr_theme';
// Keep the existing key so stored terminal-size preferences migrate into the
// whole-interface size without resetting users.
export const INTERFACE_SIZE_KEY = 'herdr_terminal_font_size';
export const LEGACY_FONT_KEY = 'herdr_home_font_size';
export const TERMINAL_HISTORY_KEY = 'herdr_terminal_history_lines';
export const TERMINAL_REFRESH_KEY = 'herdr_terminal_refresh_ms';
export const HOME_LAYOUT_KEY = 'herdr_home_workspace_layout';
export const DEVICE_LOCK_KEY = 'herdr_require_device_unlock';
export const DEVICE_CREDENTIAL_KEY = 'herdr_device_unlock_credential';
export const PUSH_ENABLED_KEY = 'herdr_push_enabled';
export const PUSH_FINISHED_KEY = 'herdr_push_finished';
export const PUSH_CLIENT_KEY = 'herdr_push_client_id';
export const PUSH_VAPID_KEY_PREFIX = 'herdr_push_vapid_key_';
export const HANDLED_NOTIFICATION_ACTIONS_KEY = 'herdr_handled_notification_actions';

export const APP_PROTOCOL_VERSION = __APP_PROTOCOL_VERSION__;
export const APP_VERSION = __APP_VERSION__;
export const APP_ASSET_VERSION = __APP_ASSET_VERSION__;
export const SERVICE_WORKER_URL = __SERVICE_WORKER_URL__;
export const THEMES = ['dark', 'light', 'nord', 'solarized', 'rose'] as const;
export type Theme = (typeof THEMES)[number];
export const INTERFACE_SIZES = ['compact', 'regular', 'large'] as const;
export type InterfaceSize = (typeof INTERFACE_SIZES)[number];
export const TERMINAL_HISTORY_OPTIONS = [100, 500, 1_000] as const;
export type TerminalHistoryLines = (typeof TERMINAL_HISTORY_OPTIONS)[number];
export const TERMINAL_REFRESH_OPTIONS = [100, 250, 500, 1_000] as const;
export type TerminalRefreshInterval = (typeof TERMINAL_REFRESH_OPTIONS)[number];
export const HOME_LAYOUTS = ['state', 'mixed'] as const;
export type HomeLayout = (typeof HOME_LAYOUTS)[number];
export const HOME_LAYOUT_LABELS: Record<HomeLayout, string> = {
  state: 'By State',
  mixed: 'Mixed',
};
export const TERMINAL_REFRESH_LABELS: Record<TerminalRefreshInterval, string> = {
  100: '100 ms',
  250: '250 ms',
  500: '500 ms',
  1_000: '1 s',
};
export const MIN_PANE_SIZE_COLUMNS = 40;
export const MAX_PANE_SIZE_COLUMNS = 240;
export const THEME_COLORS: Record<Theme, string> = {
  dark: '#0a0a0a',
  light: '#f5f5f5',
  nord: '#2e3440',
  solarized: '#002b36',
  rose: '#191724',
};

export function relayLabelFromUrl(url: string): string {
  try {
    return new URL(url).hostname.split('.')[0] || 'relay';
  } catch {
    return 'relay';
  }
}

export function makeRelayId(label: string, url: string, gatewayUrl = ''): string {
  // A hybrid relay has no URL of its own, so its identity is the gateway it
  // answers on plus the label from its setup link.
  const target = url || gatewayUrl;
  return `${label || relayLabelFromUrl(target)}-${target}`
    .toLowerCase()
    .replace(/^wss?:\/\//, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, 48) || 'relay';
}

/**
 * Accepts only a bare wss origin (ws when the page itself is insecure): no
 * credentials, no path, no query, no fragment. Anything else in a setup link
 * is treated as hostile.
 */
function safeSocketOrigin(value: string, pageProtocol: string): string | null {
  try {
    const parsed = new URL(value.trim());
    const allowedProtocol = parsed.protocol === 'wss:' || (pageProtocol === 'http:' && parsed.protocol === 'ws:');
    if (
      !allowedProtocol
      || parsed.username
      || parsed.password
      || !parsed.hostname
      || !['', '/'].includes(parsed.pathname)
      || parsed.search
      || parsed.hash
    ) return null;
    return parsed.origin;
  } catch {
    return null;
  }
}

/**
 * Ordered gateway origins: unusable entries are dropped and repeats collapsed,
 * so the failover never dials the same address twice in one pass.
 */
function gatewayOrigins(values: readonly string[], pageProtocol: string): string[] {
  const origins: string[] = [];
  for (const value of values) {
    const origin = safeSocketOrigin(String(value || ''), pageProtocol);
    if (!origin || origins.includes(origin)) continue;
    origins.push(origin);
  }
  return origins;
}

export function normalizeRelayConfig(relay: Partial<RelayConfig>): RelayConfig {
  const url = String(relay.url || '').trim();
  // The primary leads: a relay that advertises a new gateway address while it
  // is connected is fresher than the list stored beside it, and a config
  // written before the list existed carries the primary alone. Stored entries
  // were checked against the page protocol when they were imported, so
  // re-reading them uses the permissive rule and a LAN gateway paired over
  // plain http keeps its ws: address.
  const listed = Array.isArray(relay.gatewayUrls) ? relay.gatewayUrls : [];
  const gateways = gatewayOrigins([String(relay.gatewayUrl || ''), ...listed], 'http:');
  const gatewayUrl = gateways[0] || '';
  const label = String(relay.label || relayLabelFromUrl(url || gatewayUrl)).trim();
  const config: RelayConfig = {
    id: relay.id || makeRelayId(label, url, gatewayUrl),
    label,
    url,
    token: relay.token || '',
  };
  // Legacy entries keep their exact stored shape: no transport field at all.
  if (relay.transport !== 'hybrid' && (url || !gatewayUrl)) return config;
  config.transport = 'hybrid';
  config.gatewayUrl = gatewayUrl;
  if (gateways.length) config.gatewayUrls = gateways;
  return config;
}

export function loadRelayConfigs(storage: Storage = localStorage): RelayConfig[] {
  const raw = storage.getItem(RELAYS_KEY);
  if (raw) {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (Array.isArray(parsed)) {
        return parsed
          .filter((relay): relay is Partial<RelayConfig> => Boolean(
            relay && typeof relay === 'object' && (relay.url || relay.gatewayUrl),
          ))
          .map(normalizeRelayConfig);
      }
    } catch {
      // Fall through to the legacy single-relay keys.
    }
  }
  const url = storage.getItem('herdr_relay_url') || '';
  if (!url) return [];
  const relay = normalizeRelayConfig({
    url,
    token: storage.getItem('herdr_relay_token') || '',
    label: relayLabelFromUrl(url),
  });
  storage.setItem(RELAYS_KEY, JSON.stringify([relay]));
  return [relay];
}

export function saveRelayConfigs(relays: RelayConfig[], storage: Storage = localStorage): void {
  storage.setItem(RELAYS_KEY, JSON.stringify(relays));
}

export function quickSetupConfig(locationValue: Pick<Location, 'hash' | 'protocol' | 'host'>): Omit<RelayConfig, 'id'> | null {
  const params = new URLSearchParams(String(locationValue.hash || '').replace(/^#/, ''));
  const token = params.get('setup') || '';
  if (token.length < 16 || token.length > 512) return null;
  if (!['http:', 'https:'].includes(locationValue.protocol)) return null;
  const label = (params.get('label') || 'This computer').trim().slice(0, 48) || 'This computer';
  const configuredGateways = params.get('gateways');
  const configuredGateway = params.get('gateway');
  if (configuredGateways !== null || configuredGateway) {
    // `gateways=` carries the complete ordered list, so it decides the order
    // and the primary; a link with only `gateway=` is a one-entry list. The
    // separator stays literal, each entry is percent-encoded on its own.
    const entries = configuredGateways === null
      ? [String(configuredGateway)]
      : configuredGateways.split(',');
    const gatewayUrls = gatewayOrigins(entries, locationValue.protocol);
    if (!gatewayUrls.length) return null;
    return { label, url: '', token, transport: 'hybrid', gatewayUrl: gatewayUrls[0], gatewayUrls };
  }
  const configuredRelay = params.get('relay');
  let url = `${locationValue.protocol === 'https:' ? 'wss:' : 'ws:'}//${locationValue.host}`;
  if (configuredRelay) {
    const origin = safeSocketOrigin(configuredRelay, locationValue.protocol);
    if (!origin) return null;
    url = origin;
  }
  return { label, url, token };
}

export function importQuickSetup(
  relays: RelayConfig[],
  locationValue: Pick<Location, 'hash' | 'protocol' | 'host'>,
): RelayConfig[] | null {
  const setup = quickSetupConfig(locationValue);
  if (!setup) return null;
  // A shared gateway hosts many computers, so a hybrid entry is matched on the
  // credential or the label rather than on the gateway address alone. Any
  // shared entry counts: a relay that gained a gateway or reordered its list
  // updates its entry instead of pairing itself a second time.
  const existing = setup.transport === 'hybrid'
    ? relays.find((relay) => relay.transport === 'hybrid'
      && (relay.gatewayUrls ?? [relay.gatewayUrl ?? '']).some((entry) => setup.gatewayUrls?.includes(entry))
      && (relay.token === setup.token || relay.label === setup.label))
    : relays.find((relay) => relay.url === setup.url);
  const next = normalizeRelayConfig({
    id: existing?.id,
    label: existing?.label || setup.label,
    url: setup.url,
    token: setup.token,
    transport: setup.transport,
    gatewayUrl: setup.gatewayUrl,
    gatewayUrls: setup.gatewayUrls,
  });
  return existing ? relays.map((relay) => (relay.id === existing.id ? next : relay)) : [...relays, next];
}

declare const __APP_PROTOCOL_VERSION__: number;
declare const __APP_VERSION__: string;
declare const __APP_ASSET_VERSION__: number;
declare const __SERVICE_WORKER_URL__: string;
