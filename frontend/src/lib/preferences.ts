import { writable } from 'svelte/store';
import {
  LEGACY_FONT_KEY,
  INTERFACE_SIZE_KEY,
  INTERFACE_SIZES,
  STATUS_LINE_KEY,
  TERMINAL_HISTORY_KEY,
  TERMINAL_HISTORY_OPTIONS,
  THEME_COLORS,
  THEME_KEY,
  THEMES,
  type InterfaceSize,
  type TerminalHistoryLines,
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

function savedStatusLine(): boolean {
  const value = localStorage.getItem(STATUS_LINE_KEY);
  if (value !== null) return value !== 'false';
  return !window.matchMedia?.('(max-width: 767px)').matches;
}

function savedTerminalHistoryLines(): TerminalHistoryLines {
  const value = Number(localStorage.getItem(TERMINAL_HISTORY_KEY));
  return TERMINAL_HISTORY_OPTIONS.includes(value as TerminalHistoryLines)
    ? value as TerminalHistoryLines
    : 1_000;
}

export const theme = writable<Theme>(savedTheme());
export const interfaceSize = writable<InterfaceSize>(savedInterfaceSize());
export const showAgentStatusLine = writable(savedStatusLine());
export const terminalHistoryLines = writable<TerminalHistoryLines>(savedTerminalHistoryLines());

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

export function setShowAgentStatusLine(value: boolean): void {
  localStorage.setItem(STATUS_LINE_KEY, value ? 'true' : 'false');
  showAgentStatusLine.set(value);
}

export function setTerminalHistoryLines(value: TerminalHistoryLines): void {
  localStorage.setItem(TERMINAL_HISTORY_KEY, String(value));
  terminalHistoryLines.set(value);
}

export function initializePreferences(): void {
  theme.subscribe((value) => {
    document.documentElement.dataset.theme = value;
    document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', THEME_COLORS[value]);
  })();
  interfaceSize.subscribe((value) => { document.documentElement.dataset.interfaceSize = value; })();
}
