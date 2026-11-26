/**
 * @param {import('@playwright/test').Page} page
 * @param {string} agentName
 */
export function waitForLoadedConversation(page, agentName) {
  return page.getByRole('region', { name: `Conversation with ${agentName}`, exact: true })
    .and(page.locator('[aria-busy="false"]')).waitFor({ timeout: 10_000 });
}
