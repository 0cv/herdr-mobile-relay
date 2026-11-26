import { get } from 'svelte/store';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  HOME_LAYOUT_KEY,
  INTERFACE_SIZE_KEY,
  TERMINAL_HEIGHT_LEASE_KEY,
  TERMINAL_HISTORY_KEY,
  TERMINAL_KEY_ROW_KEY,
  TERMINAL_REFRESH_KEY,
  TERMINAL_WAKE_LOCK_KEY,
  THEME_KEY,
} from '$lib/config';
import { defaultKeyRow } from '$lib/terminal-key-row';

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

it.each([null, '{', 'null', '"Tab"', '["Tab","Tab"]', '["Tab","unknown"]'])(
  'uses the default key row for missing or invalid data: %s', async (stored) => {
    if (stored !== null) localStorage.setItem(TERMINAL_KEY_ROW_KEY, stored);
    expect(get((await import('$lib/preferences')).terminalKeyRow)).toEqual(defaultKeyRow);
  },
);

it.each([{ row: [] }, { row: ['ctrl+c', 'Tab'] }])('reloads the saved key row $row and removes it when reset', async ({ row }) => {
  const original = await import('$lib/preferences');
  original.setTerminalKeyRow(row);
  expect(get(original.terminalKeyRow)).toEqual(row);
  expect(localStorage.getItem(TERMINAL_KEY_ROW_KEY)).toBe(JSON.stringify(row));
  vi.resetModules();
  const reloaded = await import('$lib/preferences');
  expect(get(reloaded.terminalKeyRow)).toEqual(row);
  reloaded.setTerminalKeyRow(defaultKeyRow);
  expect(get(reloaded.terminalKeyRow)).toEqual(defaultKeyRow);
  expect(localStorage.getItem(TERMINAL_KEY_ROW_KEY)).toBeNull();
});

it('does not publish a key row if validation or storage fails', async () => {
  const read = localStorage.getItem.bind(localStorage);
  vi.spyOn(localStorage, 'getItem').mockImplementation((key) => {
    if (key === TERMINAL_KEY_ROW_KEY) throw new Error('blocked');
    return read(key);
  });
  const { setTerminalKeyRow, terminalKeyRow } = await import('$lib/preferences');
  expect(get(terminalKeyRow)).toEqual(defaultKeyRow);
  expect(() => setTerminalKeyRow(['unknown'])).toThrow();
  expect(get(terminalKeyRow)).toEqual(defaultKeyRow);

  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
  expect(() => setTerminalKeyRow([])).toThrow('blocked');
  expect(get(terminalKeyRow)).toEqual(defaultKeyRow);
  vi.mocked(localStorage.setItem).mockRestore();
  setTerminalKeyRow(['ctrl+c']);
  vi.spyOn(localStorage, 'removeItem').mockImplementation(() => { throw new Error('blocked'); });
  expect(() => setTerminalKeyRow(defaultKeyRow)).toThrow('blocked');
  expect(get(terminalKeyRow)).toEqual(['ctrl+c']);
});
