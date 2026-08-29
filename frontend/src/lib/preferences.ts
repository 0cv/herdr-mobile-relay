import { writable } from 'svelte/store';
import {
  LEGACY_FONT_KEY,
  HOME_LAYOUT_KEY,
  HOME_LAYOUTS,
  INTERFACE_SIZE_KEY,
  INTERFACE_SIZES,
  TERMINAL_HISTORY_KEY,
  TERMINAL_HISTORY_OPTIONS,
  TERMINAL_HEIGHT_LEASE_KEY,
  TERMINAL_REFRESH_KEY,
  TERMINAL_REFRESH_OPTIONS,
  THEME_COLORS,
  THEME_KEY,
  THEME_TERMINAL_SCHEMES,
  THEMES,
  type HomeLayout,
  type InterfaceSize,
  type TerminalHistoryLines,
  type TerminalRefreshInterval,
  type Theme,
} from './config';
import { setTerminalScheme } from './terminal';

function savedTheme(): Theme {
  const value = localStorage.getItem(THEME_KEY);
  return THEMES.includes(value as Theme) ? value as Theme : 'nord';
}

function savedInterfaceSize(): InterfaceSize {
  const value = localStorage.getItem(INTERFACE_SIZE_KEY) || localStorage.getItem(LEGACY_FONT_KEY);
  return INTERFACE_SIZES.includes(value as InterfaceSize) ? value as InterfaceSize : 'compact';
}


function savedTerminalHistoryLines(): TerminalHistoryLines {
  const value = Number(localStorage.getItem(TERMINAL_HISTORY_KEY));
  return TERMINAL_HISTORY_OPTIONS.includes(value as TerminalHistoryLines)
    ? value as TerminalHistoryLines
    : 1_000;
}

function savedTerminalRefreshInterval(): TerminalRefreshInterval {
  const value = Number(localStorage.getItem(TERMINAL_REFRESH_KEY));
  return TERMINAL_REFRESH_OPTIONS.includes(value as TerminalRefreshInterval)
    ? value as TerminalRefreshInterval
    : 250;
}

// Mixed by default: one card per workspace with a state dot reads better than
// three state sections once workspaces carry worktrees, and agents needing
// input stay on top in both layouts.
function savedHomeLayout(): HomeLayout {
  const value = localStorage.getItem(HOME_LAYOUT_KEY);
  return HOME_LAYOUTS.includes(value as HomeLayout) ? value as HomeLayout : 'mixed';
}


export const theme = writable<Theme>(savedTheme());
export const interfaceSize = writable<InterfaceSize>(savedInterfaceSize());
export const terminalHistoryLines = writable<TerminalHistoryLines>(savedTerminalHistoryLines());
export const terminalRefreshInterval = writable<TerminalRefreshInterval>(savedTerminalRefreshInterval());
export const homeLayout = writable<HomeLayout>(savedHomeLayout());
// Off by default: resizing the shared pane's height strands stale copies of
// inline agents' status bars in the scrollback (the terminal reflows the
// primary buffer before the agent can repaint), so only people who need
// full-screen TUIs to fit the phone opt in.
export const terminalHeightLease = writable<boolean>(
  localStorage.getItem(TERMINAL_HEIGHT_LEASE_KEY) === 'true',
);

function applyTheme(value: Theme): void {
  document.documentElement.dataset.theme = value;
  document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', THEME_COLORS[value]);
  setTerminalScheme(THEME_TERMINAL_SCHEMES[value]);
}

export function setTheme(value: Theme): void {
  localStorage.setItem(THEME_KEY, value);
  applyTheme(value);
  theme.set(value);
}

export function setInterfaceSize(value: InterfaceSize): void {
  localStorage.setItem(INTERFACE_SIZE_KEY, value);
  interfaceSize.set(value);
  document.documentElement.dataset.interfaceSize = value;
}


export function setTerminalHistoryLines(value: TerminalHistoryLines): void {
  localStorage.setItem(TERMINAL_HISTORY_KEY, String(value));
  terminalHistoryLines.set(value);
}

export function setTerminalRefreshInterval(value: TerminalRefreshInterval): void {
  localStorage.setItem(TERMINAL_REFRESH_KEY, String(value));
  terminalRefreshInterval.set(value);
}

export function setTerminalHeightLease(value: boolean): void {
  localStorage.setItem(TERMINAL_HEIGHT_LEASE_KEY, String(value));
  terminalHeightLease.set(value);
}

export function setHomeLayout(value: HomeLayout): void {
  localStorage.setItem(HOME_LAYOUT_KEY, value);
  homeLayout.set(value);
}


export function initializePreferences(): void {
  theme.subscribe(applyTheme)();
  interfaceSize.subscribe((value) => { document.documentElement.dataset.interfaceSize = value; })();
}
