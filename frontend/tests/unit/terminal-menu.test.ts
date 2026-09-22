import { describe, expect, it } from 'vitest';

import { detectTerminalMenu, terminalTextInputMode } from '../../src/lib/terminal-menu';

describe('terminalTextInputMode', () => {
  it('keeps matching the Hermes approval footer it was built for', () => {
    // Real Hermes question footer, as captured in this repo's own fixtures
    // (tests/browser/mobile-journeys.spec.ts, tests/unit/components.test.ts).
    expect(terminalTextInputMode('Custom answer: Which weekend?\n>\nenter or ctrl+q submit  esc cancel  ctrl+g external editor')).toBe('submit');
    expect(terminalTextInputMode('press enter submit')).toBe('submit');
  });

  it('matches the Cursor model picker footer', () => {
    // Captured from a live cursor pane running /model (agent bundle
    // v2026.09.18): the picker renders its hints as one bullet-separated line.
    const footer = 'Type to filter • Enter to select • Tab to edit';
    expect(terminalTextInputMode(footer)).toBe('filter');
    expect(terminalTextInputMode(footer.replaceAll('•', '|'))).toBe('filter');
    expect(terminalTextInputMode(`${footer}\n${'old output\n'.repeat(8)}`)).toBeNull();
  });

  it.each(['•', '·', '|', ''])('matches wrapped picker hints separated by "%s"', (separator) => {
    const footer = `  Type to\nfilter ${separator} Enter to select\n${separator} Tab to edit ${separator} Esc to close  `;
    expect(terminalTextInputMode(footer)).toBe('filter');
    expect(terminalTextInputMode(footer.replaceAll('\n', '\r\n'))).toBe('filter');
    expect(terminalTextInputMode('Type to filter\nEnter to select')).toBe('filter');
  });

  it.each([
    'Implemented the search field with placeholder "Type to filter".\nReady for the next request.',
    'Type to filter',
    'Type to filter the list, then press Enter to select an item.',
    'The picker says "Type to filter • Enter to select • Tab to edit".',
    'Type to filter\nUnrelated output\nEnter to select',
  ])('does not treat ordinary output as a picker: %s', (output) => {
    expect(terminalTextInputMode(output)).toBeNull();
  });

  it('keeps editor submission active when earlier output mentions filtering', () => {
    expect(terminalTextInputMode('Implemented "Type to filter".\nCustom answer:\n>\nenter or ctrl+q submit  esc cancel')).toBe('submit');
  });

  it('matches the picker footer with the filter box focused', () => {
    // The same footer once the user focuses the filter input; the hints do
    // not change, but the footer sits above a longer scrollback tail.
    const scrollback = 'Available models\n\n Filter: opus\n\n    Claude Opus 5            300K High\n\n 1-2 of 38\n\n Type to filter • Enter to select • Tab to edit';
    expect(terminalTextInputMode(scrollback)).toBe('filter');
  });

  it('does not match an idle pane or a model list without the footer', () => {
    expect(terminalTextInputMode('Manual Reboot DR [Grok 4.6 Medium]\nctx 24% used')).toBeNull();
    expect(terminalTextInputMode('Available models\n Filter:\n    Auto\n    Grok 4.7')).toBeNull();
  });
});

describe('detectTerminalMenu on the Cursor picker footer', () => {
  it('extracts Enter-select and Tab-edit as tappable actions', () => {
    const footer = 'Available models\n\n Type to filter • Enter to select • Tab to edit';
    const menu = detectTerminalMenu(footer);
    expect(menu).not.toBeNull();
    const labels = menu!.actions.map((action) => action.keys.join('+'));
    expect(labels).toContain('Enter');
    expect(labels).toContain('Tab');
  });

  it('extracts Esc-close when the filter has text', () => {
    // Same pane state once the filter input holds text: the last hint
    // switches from "Esc to close" to "Esc to clear".
    const footer = ' Filter: grok\n\n 1-2 of 38\n\n Type to filter • Enter to select • Tab to edit • Esc to clear';
    const menu = detectTerminalMenu(footer);
    expect(menu).not.toBeNull();
    expect(menu!.actions.map((action) => action.keys.join('+'))).toContain('Escape');
  });
});