import { isRecord } from './record';
import { get, writable } from 'svelte/store';
import {
  DEFAULT_AGENT_VIEW_KEY,
  PANE_AGENT_VIEW_OVERRIDES_KEY,
  HIDDEN_SERVERS_KEY,
  LEGACY_FONT_KEY,
  HOME_LAYOUT_KEY,
  HOME_LAYOUTS,
  INTERFACE_SIZE_KEY,
  INTERFACE_SIZES,
  TERMINAL_HISTORY_KEY,
  TERMINAL_HISTORY_OPTIONS,
  TERMINAL_HEIGHT_LEASE_KEY,
  TERMINAL_RESIZE_SESSION_KEY,
  TERMINAL_WAKE_LOCK_KEY,
  TERMINAL_REFRESH_KEY,
  TERMINAL_REFRESH_OPTIONS,
  THEME_COLORS,
  THEME_KEY,
  THEME_TERMINAL_SCHEMES,
  THEMES,
  type AgentView,
  type HomeLayout,
  type InterfaceSize,
  type TerminalHistoryLines,
  type TerminalRefreshInterval,
  type Theme,
} from './config';
import { setTerminalScheme } from './terminal';
import type { Agent } from './types';
import {
  isAgentView,
  paneViewPreferenceKey,
  parsePaneViewPreferenceKey,
  type PaneAgentViewOverrides,
} from './agent-view';

export function readTerminalResizeSession(storage?: Pick<Storage, 'getItem'>): boolean {
  try {
    const value = (storage || localStorage).getItem(TERMINAL_RESIZE_SESSION_KEY);
    return value === null || value === 'true';
  } catch {
    return false;
  }
}

export function readDefaultAgentView(storage?: Pick<Storage, 'getItem'>): AgentView {
  try {
    const value = (storage || localStorage).getItem(DEFAULT_AGENT_VIEW_KEY);
    return isAgentView(value) ? value : 'conversation';
  } catch {
    return 'conversation';
  }
}

export function readPaneAgentViewOverrides(
  storage?: Pick<Storage, 'getItem'>,
): PaneAgentViewOverrides {
  try {
    const raw = (storage || localStorage).getItem(PANE_AGENT_VIEW_OVERRIDES_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!isRecord(parsed)) return {};
    const overrides: Record<string, AgentView> = {};
    for (const [key, value] of Object.entries(parsed)) {
      if (parsePaneViewPreferenceKey(key) && isAgentView(value)) overrides[key] = value;
    }
    return overrides;
  } catch {
    return {};
  }
}

function savedChoice<T extends string | number>(value: unknown, choices: readonly T[], fallback: T): T {
  return choices.includes(value as T) ? value as T : fallback;
}

function persistedPreference<T>(
  key: string,
  initialValue: T,
  serialize: (value: T) => string | null = String,
) {
  const store = writable(initialValue);
  function persist(value: T): void {
    const serialized = serialize(value);
    if (serialized === null) localStorage.removeItem(key);
    else localStorage.setItem(key, serialized);
  }
  return {
    store,
    persist,
    set(value: T): void {
      persist(value);
      store.set(value);
    },
  };
}

const defaultAgentViewPreference = persistedPreference(DEFAULT_AGENT_VIEW_KEY, readDefaultAgentView());
const paneAgentViewPreference = persistedPreference(
  PANE_AGENT_VIEW_OVERRIDES_KEY,
  readPaneAgentViewOverrides(),
  (value) => Object.keys(value).length ? JSON.stringify(value) : null,
);
export const defaultAgentView = defaultAgentViewPreference.store;
export const paneAgentViewOverrides = paneAgentViewPreference.store;
const themePreference = persistedPreference<Theme>(
  THEME_KEY,
  savedChoice(localStorage.getItem(THEME_KEY), THEMES, 'nord'),
);
export const theme = themePreference.store;
const interfaceSizePreference = persistedPreference<InterfaceSize>(
  INTERFACE_SIZE_KEY,
  savedChoice(localStorage.getItem(INTERFACE_SIZE_KEY) || localStorage.getItem(LEGACY_FONT_KEY), INTERFACE_SIZES, 'compact'),
);
export const interfaceSize = interfaceSizePreference.store;
export const { store: terminalHistoryLines, set: setTerminalHistoryLines } = persistedPreference<TerminalHistoryLines>(
  TERMINAL_HISTORY_KEY,
  savedChoice(Number(localStorage.getItem(TERMINAL_HISTORY_KEY)), TERMINAL_HISTORY_OPTIONS, 1_000),
);
export const { store: terminalRefreshInterval, set: setTerminalRefreshInterval } = persistedPreference<TerminalRefreshInterval>(
  TERMINAL_REFRESH_KEY,
  savedChoice(Number(localStorage.getItem(TERMINAL_REFRESH_KEY)), TERMINAL_REFRESH_OPTIONS, 250),
);
export const { store: homeLayout, set: setHomeLayout } = persistedPreference<HomeLayout>(
  HOME_LAYOUT_KEY,
  savedChoice(localStorage.getItem(HOME_LAYOUT_KEY), HOME_LAYOUTS, 'mixed'),
);
const hiddenServersPreference = persistedPreference(HIDDEN_SERVERS_KEY, readHiddenServers(), JSON.stringify);
export const hiddenServers = hiddenServersPreference.store;
const terminalResizeSessionPreference = persistedPreference(TERMINAL_RESIZE_SESSION_KEY, readTerminalResizeSession());
export const terminalResizeSession = terminalResizeSessionPreference.store;
export const { store: terminalHeightLease, set: setTerminalHeightLease } = persistedPreference(
  TERMINAL_HEIGHT_LEASE_KEY,
  localStorage.getItem(TERMINAL_HEIGHT_LEASE_KEY) === 'true',
);
export const { store: terminalWakeLock, set: setTerminalWakeLock } = persistedPreference(
  TERMINAL_WAKE_LOCK_KEY,
  localStorage.getItem(TERMINAL_WAKE_LOCK_KEY) === 'true',
);

function applyTheme(value: Theme): void {
  document.documentElement.dataset.theme = value;
  document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', THEME_COLORS[value]);
  setTerminalScheme(THEME_TERMINAL_SCHEMES[value]);
}

export function setDefaultAgentView(value: AgentView): 'saved' | 'unavailable' {
  if (!isAgentView(value)) return 'unavailable';
  try {
    defaultAgentViewPreference.persist(value);
  } catch {
    return 'unavailable';
  }
  defaultAgentView.set(value);
  return 'saved';
}

export function setPaneAgentView(
  agent: Agent,
  value: AgentView | null,
): 'saved' | 'unavailable' | 'invalid-target' {
  const key = paneViewPreferenceKey(agent);
  if (!key) return 'invalid-target';
  if (value !== null && !isAgentView(value)) return 'unavailable';
  const current = get(paneAgentViewOverrides);
  const next = { ...current } as Record<string, AgentView>;
  if (value === null) delete next[key];
  else next[key] = value;
  const changed = Object.keys(current).length !== Object.keys(next).length
    || Object.entries(next).some(([entryKey, entryValue]) => current[entryKey] !== entryValue);
  if (!changed) return 'saved';
  try {
    paneAgentViewPreference.persist(next);
  } catch {
    return 'unavailable';
  }
  paneAgentViewOverrides.set(next);
  return 'saved';
}

function readHiddenServers(): string[] {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(HIDDEN_SERVERS_KEY) || '[]');
    return Array.isArray(value) ? value.filter((id) => typeof id === 'string') : [];
  } catch {
    return [];
  }
}

function saveHiddenServers(next: string[]): void {
  hiddenServers.set(next);
  try {
    hiddenServersPreference.persist(next);
  } catch {
    // Without storage the store still applies the choice until the app reloads.
  }
}

export function setServerHidden(relayId: string, hidden: boolean): void {
  const current = get(hiddenServers);
  if (current.includes(relayId) === hidden) return;
  saveHiddenServers(hidden ? [...current, relayId] : current.filter((id) => id !== relayId));
}

// With one computer left the filter row disappears, so nothing may stay hidden.
export function pruneHiddenServers(remainingRelayIds: string[]): void {
  const current = get(hiddenServers);
  const next = remainingRelayIds.length > 1 ? current.filter((id) => remainingRelayIds.includes(id)) : [];
  if (next.length !== current.length) saveHiddenServers(next);
}

export function clearPaneAgentViewOverridesForRelay(relayId: string): 'saved' | 'unavailable' {
  const current = get(paneAgentViewOverrides);
  const next = { ...current } as Record<string, AgentView>;
  let changed = false;
  for (const key of Object.keys(current)) {
    const identity = parsePaneViewPreferenceKey(key);
    if (identity?.[0] !== relayId) continue;
    delete next[key];
    changed = true;
  }
  if (!changed) return 'saved';
  try {
    paneAgentViewPreference.persist(next);
  } catch {
    return 'unavailable';
  }
  paneAgentViewOverrides.set(next);
  return 'saved';
}

export function setTheme(value: Theme): void {
  themePreference.persist(value);
  applyTheme(value);
  theme.set(value);
}

export function setInterfaceSize(value: InterfaceSize): void {
  interfaceSizePreference.set(value);
  document.documentElement.dataset.interfaceSize = value;
}

export function setTerminalResizeSession(value: boolean): 'saved' | 'unavailable' {
  // Turning off remains effective locally even if persistence is unavailable.
  if (!value) terminalResizeSession.set(false);
  try {
    terminalResizeSessionPreference.persist(value);
  } catch {
    return 'unavailable';
  }
  terminalResizeSession.set(value);
  return 'saved';
}

export function initializePreferences(): void {
  theme.subscribe(applyTheme)();
  interfaceSize.subscribe((value) => { document.documentElement.dataset.interfaceSize = value; })();
}
