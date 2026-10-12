import { describe, expect, it } from 'vitest';
import { terminalScreenColumns } from '$lib/terminal';

// Row shape measured from a real Claude pane read over the Herdr socket API:
// 23 physical rows, widest 162 columns, and no row narrow enough to fit a
// phone viewport. Every source ("visible", "recent", "recent-unwrapped")
// returned the same widths, so the pane, not the read, decides the geometry.
const WIDEST_PANE_COLUMNS = 162;

function paneRows(): { columns: number }[] {
  return [
    { columns: 54 },
    { columns: WIDEST_PANE_COLUMNS },
    { columns: 39 },
  ];
}

describe('terminalScreenColumns', () => {
  it('sizes the screen to the widest row so a controller pane scrolls sideways', () => {
    expect(terminalScreenColumns(paneRows())).toBe(WIDEST_PANE_COLUMNS);
  });

  it('reports no columns for an empty screen', () => {
    expect(terminalScreenColumns([])).toBe(0);
  });
});
