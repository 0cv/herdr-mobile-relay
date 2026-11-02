import { describe, expect, it } from 'vitest';
import { ansi256Color, ansiLineBackground, ansiLineBackgroundIndent, ansiToHtml, terminalHtmlRows, terminalResizeLayoutEngaged, terminalScreenColumns } from '$lib/terminal';

// Row shape measured from a real Claude pane read over the Herdr socket API:
// 23 physical rows, widest 162 columns, and no row narrow enough to fit a
// phone viewport. Every source ("visible", "recent", "recent-unwrapped")
// returned the same widths, so the pane, not the read, decides the geometry.
const WIDEST_PANE_COLUMNS = 162;

function paneRows(): { columns: number; fixedGrid: boolean }[] {
  return [
    { columns: 54, fixedGrid: false },
    { columns: WIDEST_PANE_COLUMNS, fixedGrid: false },
    { columns: 39, fixedGrid: false },
  ];
}

describe('sequential ANSI SGR', () => {
  it.each(['38;2;0;128;0', '38;5;0'])('retains bold and background around %s operands', (color) => {
    const html = ansiToHtml(`\x1b[1;44m\x1b[${color}mtext`);
    expect(html).toContain(`font-weight:700;background-color:${ansi256Color(4)};color:${color.includes(';2;') ? 'rgb(0,128,0)' : ansi256Color(0)}`);
    expect(ansiLineBackground(`\x1b[44m\x1b[${color}mtext`)).toBe(ansi256Color(4));
  });

  it.each(['48;2;0;128;0', '48;5;0'])('retains bold and foreground around %s operands', (color) => {
    expect(ansiToHtml(`\x1b[1;31m\x1b[${color}mtext`)).toContain(
      `font-weight:700;color:${ansi256Color(1)};background-color:${color.includes(';2;') ? 'rgb(0,128,0)' : ansi256Color(0)}`,
    );
    expect(ansiLineBackground(`\x1b[${color}mtext`)).toBe(color.includes(';2;') ? 'rgb(0,128,0)' : ansi256Color(0));
  });

  it('applies leading and trailing resets in order', () => {
    expect(ansiToHtml('\x1b[31;0mtext')).toBe('text');
    expect(ansiToHtml('\x1b[0;31mtext')).toBe(`<span style="color:${ansi256Color(1)}">text</span>`);
    expect(ansiLineBackground('\x1b[44;0mtext')).toBe('');
    expect(ansiLineBackground('\x1b[0;44mtext')).toBe(ansi256Color(4));
    expect(ansiLineBackground('\x1b[48;2;0;128;0;0mtext')).toBe('');
    expect(ansiLineBackground('\x1b[0;48;5;0mtext')).toBe(ansi256Color(0));
  });

  it('preserves partial extended-color handling', () => {
    expect(ansiToHtml('\x1b[38;2;31mtext')).toBe(`<span style="opacity:0.7;color:${ansi256Color(1)}">text</span>`);
    expect(ansiLineBackground('\x1b[48;2;44mtext')).toBe(ansi256Color(4));
  });

  it('preserves escaping, indentation and snapshot cell geometry', () => {
    const text = '  │<&>\t界  ';
    const styled = `  \x1b[1;44m\x1b[38;2;0;128;0m${text.slice(2)}`;
    expect(ansiLineBackgroundIndent(styled)).toBe(2);
    const [plain] = terminalHtmlRows(text, false, true);
    const [row] = terminalHtmlRows(styled, false, true);
    expect({ ...row, html: '' }).toEqual({ ...plain, html: '' });
    expect(row.columns).toBe(12);
    expect(row.fixedGrid).toBe(true);
    expect(row.html).toContain('&lt;&amp;&gt;');
    expect(row.html).toContain('font-weight:700');
    expect(row.html).toContain('transparent 0 2ch');
  });
});

describe('terminalResizeLayoutEngaged', () => {
  it('engages once the cell advance and the pane width are both known', () => {
    expect(terminalResizeLayoutEngaged(true, 7.2, 40)).toBe(true);
  });

  it('stays disengaged for a relay that cannot lease a pane size', () => {
    expect(terminalResizeLayoutEngaged(false, 7.2, 40)).toBe(false);
  });

  it('stays disengaged before the cell probe has been measured', () => {
    expect(terminalResizeLayoutEngaged(true, 0, 40)).toBe(false);
  });

  // The regression: a pane shown before its lease lands has a session but no
  // width. Wrapping then had no cap and broke every row mid-word at the
  // container width.
  it('stays disengaged while the pane has no leased width', () => {
    expect(terminalResizeLayoutEngaged(true, 7.2, 0)).toBe(false);
  });
});

describe('terminalScreenColumns', () => {
  it('sizes the screen to the widest row when wrapping is disengaged', () => {
    expect(terminalScreenColumns(paneRows(), false, 0)).toBe(WIDEST_PANE_COLUMNS);
  });

  it('keeps the widest row even when a stale cap is passed in', () => {
    expect(terminalScreenColumns(paneRows(), false, 40)).toBe(WIDEST_PANE_COLUMNS);
  });

  it('sizes the screen to the cap when wrapping is engaged', () => {
    expect(terminalScreenColumns(paneRows(), true, 40)).toBe(40);
  });

  it('lets a fixed-grid row exceed the cap, since such rows never wrap', () => {
    const rows = [...paneRows(), { columns: 96, fixedGrid: true }];
    expect(terminalScreenColumns(rows, true, 40)).toBe(96);
  });

  it('reports no columns for an empty screen', () => {
    expect(terminalScreenColumns([], false, 0)).toBe(0);
  });
});
