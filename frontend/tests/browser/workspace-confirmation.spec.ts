import { expect, test } from '@playwright/test';

test('workspace confirmations focus Cancel, including force escalation', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_relays', JSON.stringify([
      { id: 'test', label: 'Test', url: 'ws://127.0.0.1:8871', token: '' },
    ]));
    class MockSocket {
      static OPEN = 1;
      static CONNECTING = 0;
      static CLOSED = 3;
      readyState = 0;
      onopen: (() => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      constructor() {
        queueMicrotask(() => {
          this.readyState = 1;
          this.onopen?.();
          this.server({ type: 'push_config', protocol: 3, host: 'test', capabilities: ['workspace_management', 'worktree_management', 'directory_browser'] });
          this.server({ type: 'workspaces', workspaces: [false, true].map((linked, i) => ({
            workspace_id: `w${i + 1}`, number: i + 1, label: linked ? 'Fix' : 'Project', pane_count: 1, tab_count: 1,
            cwd: `/work/${i}`, worktree: { repo_key: 'repo', repo_name: 'project', repo_root: '/work/0', checkout_path: `/work/${i}`, is_linked_worktree: linked },
          })) });
          this.server({ type: 'agents', agents: [] });
        });
      }
      server(message: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) })); }
      send(payload: string) {
        const command = JSON.parse(payload);
        if (command.type === 'workspace_close' || command.type === 'worktree_remove') {
          document.body.dataset.operations = String(Number(document.body.dataset.operations || 0) + 1);
          queueMicrotask(() => this.server({ type: 'command_result', request_id: command.request_id, action: command.type, ok: false, phase: 'failed', error: 'Refused', data: { force_available: true, code: 'workspace_group_close_required' } }));
        }
      }
      close() { this.readyState = 3; }
    }
    Object.defineProperty(window, 'WebSocket', { value: MockSocket });
  });
  await page.goto('/');
  await page.getByRole('button', { name: 'Manage workspaces' }).click();
  const dialog = page.locator('#workspace-destructive-dialog');
  const cancel = dialog.getByRole('button', { name: 'Cancel', exact: true });
  for (const name of ['Close', 'Remove Worktree']) {
    await page.getByRole('button', { name, exact: true }).first().click();
    await expect(cancel).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(dialog).toBeHidden();
    await expect(page.locator('body')).not.toHaveAttribute('data-operations');
  }
  await page.getByRole('button', { name: 'Remove Worktree', exact: true }).click();
  await expect(cancel).toBeFocused();
  await dialog.getByRole('button', { name: 'Remove Worktree', exact: true }).click();
  await expect(dialog.getByRole('button', { name: 'Force Remove', exact: true })).toBeEnabled();
  await expect(cancel).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(dialog).toBeHidden();
  await expect(page.locator('body')).toHaveAttribute('data-operations', '1');
  await page.getByRole('button', { name: 'Close', exact: true }).first().click();
  await dialog.getByRole('button', { name: 'Close Workspace', exact: true }).click();
  await expect(dialog.getByRole('button', { name: 'Close Workspace Group' })).toBeEnabled();
  await expect(cancel).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(dialog).toBeHidden();
  await expect(page.locator('body')).toHaveAttribute('data-operations', '2');
});
