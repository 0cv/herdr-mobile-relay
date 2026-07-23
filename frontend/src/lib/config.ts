import type { RelayConfig } from './types';

export const RELAYS_KEY = 'herdr_relays';
export const THEME_KEY = 'herdr_theme';
// Keep the existing key so stored terminal-size preferences migrate into the
// whole-interface size without resetting users.
export const INTERFACE_SIZE_KEY = 'herdr_terminal_font_size';
export const LEGACY_FONT_KEY = 'herdr_home_font_size';
export const STATUS_LINE_KEY = 'herdr_show_codex_status_line';
export const TERMINAL_HISTORY_KEY = 'herdr_terminal_history_lines';
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
export const UPSTREAM_APP_VERSION_URL = 'https://raw.githubusercontent.com/0cv/herdr-mobile-relay/main/web/version.json';
export const THEMES = ['dark', 'light', 'nord', 'solarized', 'rose'] as const;
export type Theme = (typeof THEMES)[number];
export const INTERFACE_SIZES = ['compact', 'regular', 'large'] as const;
export type InterfaceSize = (typeof INTERFACE_SIZES)[number];
export const TERMINAL_HISTORY_OPTIONS = [100, 1_000, 5_000, 10_000] as const;
export type TerminalHistoryLines = (typeof TERMINAL_HISTORY_OPTIONS)[number];
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

export function makeRelayId(label: string, url: string): string {
  return `${label || relayLabelFromUrl(url)}-${url}`
    .toLowerCase()
    .replace(/^wss?:\/\//, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, 48) || 'relay';
}

export function normalizeRelayConfig(relay: Partial<RelayConfig>): RelayConfig {
  const url = String(relay.url || '').trim();
  const label = String(relay.label || relayLabelFromUrl(url)).trim();
  return {
    id: relay.id || makeRelayId(label, url),
    label,
    url,
    token: relay.token || '',
  };
}

export function loadRelayConfigs(storage: Storage = localStorage): RelayConfig[] {
  const raw = storage.getItem(RELAYS_KEY);
  if (raw) {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (Array.isArray(parsed)) {
        return parsed
          .filter((relay): relay is Partial<RelayConfig> => Boolean(
            relay && typeof relay === 'object' && 'url' in relay && relay.url,
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
  const configuredRelay = params.get('relay');
  let url = `${locationValue.protocol === 'https:' ? 'wss:' : 'ws:'}//${locationValue.host}`;
  if (configuredRelay) {
    try {
      const parsed = new URL(configuredRelay);
      const allowedProtocol = parsed.protocol === 'wss:'
        || (locationValue.protocol === 'http:' && parsed.protocol === 'ws:');
      if (
        !allowedProtocol
        || parsed.username
        || parsed.password
        || !parsed.hostname
        || !['', '/'].includes(parsed.pathname)
        || parsed.search
        || parsed.hash
      ) return null;
      url = parsed.origin;
    } catch {
      return null;
    }
  }
  const label = (params.get('label') || 'This computer').trim().slice(0, 48) || 'This computer';
  return { label, url, token };
}

export function importQuickSetup(
  relays: RelayConfig[],
  locationValue: Pick<Location, 'hash' | 'protocol' | 'host'>,
): RelayConfig[] | null {
  const setup = quickSetupConfig(locationValue);
  if (!setup) return null;
  const existing = relays.find((relay) => relay.url === setup.url);
  const next = normalizeRelayConfig({
    id: existing?.id,
    label: existing?.label || setup.label,
    url: setup.url,
    token: setup.token,
  });
  return existing ? relays.map((relay) => (relay.id === existing.id ? next : relay)) : [...relays, next];
}

declare const __APP_PROTOCOL_VERSION__: number;
declare const __APP_VERSION__: string;
declare const __APP_ASSET_VERSION__: number;
declare const __SERVICE_WORKER_URL__: string;
