import { expect, it } from 'vitest';
import { defaultKeyRow, keyRowEntries, keyRowLabel, validKeyRow } from '$lib/terminal-key-row';

it('offers only the eight defaults, direct arrows, Shift+Tab and supported Ctrl+letters', () => {
  expect(defaultKeyRow.map(keyRowLabel)).toEqual(['Esc', 'Tab', 'Shift', 'Ctrl', 'Alt', 'F keys', 'Arrow keys', 'Enter']);
  expect(keyRowEntries).toEqual([
    ...defaultKeyRow, 'Up', 'Down', 'Left', 'Right', 'shift+tab',
    ...[...'abcdefghijklmnopqrstuvwxyz'].filter((letter) => !['i', 'm'].includes(letter)).map((letter) => `ctrl+${letter}`),
  ]);
  expect(keyRowLabel('ctrl+c')).toBe('Ctrl+C');
  expect(keyRowLabel('shift+tab')).toBe('Shift+Tab');
});

it('validates whole layouts including the empty layout', () => {
  expect(validKeyRow(keyRowEntries)).toBe(true);
  expect(validKeyRow([])).toBe(true);
  for (const invalid of [null, {}, 'Tab', ['Tab', 'Tab'], [null], ['ctrl+i'], ['ctrl+m'], ['Ctrl+C'],
    ['ctrl+up'], ['ctrl+pageup'], ['PageUp'], ['F1'], ['hello'], [['Tab', 'Enter']]]) {
    expect(validKeyRow(invalid)).toBe(false);
  }
});
