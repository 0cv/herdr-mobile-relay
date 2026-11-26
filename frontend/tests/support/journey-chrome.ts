import { expect, type Page } from '@playwright/test';

export function moreTrigger(page: Page) {
  return page.getByRole('banner').getByRole('button', { name: /^More\b/ });
}

export async function openMore(page: Page) {
  await moreTrigger(page).click();
  return page.locator('#app-more');
}

export async function chooseMore(page: Page, name: string | RegExp) {
  const dialog = await openMore(page);
  await dialog.getByRole('button', { name, exact: typeof name === 'string' }).click();
}

export async function openConversationSearch(page: Page) {
  await chooseMore(page, 'Search loaded conversation');
  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await expect(search).toBeFocused();
  return search;
}
