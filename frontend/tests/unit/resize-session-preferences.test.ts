import { get } from 'svelte/store';
import { afterEach, expect, it, vi } from 'vitest';
import { TERMINAL_HEIGHT_LEASE_KEY, TERMINAL_RESIZE_SESSION_KEY } from '$lib/config';
import { readTerminalResizeSession, setTerminalResizeSession, terminalHeightLease, terminalResizeSession } from '$lib/preferences';

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  terminalResizeSession.set(false);
});

it('defaults on without a stored preference and leaves height off', () => {
  expect(readTerminalResizeSession()).toBe(true);
  expect(get(terminalResizeSession)).toBe(true);
  expect(get(terminalHeightLease)).toBe(false);
  expect(localStorage.getItem(TERMINAL_RESIZE_SESSION_KEY)).toBeNull();
  localStorage.setItem(TERMINAL_HEIGHT_LEASE_KEY, 'true');
  expect(readTerminalResizeSession()).toBe(true);
  for (const value of ['', '1', 'TRUE', 'false', 'garbage', '{}']) {
    localStorage.setItem(TERMINAL_RESIZE_SESSION_KEY, value);
    expect(readTerminalResizeSession()).toBe(false);
  }
  expect(readTerminalResizeSession({ getItem() { throw new Error('unavailable'); } })).toBe(false);
});

it('persists explicit choices independently of height', () => {
  localStorage.setItem(TERMINAL_HEIGHT_LEASE_KEY, 'true');
  expect(setTerminalResizeSession(true)).toBe('saved');
  expect(get(terminalResizeSession)).toBe(true);
  expect(readTerminalResizeSession()).toBe(true);
  expect(setTerminalResizeSession(false)).toBe('saved');
  expect(get(terminalResizeSession)).toBe(false);
  expect(readTerminalResizeSession()).toBe(false);
  expect(localStorage.getItem(TERMINAL_HEIGHT_LEASE_KEY)).toBe('true');
});

it('fails closed on opt-in persistence failure and still permits local stop', () => {
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
  terminalResizeSession.set(false);
  expect(setTerminalResizeSession(true)).toBe('unavailable');
  expect(get(terminalResizeSession)).toBe(false);
  terminalResizeSession.set(true);
  expect(setTerminalResizeSession(false)).toBe('unavailable');
  expect(get(terminalResizeSession)).toBe(false);
});
