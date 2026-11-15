import { expect, test } from '@playwright/test';
import { openAgent, openOmpPane, server } from '../support/journey-relay';

test('virtualizes terminal history and copies the latest response', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedTerminal', text);
        },
      },
    });
  });
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Long history', agent: 'opencode' });
  const content = Array.from({ length: 1_000 }, (_, index) => {
    const row = `row ${String(index + 1).padStart(4, '0')}`;
    return index % 50 === 0 ? `${row} ${'wrapped content '.repeat(32).trimEnd()}` : row;
  }).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content,
  });

  const terminal = page.getByRole('log');
  const screen = terminal.locator('.term-screen');
  const mountedRows = screen.locator('[data-terminal-row]');
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1000');
  await expect(terminal).toContainText('row 1000');
  expect(await mountedRows.count()).toBeLessThan(250);

  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row'))).toBe(0);
  const topHeights = await mountedRows.evaluateAll((elements) => elements.slice(0, 2)
    .map((element) => element.getBoundingClientRect().height));
  expect(Math.abs(topHeights[0] - topHeights[1])).toBeLessThan(0.1);

  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row')))
    .toBeGreaterThan(100);
  expect(await mountedRows.count()).toBeLessThan(250);
  const middleAnchor = await terminal.evaluate((element) => {
    const viewportTop = element.getBoundingClientRect().top;
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > viewportTop);
    if (!row) return null;
    return {
      index: Number(row.dataset.terminalRow),
      offset: row.getBoundingClientRect().top - viewportTop,
    };
  });
  expect(middleAnchor).not.toBeNull();
  const updatedContent = `${content}\nrow 1001 updated while scrolled`;
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: updatedContent,
  });
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1001');
  expect(await mountedRows.count()).toBeLessThan(250);
  await expect.poll(async () => terminal.evaluate((element, expected) => {
    const viewportTop = element.getBoundingClientRect().top;
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > viewportTop);
    return row ? Math.abs(Number(row.dataset.terminalRow) - expected) : Number.POSITIVE_INFINITY;
  }, middleAnchor?.index || 0)).toBeLessThanOrEqual(1);
  await expect.poll(async () => terminal.evaluate((element, expected) => {
    const viewport = element.getBoundingClientRect();
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => Number(candidate.dataset.terminalRow) === expected);
    if (!row) return false;
    const bounds = row.getBoundingClientRect();
    return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
  }, middleAnchor?.index || 0)).toBe(true);

  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.last().getAttribute('data-terminal-row'))).toBe(1000);
  const bottomUpdatedContent = `${updatedContent}\nrow 1002 appended at bottom`;
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: bottomUpdatedContent,
  });
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1002');
  await expect.poll(async () => Number(await mountedRows.last().getAttribute('data-terminal-row'))).toBe(1001);
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  const viewport = page.viewportSize();
  expect(viewport).not.toBeNull();
  await page.setViewportSize({ width: viewport!.width, height: viewport!.height - 180 });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await terminal.evaluate((element) => {
    const view = element.closest<HTMLElement>('.terminal-view');
    if (!view) throw new Error('Terminal view is missing');
    view.style.height = `${view.clientHeight - 120}px`;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1002');
  expect(await mountedRows.count()).toBeLessThan(250);
  const finalResponseContent = [
    bottomUpdatedContent,
    '',
    '     + Thought: 1.0s',
    '     Final response line one.',
    '     Second line.',
    '     ▣ Build · model · 2m 19s',
    '     Next question',
  ].join('\n');
  const finalResponse = 'Final response line one.\nSecond line.';
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: finalResponseContent,
  });
  await expect(screen).toContainText('Final response line one.');
  const fullTranscript = page.getByRole('textbox', { name: 'Full terminal transcript' });
  const responseTranscript = page.getByRole('textbox', { name: 'Latest final response' });
  const copyButton = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(fullTranscript).toHaveValue(/Final response line one\./);
  await expect(responseTranscript).toHaveValue(finalResponse);
  await copyButton.click();
  await expect.poll(() => page.evaluate(() =>
    Reflect.get(window, '__copiedTerminal'))).toBe(finalResponse);
  await expect(page.getByRole('status').filter({ hasText: 'Final response copied.' })).toBeVisible();
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
  });
  await copyButton.click();
  await expect(responseTranscript).toBeFocused();
  expect(await responseTranscript.evaluate((element) => {
    if (!(element instanceof HTMLTextAreaElement)) return null;
    return { start: element.selectionStart, end: element.selectionEnd };
  })).toEqual({ start: 0, end: finalResponse.length });
  await expect(page.getByRole('status').filter({ hasText: 'Final response selected.' })).toBeVisible();
});

test('keeps rapid streaming frames pinned to the latest terminal row', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Streaming', agent: 'omp' });
  const initial = Array.from({ length: 200 }, (_, index) => `stream history ${index + 1}`).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: initial,
  });

  const terminal = page.getByRole('log');
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  let maxBottomDistance = 0;
  for (let frame = 1; frame <= 24; frame += 1) {
    const content = Array.from(
      { length: 200 },
      (_, index) => `stream history ${frame + index + 1}`,
    );
    content.push(`streaming response chunk ${frame}`);
    await server(page, 0, {
      type: 'pane_content',
      pane_id: 'w1:p1',
      format: 'plain',
      content: content.join('\n'),
    });
    await page.waitForTimeout(8);
    maxBottomDistance = Math.max(
      maxBottomDistance,
      await terminal.evaluate((element) =>
        element.scrollHeight - element.scrollTop - element.clientHeight),
    );
  }

  expect(maxBottomDistance).toBeLessThan(48);
  await expect(terminal).toContainText('streaming response chunk 24');
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
});

test('virtualizes large ANSI grids on a controller pane', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'ANSI grid history', agent: 'omp' });
  const content = Array.from({ length: 600 }, (_, index) => {
    if (index % 20 === 0) return `╭${'─'.repeat(98)}╮`;
    if (index % 20 === 1) {
      return `\u001b[48;2;61;64;64m${`│ grid row ${index + 1}`.padEnd(99)}│\u001b[0m`;
    }
    return `\u001b[38;2;95;175;255mANSI agent row ${index + 1} 🐑\u001b[0m`;
  }).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content,
  });

  const terminal = page.getByRole('log');
  const screen = terminal.locator('.term-screen');
  const mountedRows = screen.locator('[data-terminal-row]');
  await expect(screen).toHaveAttribute('data-terminal-row-count', '600');
  await expect.poll(async () => Number(await mountedRows.last().getAttribute('data-terminal-row'))).toBe(599);
  expect(await mountedRows.count()).toBeLessThan(250);
  expect(await terminal.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);

  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row'))).toBe(0);
  const gridGeometry = await terminal.locator('.terminal-grid-line').first().evaluate((element) => ({
    height: element.getBoundingClientRect().height,
    lineHeight: Number.parseFloat(getComputedStyle(element).lineHeight),
  }));
  expect(gridGeometry.height).toBeLessThan(gridGeometry.lineHeight * 2);
  await expect(terminal.locator('.ansi-line-background').first()).toBeVisible();

  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row')))
    .toBeGreaterThan(100);
  expect(await mountedRows.count()).toBeLessThan(250);
});

test('renders the raw pane window as terminal history', async ({ page }) => {
  await openOmpPane(page, 'Raw window');
  const windowRow = (index: number) => `raw window row ${String(index).padStart(3, '0')}`;
  const windowRows = Array.from({ length: 120 }, (_, index) => windowRow(index + 1));
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: windowRows.join('\n'),
  });
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: windowRows.join('\n'),
    viewport_only: true,
    viewport_rows: 20,
  });

  const terminal = page.getByRole('log');
  // The raw window renders exactly as read: scrolled-back rows included.
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');
  await expect(terminal).toContainText(windowRow(120));

  // A window shorter than the requested history carries the truncation flag.
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: windowRows.concat('raw window row 121').join('\n'),
    viewport_only: true,
    viewport_rows: 20,
    truncated: true,
  });
  await expect(page.getByRole('status').filter({ hasText: 'Older terminal history is not shown' }))
    .toBeVisible();
});

test('keeps the reader anchored while streamed output grows and unsticks on scroll-up', async ({ page }) => {
  await page.clock.install();
  // Rows 401-700 are three-character rows: too short for anchor text
  // matching, so only correct index math can keep the reader anchored there.
  const historyRow = (index: number) => (index > 400 && index <= 700
    ? String(index)
    : `anchored history row ${String(index).padStart(4, '0')}`);
  const historyRows = Array.from({ length: 1_000 }, (_, index) => historyRow(index + 1));
  await openOmpPane(page, 'Anchored stream');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: historyRows[0],
  });
  await page.clock.runFor(300);

  const streamedRows = Array.from({ length: 100 }, (_, index) => `anchored burst row ${index + 1}`);
  const stream = historyRows.concat(streamedRows);
  // Herdr always serves the newest 1,000-row window: streamed output appends
  // at the bottom and crops the oldest rows off the front.
  const frame = (streamed: number) => server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: stream
      .slice(Math.max(0, historyRows.length + streamed - 1_000), historyRows.length + streamed)
      .join('\n'),
    viewport_only: true,
    viewport_rows: 46,
  });
  await frame(0);
  const terminal = page.getByRole('log');
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '1000');

  // Pinned at the bottom: appended output is shown immediately.
  await frame(54);
  await expect(terminal).toContainText('anchored burst row 54');
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  // Scrolling up leaves sticky mode.
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  expect(await terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  const firstVisibleRow = () => terminal.evaluate((element) => {
    const top = element.getBoundingClientRect().top;
    const row = [...element.querySelectorAll('[data-terminal-row]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > top + 1);
    return {
      text: row?.textContent || '',
      offset: row ? Math.round(row.getBoundingClientRect().top - top) : 0,
    };
  });
  // The virtual window re-renders on the next animation frame.
  await expect.poll(async () => (await firstVisibleRow()).offset).toBeLessThanOrEqual(1);
  const before = await firstVisibleRow();
  expect(before.text.trim()).toMatch(/^\d{3}$/);

  // More output arrives and the served window crops rows from the front:
  // the reader's position must not move and sticky mode must stay off.
  await frame(100);
  await expect.poll(async () => (await firstVisibleRow()).text).toBe(before.text);
  const after = await firstVisibleRow();
  // Same row, and within a row of the same position: cropped rows shift every
  // index, so only exact index math keeps this stable.
  const rowHeight = await terminal.evaluate((element) => {
    const row = element.querySelector<HTMLElement>('[data-terminal-row]');
    return row ? row.getBoundingClientRect().height : 20;
  });
  expect(Math.abs(after.offset - before.offset)).toBeLessThan(rowHeight);
  expect(await terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);

  // Returning to the bottom re-engages sticky mode.
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
});

test('keeps a small scroll-up from the bottom while output streams', async ({ page }) => {
  await page.clock.install();
  const rows = Array.from({ length: 220 }, (_, index) => `near-bottom row ${index + 1}`);
  await openOmpPane(page, 'Near bottom');
  const frame = (count: number) => server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: rows.slice(0, count).join('\n'),
  });
  await frame(150);
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('near-bottom row 150');
  const bottomDistance = () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight);
  await expect.poll(bottomDistance).toBeLessThan(2);

  // A finger drags the view up by less than the 48px "near the bottom" band.
  await terminal.evaluate((element) => {
    element.dispatchEvent(new Event('touchmove', { bubbles: true }));
    element.scrollTop -= 20;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  await frame(151);
  await expect(terminal).toContainText('near-bottom row 151');
  expect(await bottomDistance()).toBeGreaterThan(10);

  // Long after the scroll, an agent keeps rewriting its last line.
  await page.clock.runFor(1_600);
  const readingTop = await terminal.evaluate((element) => element.scrollTop);
  for (let tick = 1; tick <= 5; tick += 1) {
    await server(page, 0, {
      type: 'pane_content',
      pane_id: 'w1:p1',
      format: 'ansi',
      content: rows.slice(0, 150).concat(`working ${tick}`).join('\n'),
    });
    await expect(terminal).toContainText(`working ${tick}`);
    await page.clock.runFor(200);
  }
  expect(Math.abs(await terminal.evaluate((element) => element.scrollTop) - readingTop))
    .toBeLessThanOrEqual(1);
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  // Scrolling partway back down is still reading, not following.
  await terminal.evaluate((element) => {
    element.scrollTop += 5;
    element.dispatchEvent(new Event('scroll'));
  });
  await frame(152);
  await expect(terminal).toContainText('near-bottom row 152');
  expect(await bottomDistance()).toBeGreaterThan(10);

  await terminal.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
  await frame(200);
  await expect(terminal).toContainText('near-bottom row 200');
  await expect.poll(bottomDistance).toBeLessThan(2);

  await terminal.evaluate((element) => {
    element.scrollTop -= 20;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  // Reaching the bottom follows again, even when output lands before the scroll event.
  await terminal.evaluate((element, message) => {
    element.scrollTop = element.scrollHeight;
    window.__relayServer(0, message);
  }, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: rows.slice(0, 203).join('\n') });
  await expect(terminal).toContainText('near-bottom row 203');
  await frame(220);
  await expect(terminal).toContainText('near-bottom row 220');
  await expect.poll(bottomDistance).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeHidden();
});

test('renders rows after a long wheel scroll to just above the bottom', async ({ page }) => {
  await page.clock.install();
  const rows = Array.from({ length: 1_000 }, (_, index) => `wheel row ${index + 1}`);
  await openOmpPane(page, 'Wheel jump');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: rows.join('\n'),
  });
  const terminal = page.getByRole('log');
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '1000');
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
  });
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
  await page.clock.runFor(300);

  // One big wheel scroll lands 20px above the bottom.
  await terminal.evaluate((element) => {
    element.dispatchEvent(new WheelEvent('wheel', { bubbles: true, deltaY: 10_000 }));
    element.scrollTop = element.scrollHeight - element.clientHeight - 20;
  });
  const visibleRow = () => terminal.evaluate((element) => {
    const viewport = element.getBoundingClientRect();
    return [...element.querySelectorAll('[data-terminal-row]')].some((row) => {
      const bounds = row.getBoundingClientRect();
      return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
    });
  });
  await expect.poll(visibleRow).toBe(true);
  // Being near the bottom is not a request to follow output.
  expect(await terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(10);
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
});
