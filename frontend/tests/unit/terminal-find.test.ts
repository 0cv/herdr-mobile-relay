import { describe, expect, it } from 'vitest';
import {
  findTerminalText,
  terminalMatchFragments,
  terminalRowForOffset,
  terminalRowOffsets,
  terminalSearchText,
} from '$lib/terminal-find';

describe('terminal find', () => {
  const rows = [
    { text: 'Build [release].' },
    { text: 'second BUILD result' },
    { text: 'done' },
  ];

  it('finds literal text case-insensitively across the loaded transcript', () => {
    const text = terminalSearchText(rows);
    expect(findTerminalText(text, 'build').matches).toEqual([
      { start: 0, end: 5 },
      { start: 24, end: 29 },
    ]);
    expect(findTerminalText(text, '[release].').matches).toEqual([
      { start: 6, end: 16 },
    ]);
  });

  it('caps pathological result sets and reports the cap', () => {
    expect(findTerminalText('x x x', 'x', 2)).toEqual({
      matches: [{ start: 0, end: 1 }, { start: 2, end: 3 }],
      truncated: true,
    });
  });

  it('maps matches back to virtualized rows, including line-spanning text', () => {
    const compactRows = [{ text: 'alpha' }, { text: 'beta' }, { text: 'gamma' }];
    const text = terminalSearchText(compactRows);
    const offsets = terminalRowOffsets(compactRows);
    const [match] = findTerminalText(text, 'ha\nbe').matches;

    expect(offsets).toEqual([0, 6, 11]);
    expect(terminalRowForOffset(compactRows, offsets, match.start)).toBe(0);
    expect(terminalMatchFragments(compactRows, offsets, match)).toEqual([
      { row: 0, start: 3, end: 5 },
      { row: 1, start: 0, end: 2 },
    ]);
  });
});
