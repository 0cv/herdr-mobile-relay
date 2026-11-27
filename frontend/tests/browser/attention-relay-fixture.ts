import { expect, type Page, type Browser } from '@playwright/test';
import { createConnection } from 'node:net';

const button = (page: Page, name: string) => page.getByRole('button', { name, exact: true });
export const settingsButton = (page: Page) => page.getByRole('button', { name: /^Settings(,|$)/ });

export function readerResultPane(project: string, repeat: number, retry: number): string {
  return `reader-result-${project}-${repeat}-${retry}`;
}

export async function control(command: Record<string, unknown>): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => {
    const socket = createConnection(process.env.HERDR_ATTENTION_CONTROL!);
    socket.setEncoding('utf8');
    socket.setTimeout(3_000, () => socket.destroy(new Error('Private fixture control timed out')));
    socket.once('connect', () => socket.write(JSON.stringify(command) + '\n'));
    socket.once('error', reject);
    let response = '';
    socket.on('data', chunk => {
      response += chunk;
      if (!response.includes('\n')) return;
      socket.end();
      try {
        const result = JSON.parse(response.slice(0, response.indexOf('\n')));
        if (!result.ok) throw new Error(result.error || 'Private fixture control failed');
        resolve(result.state);
      } catch (error) { reject(error); }
    });
  });
}

export async function withReader(page: Page, browser: Browser, run: (reader: Page) => Promise<void>) {
  await settingsButton(page).click();
  await button(page, 'Invite Device').click();
  const invite = page.getByRole('dialog', { name: 'Invite Device', exact: true });
  await invite.getByLabel('Device name', { exact: true }).fill('Private fixture reader');
  await invite.getByLabel('Role', { exact: true }).selectOption('reader');
  await invite.getByRole('button', { name: 'Create Invitation', exact: true }).click();
  const link = await page.getByLabel('One-use invitation link', { exact: true }).inputValue();
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' });
  try {
    await context.addInitScript(() => { Object.defineProperty(navigator, 'standalone', { value: true }); localStorage.setItem('herdr_default_agent_view', 'conversation'); });
    const reader = await context.newPage();
    await reader.goto(link);
    await expect(reader.getByRole('img', { name: '1/1 relays connected', exact: true })).toBeVisible();
    await run(reader);
  } finally { await context.close(); }
}
