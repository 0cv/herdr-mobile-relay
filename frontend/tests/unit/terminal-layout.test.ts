import { describe, expect, it } from 'vitest';
import { terminalResizeLayoutEngaged, terminalScreenColumns } from '$lib/terminal';

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

  // The regression: the relay advertises the lease capability unconditionally,
  // so a pane shown before its lease lands has a session but no width. Wrapping
  // then had no cap and broke every row mid-word at the container width.
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
