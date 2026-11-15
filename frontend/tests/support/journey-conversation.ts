import { expect, type Page } from '@playwright/test';

export async function scrollConversationToBottom(page: Page) {
  const list = page.locator('.conversation-list');
  await expect.poll(async () => {
    await list.evaluate(async element => {
      element.scrollTop = element.scrollHeight;
      element.dispatchEvent(new Event('scroll'));
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    const range = (await list.locator('.conversation-source-status').allTextContents()).join('').match(/Showing \d+–(\d+) of (\d+)/);
    return (!range || range[1] === range[2])
      && await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight < 2);
  }, { timeout: 15_000, intervals: [16] }).toBe(true);
}

export async function showOlderConversationEntry(page: Page, id: string) {
  const list = page.locator('.conversation-list');
  const entry = list.locator(`[data-conversation-entry-id="${id}"]`);
  await expect.poll(async () => {
    if (await entry.count()) return true;
    const earlier = page.getByRole('button', { name: 'Show earlier loaded messages', exact: true });
    if (await earlier.count()) await earlier.evaluate(element => (element as HTMLElement).click());
    else {
      await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
      await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    }
    return Boolean(await entry.count());
  }, { timeout: 15_000, intervals: [16] }).toBe(true);
}
