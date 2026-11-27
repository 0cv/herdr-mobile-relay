import { get } from 'svelte/store';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  HOME_LAYOUT_KEY,
  INTERFACE_SIZE_KEY,
  RECENT_SLASH_COMMANDS_KEY,
  TERMINAL_HEIGHT_LEASE_KEY,
  TERMINAL_HISTORY_KEY,
  TERMINAL_REFRESH_KEY,
  TERMINAL_WAKE_LOCK_KEY,
  THEME_KEY,
} from '$lib/config';

type Preferences = typeof import('$lib/preferences');

function values(preferences: Preferences) {
  return {
    theme: get(preferences.theme),
    interfaceSize: get(preferences.interfaceSize),
    historyLines: get(preferences.terminalHistoryLines),
    refreshInterval: get(preferences.terminalRefreshInterval),
    homeLayout: get(preferences.homeLayout),
    heightLease: get(preferences.terminalHeightLease),
    wakeLock: get(preferences.terminalWakeLock),
  };
}

const keys = [THEME_KEY, INTERFACE_SIZE_KEY, TERMINAL_HISTORY_KEY, TERMINAL_REFRESH_KEY,
  HOME_LAYOUT_KEY, TERMINAL_HEIGHT_LEASE_KEY, TERMINAL_WAKE_LOCK_KEY];

beforeEach(() => {
  localStorage.clear();
  vi.resetModules();
});

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  delete document.documentElement.dataset.theme;
  delete document.documentElement.dataset.interfaceSize;
});

it.each([null, 'invalid', ''])('loads defaults for missing or invalid preferences: %s', async (stored) => {
  if (stored !== null) for (const key of keys) localStorage.setItem(key, stored);
  const preferences = await import('$lib/preferences');
  expect(values(preferences)).toEqual({
    theme: 'nord', interfaceSize: 'compact', historyLines: 1_000, refreshInterval: 250,
    homeLayout: 'mixed', heightLease: false, wakeLock: false,
  });
});

it.each([
  null, 'invalid', '[]', 'null', '{"codex":"/review"}', '{"codex":["/review focus"]}',
  '{"codex":["/review","/review"]}', '{"codex":[4]}', '{"not-an-agent":["/review"]}',
  JSON.stringify({ codex: Array.from({ length: 11 }, (_, i) => `/command-${i}`) }),
  JSON.stringify({ codex: ['/' + 'a'.repeat(121)] }),
])('starts with no recent commands for missing or malformed storage: %s', async (stored) => {
  if (stored !== null) localStorage.setItem(RECENT_SLASH_COMMANDS_KEY, stored);
  const { readRecentSlashCommands } = await import('$lib/preferences');
  expect(readRecentSlashCommands()).toEqual({});
});

it('stores up to ten distinct commands per canonical kind, newest first', async () => {
  const { readRecentSlashCommands, recordRecentSlashCommand } = await import('$lib/preferences');
  for (let i = 0; i < 11; i++) recordRecentSlashCommand('Codex', `/command-${i}`);
  recordRecentSlashCommand('OpenAI Codex', '/command-3');
  recordRecentSlashCommand('Claude Code', '/command-3');
  recordRecentSlashCommand('unknown', '/review');

  const expected = {
    codex: [3, 10, 9, 8, 7, 6, 5, 4, 2, 1].map((i) => `/command-${i}`),
    claude: ['/command-3'],
  };
  expect(JSON.parse(localStorage.getItem(RECENT_SLASH_COMMANDS_KEY)!)).toEqual(expected);
  expect(readRecentSlashCommands()).toEqual(expected);
});

it('merges the latest storage before a write', async () => {
  const { readRecentSlashCommands, recordRecentSlashCommand } = await import('$lib/preferences');
  expect(readRecentSlashCommands()).toEqual({});
  localStorage.setItem(RECENT_SLASH_COMMANDS_KEY, JSON.stringify({ codex: ['/review'] }));
  recordRecentSlashCommand('Claude Code', '/help');
  expect(readRecentSlashCommands()).toEqual({ codex: ['/review'], claude: ['/help'] });
});

it('ignores inaccessible recent-command storage', async () => {
  const getItem = localStorage.getItem.bind(localStorage);
  vi.spyOn(localStorage, 'getItem').mockImplementation((key) => {
    if (key === RECENT_SLASH_COMMANDS_KEY) throw new Error('unreadable');
    return getItem(key);
  });
  const { readRecentSlashCommands, recordRecentSlashCommand } = await import('$lib/preferences');
  expect(readRecentSlashCommands()).toEqual({});
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
  expect(() => recordRecentSlashCommand('codex', '/review')).not.toThrow();
});

it('persists choices, applies appearance changes and reloads the saved values', async () => {
  const preferences = await import('$lib/preferences');
  preferences.setTheme('light');
  preferences.setInterfaceSize('large');
  preferences.setTerminalHistoryLines(500);
  preferences.setTerminalRefreshInterval(1_000);
  preferences.setHomeLayout('state');
  preferences.setTerminalHeightLease(true);
  preferences.setTerminalWakeLock(true);
  const expected = {
    theme: 'light', interfaceSize: 'large', historyLines: 500, refreshInterval: 1_000,
    homeLayout: 'state', heightLease: true, wakeLock: true,
  };
  expect(values(preferences)).toEqual(expected);
  expect(keys.map((key) => localStorage.getItem(key))).toEqual([
    'light', 'large', '500', '1000', 'state', 'true', 'true',
  ]);
  expect(document.documentElement.dataset.theme).toBe('light');
  expect(document.documentElement.dataset.interfaceSize).toBe('large');
  vi.resetModules();
  expect(values(await import('$lib/preferences'))).toEqual(expected);
});

it('leaves choices and appearance unchanged when storage writes throw', async () => {
  const preferences = await import('$lib/preferences');
  preferences.initializePreferences();
  const initial = values(preferences);
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
  for (const set of [
    () => preferences.setTheme('light'),
    () => preferences.setInterfaceSize('large'),
    () => preferences.setTerminalHistoryLines(500),
    () => preferences.setTerminalRefreshInterval(1_000),
    () => preferences.setHomeLayout('state'),
    () => preferences.setTerminalHeightLease(true),
    () => preferences.setTerminalWakeLock(true),
  ]) expect(set).toThrow('full');
  expect(values(preferences)).toEqual(initial);
  expect(document.documentElement.dataset.theme).toBe('nord');
  expect(document.documentElement.dataset.interfaceSize).toBe('compact');
});

it('keeps server visibility choices in memory when storage is unavailable', async () => {
  const preferences = await import('$lib/preferences');
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
  preferences.setServerHidden('relay-1', true);
  expect(get(preferences.hiddenServers)).toEqual(['relay-1']);
  preferences.pruneHiddenServers(['relay-2']);
  expect(get(preferences.hiddenServers)).toEqual([]);
});
