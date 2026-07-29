import { writable } from 'svelte/store';
import {
  LEGACY_FONT_KEY,
  INTERFACE_SIZE_KEY,
  INTERFACE_SIZES,
  TERMINAL_HISTORY_KEY,
  TERMINAL_HISTORY_OPTIONS,
  TERMINAL_REFRESH_KEY,
  TERMINAL_REFRESH_OPTIONS,
  TERMINAL_LAYOUT_KEY,
  TERMINAL_LAYOUTS,
  THEME_COLORS,
  THEME_KEY,
  THEMES,
  type InterfaceSize,
  type TerminalHistoryLines,
  type TerminalRefreshInterval,
  type TerminalLayout,
  type Theme,
} from './config';

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

function savedTerminalLayout(): TerminalLayout {
  const value = localStorage.getItem(TERMINAL_LAYOUT_KEY);
  return TERMINAL_LAYOUTS.includes(value as TerminalLayout) ? value as TerminalLayout : 'resize';
}

export const theme = writable<Theme>(savedTheme());
export const interfaceSize = writable<InterfaceSize>(savedInterfaceSize());
export const terminalHistoryLines = writable<TerminalHistoryLines>(savedTerminalHistoryLines());
export const terminalRefreshInterval = writable<TerminalRefreshInterval>(savedTerminalRefreshInterval());
export const terminalLayout = writable<TerminalLayout>(savedTerminalLayout());

export function setTheme(value: Theme): void {
  localStorage.setItem(THEME_KEY, value);
  theme.set(value);
  document.documentElement.dataset.theme = value;
  document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', THEME_COLORS[value]);
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

export function setTerminalLayout(value: TerminalLayout): void {
  localStorage.setItem(TERMINAL_LAYOUT_KEY, value);
  terminalLayout.set(value);
}

export function initializePreferences(): void {
  theme.subscribe((value) => {
    document.documentElement.dataset.theme = value;
    document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', THEME_COLORS[value]);
  })();
  interfaceSize.subscribe((value) => { document.documentElement.dataset.interfaceSize = value; })();
}
